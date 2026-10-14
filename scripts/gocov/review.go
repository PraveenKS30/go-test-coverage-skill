package main

// Adversarial review step. After tests are written, a reviewer with a FRESH
// context (it sees only the diff and the criteria, not the reasoning that
// produced the tests) tries to find tests that would not catch real
// regressions. gocov prepares the review packet deterministically; the
// reviewer is either a subagent of the current session or one headless
// `claude -p` call. Findings land in REVIEW.md and are summarised in
// FINAL_REPORT.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func reviewDir(o *opts, name string) string  { return filepath.Join(repoDir(o, name), "review") }
func reviewFile(o *opts, name string) string { return filepath.Join(repoDir(o, name), "REVIEW.md") }

func reviewTestFiles(st *RepoState) []string {
	set := map[string]bool{}
	for _, it := range st.Items {
		if it.Status != "done" {
			continue
		}
		for _, f := range it.TestFiles {
			set[f] = true
		}
	}
	var out []string
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func buildReviewPrompt(o *opts, rc RepoCfg, st *RepoState) string {
	var sb strings.Builder
	sb.WriteString("You are an adversarial reviewer of newly written Go unit tests. You have NOT seen how they were written; judge only what is on disk. Your job is to find tests that would NOT catch a real regression, not to polish them.\n\n")
	fmt.Fprintf(&sb, "Repo: %s (%s)\nGoal: %.0f%% coverage (%s metric). Current estimate: %.2f%%.\nComplexity limit per function: %d (%s).\n\n", st.Name, rc.Path, st.Goal, st.GoalBasis, st.goalTotals().Pct(), o.cfg.MaxComplexity, o.cfg.ComplexityMetric)

	sb.WriteString("## What to review\n\n| Package | Test files | Stmts gained | Attempts |\n|---|---|---:|---:|\n")
	for _, it := range st.Items {
		if it.Status != "done" {
			continue
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %d | %d |\n", it.Pkg, strings.Join(it.TestFiles, ", "), it.Gained, it.Attempts)
	}
	sb.WriteString("\nThe test files may be uncommitted: read them directly from disk, and use `git status` / `git diff` for context. Read the production code each test exercises.\n\n")

	sb.WriteString("## Report a finding ONLY if it affects correctness or the stated requirements\n\n")
	sb.WriteString(`1. WEAK: a test that would still pass if the behavior it claims to protect were broken. Name the concrete mutation (file, line, change) that would survive, e.g. "flip 409 to 500 in writeError" or "drop the stock check in AdjustStock". Check that the assertion would fail under it.
2. JUNK: assertion-free tests, tests that only call a function, self-fulfilling mocks, trivial getter/setter tests, permanent t.Skip, cosmetic duplicate table rows (see the junk-patterns reference).
3. UNPROTECTED: behavior the package's public contract clearly has (error paths, boundaries, state changes, ordering, empty-slice-vs-null) that no test asserts, in the files above.
4. NONDETERMINISTIC: sleeps, wall-clock, map-iteration order, shared global state, network, ordering assumptions, goroutine races.
5. LOCKS-IN-A-BUG: a test that asserts behavior that looks like a production defect. Describe the defect; do not recommend changing the test to hide it.
6. SCOPE: any change outside *_test.go / testdata/ (production code, go.mod, go.sum).
7. COMPLEXITY: a function over the limit.

Do NOT report: style, naming, comment wording, missing tests for trivial code, "could also test X" ideas without a concrete regression, or theoretical cases that cannot occur. A reviewer asked to find gaps will find some even when the work is sound - report none rather than padding. Prefer 3 real findings over 15 weak ones.

`)
	sb.WriteString("## How you may work\n\nYou may read files and run `go test` / `go vet` / `git diff` / `git status`. Do NOT edit any file (not even temporarily): propose mutations in words and judge them by reading the assertions. If you can verify a claim by running the tests, do.\n\n")

	sb.WriteString("## Output format (exact)\n\nStart with one line: `Verdict: PASS` (no blocking findings) or `Verdict: CHANGES NEEDED`.\nThen one section per finding:\n\n```\n### BLOCKING - <WEAK|JUNK|UNPROTECTED|NONDETERMINISTIC|LOCKS-IN-A-BUG|SCOPE|COMPLEXITY> - <path/to/file_test.go>:<line>\n<what is wrong, and the concrete regression or mutation that would go unnoticed>\nFix: <one-sentence concrete change>\n```\n\nUse `### OPTIONAL - ...` (same shape) for real but non-essential improvements. Anything that does not affect correctness or the requirements is not a finding at all.\n")

	if dir := skillRefsDir(); dir != "" {
		fmt.Fprintf(&sb, "\nReference: %s\n", filepath.Join(dir, "junk-patterns.md"))
	}
	return sb.String()
}

var (
	blockingRe = regexp.MustCompile(`(?m)^#{2,4}\s*BLOCKING\b`)
	optionalRe = regexp.MustCompile(`(?m)^#{2,4}\s*OPTIONAL\b`)
	verdictRe  = regexp.MustCompile(`(?mi)^\s*Verdict:\s*(PASS|CHANGES NEEDED)`)
)

type reviewSummary struct {
	Exists             bool
	Blocking, Optional int
	Verdict            string
}

func readReview(o *opts, name string) reviewSummary {
	data, err := os.ReadFile(reviewFile(o, name))
	if err != nil {
		return reviewSummary{}
	}
	s := string(data)
	r := reviewSummary{Exists: true, Blocking: len(blockingRe.FindAllString(s, -1)), Optional: len(optionalRe.FindAllString(s, -1))}
	if m := verdictRe.FindStringSubmatch(s); m != nil {
		r.Verdict = strings.ToUpper(m[1])
	}
	return r
}

func (r reviewSummary) line() string {
	if !r.Exists {
		return "not run (see `gocov review`)"
	}
	return fmt.Sprintf("verdict %s: %d blocking, %d optional finding(s) - REVIEW.md", orDash(r.Verdict), r.Blocking, r.Optional)
}

func orDash(s string) string {
	if s == "" {
		return "UNKNOWN"
	}
	return s
}

func cmdReview(ctx context.Context, o *opts, headless bool) error {
	for _, rc := range o.repos() {
		st, err := loadState(o, rc.Name)
		if err != nil {
			return err
		}
		if len(reviewTestFiles(st)) == 0 {
			return fmt.Errorf("%s: nothing to review yet - no item is `done` with recorded test files", rc.Name)
		}
		prompt := buildReviewPrompt(o, rc, st)
		if err := os.MkdirAll(reviewDir(o, rc.Name), 0o755); err != nil {
			return err
		}
		pp := filepath.Join(reviewDir(o, rc.Name), "PROMPT.md")
		if err := os.WriteFile(pp, []byte(prompt), 0o644); err != nil {
			return err
		}
		if !headless {
			fmt.Printf("== %s: review packet written to %s\n", rc.Name, pp)
			fmt.Printf("   Dispatch a FRESH-context subagent with that prompt (read-only). Save its answer to:\n   %s\n   Then triage: fix every BLOCKING finding (re-run `gocov finish`), skip OPTIONAL ones unless cheap.\n", reviewFile(o, rc.Name))
			continue
		}
		if err := runHeadlessReview(ctx, o, rc, st, prompt); err != nil {
			return err
		}
	}
	return nil
}

func runHeadlessReview(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, prompt string) error {
	if err := authProbe(ctx); err != nil {
		return err
	}
	usd := min3(o.cfg.Budget.MaxUSDPerRepo-st.SpendUSD, o.cfg.Budget.MaxUSDTotal-st.SpendUSD, o.cfg.Budget.MaxUSDPerItem*2)
	if usd < 0.05 {
		return fmt.Errorf("%s: no budget left for the review step", rc.Name)
	}
	cctx, cancel := context.WithTimeout(ctx, o.cfg.itemTimeout())
	defer cancel()
	args := []string{
		"-p", "--output-format", "json",
		"--model", o.cfg.Models.Escalate, // judgement task: stronger model
		"--permission-mode", "dontAsk",
		"--max-budget-usd", fmt.Sprintf("%.2f", usd),
		"--no-session-persistence",
		"--allowedTools", "Read,Glob,Grep,Bash(go test *),Bash(go vet *),Bash(git diff *),Bash(git status *),Bash(git log *)",
	}
	cmd := exec.CommandContext(cctx, "claude", args...)
	cmd.Dir = rc.Path
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = waitDelay
	_ = cmd.Run()
	if killedAtTimeout(cctx) {
		st.SpendUSD += usd
		if err := saveState(o, st); err != nil {
			return err
		}
		return fmt.Errorf("%s: headless review killed at the %v item_timeout_min; charged its cap $%.2f (a killed call reports no cost)", rc.Name, o.cfg.itemTimeout(), usd)
	}
	var res claudeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.IsError {
		return fmt.Errorf("%s: headless review failed: %s %s", rc.Name, firstLine(res.Result), firstLine(errb.String()))
	}
	st.SpendUSD += res.TotalCostUSD
	if err := saveState(o, st); err != nil {
		return err
	}
	if err := os.WriteFile(reviewFile(o, rc.Name), []byte(res.Result+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("== %s: review %s ($%.2f)\n", rc.Name, readReview(o, rc.Name).line(), res.TotalCostUSD)
	return nil
}
