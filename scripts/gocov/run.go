package main

// `gocov run`: executes the approved plan. One fresh, tightly-scoped
// `claude -p` call per package. The script (not the model) measures, enforces
// budgets, reverts forbidden production edits, decides success / retry /
// blocked, and stops as soon as a fresh full run verifies the goal.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var outMu sync.Mutex

func say(format string, a ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Printf(format+"\n", a...)
}

type budget struct {
	mu       sync.Mutex
	spent    float64
	deadline time.Time
	cfg      *Config
}

func (b *budget) totalLeft() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.Budget.MaxUSDTotal - b.spent
}
func (b *budget) add(u float64) { b.mu.Lock(); b.spent += u; b.mu.Unlock() }
func (b *budget) timeUp() bool {
	return !b.deadline.IsZero() && time.Now().After(b.deadline)
}

func cmdRun(ctx context.Context, o *opts) error {
	if _, err := exec.LookPath("claude"); err != nil && !o.dryRun {
		return fmt.Errorf("`claude` CLI not found on PATH (needed for headless mode)")
	}
	if !o.dryRun {
		if err := authProbe(ctx); err != nil {
			return err
		}
	}
	b := &budget{cfg: o.cfg}
	if o.cfg.Budget.TimeLimitMin > 0 {
		b.deadline = time.Now().Add(time.Duration(o.cfg.Budget.TimeLimitMin) * time.Minute)
	}
	sem := make(chan struct{}, o.cfg.Jobs)
	var wg sync.WaitGroup
	errs := make(chan error, len(o.cfg.Repos))
	for _, rc := range o.repos() {
		wg.Add(1)
		sem <- struct{}{}
		go func(rc RepoCfg) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := runRepo(ctx, o, rc, b); err != nil {
				errs <- fmt.Errorf("%s: %w", rc.Name, err)
			}
		}(rc)
	}
	wg.Wait()
	close(errs)
	var all []string
	for e := range errs {
		all = append(all, e.Error())
	}
	if o.dryRun {
		return nil
	}
	_ = writeSummary(o)
	if len(all) > 0 {
		return fmt.Errorf("%s", strings.Join(all, "; "))
	}
	say("\nRun finished. Spend this run: $%.2f of $%.2f. See %s", b.spent, o.cfg.Budget.MaxUSDTotal, filepath.Join(o.cfg.OutDir, "SUMMARY.md"))
	return nil
}

func runRepo(ctx context.Context, o *opts, rc RepoCfg, b *budget) error {
	st, err := loadState(o, rc.Name)
	if err != nil {
		return err
	}
	if st.Status == "baselined" || len(st.Items) == 0 && st.Status != "achieved" {
		return fmt.Errorf("no approved plan - run `gocov plan` and review PLAN.md first")
	}
	if st.Status == "achieved" {
		say("[%s] already achieved (%.2f%%) - nothing to do", rc.Name, st.goalTotals().Pct())
		return nil
	}
	git := isGit(rc.Path)
	if !git && !rc.AllowNonGit && !o.dryRun {
		return fmt.Errorf("%s is not a git repo; refusing to let an agent edit it without a way to revert (set allow_non_git:true to override)", rc.Path)
	}
	if git && !o.dryRun && st.Status == "planned" {
		if dirty := dirtyFiles(rc.Path, o.cfg.OutDir); len(dirty) > 0 {
			return fmt.Errorf("working tree has uncommitted changes (%d files, e.g. %s); commit or stash first so generated work is cleanly separable", len(dirty), dirty[0])
		}
		if rc.Branch != "" {
			if err := gitCheckoutBranch(rc.Path, rc.Branch); err != nil {
				return err
			}
		}
	}

	processed := 0
	for i := range st.Items {
		it := &st.Items[i]
		if it.Status != "queued" {
			continue
		}
		if o.pilot > 0 && processed >= o.pilot {
			st.Status = "in-progress"
			st.Reason = fmt.Sprintf("pilot of %d item(s) finished; re-run without -pilot to continue", o.pilot)
			break
		}
		if b.timeUp() {
			st.Status, st.Reason = "paused-budget", "time limit reached; re-run to resume"
			break
		}
		left := min3(o.cfg.Budget.MaxUSDPerRepo-st.SpendUSD, b.totalLeft(), o.cfg.Budget.MaxUSDPerItem)
		if left < 0.05 {
			st.Status, st.Reason = "paused-budget", fmt.Sprintf("budget cap reached (next call allowed $%.2f; per-item cap $%.2f, repo spend $%.2f/$%.2f, run spend $%.2f/$%.2f); raise the cap in the config and re-run to resume", left, o.cfg.Budget.MaxUSDPerItem, st.SpendUSD, o.cfg.Budget.MaxUSDPerRepo, b.spent, o.cfg.Budget.MaxUSDTotal)
			break
		}
		if o.dryRun {
			meas, err := measureItem(ctx, o, rc, st, it, "dry")
			if err != nil {
				return err
			}
			say("---- DRY RUN (%s, item %s) ----\ncommand: %s\n\n%s", rc.Name, it.ID, claudeArgsString(o, modelFor(o, it, 1), left), buildPrompt(o, rc, st, it, meas, ""))
			return nil
		}
		st.Status = "in-progress"
		st.Reason = ""
		processed++
		if ferr := runItem(ctx, o, rc, st, it, b, git); ferr != nil {
			st.Status, st.Reason = "paused-error", ferr.Error()
			_ = saveState(o, st)
			say("[%s] PAUSED: %s", rc.Name, st.Reason)
			return ferr
		}
		st.Verified = false
		_ = saveState(o, st)

		if st.goalTotals().Pct() >= st.Goal {
			say("[%s] estimate reached goal - verifying with a fresh full run...", rc.Name)
			if err := fullMeasure(ctx, o, st, rc, "verify"); err != nil {
				return err
			}
			if st.goalTotals().Pct() >= st.Goal {
				break
			}
			say("[%s] verified %.2f%% < goal %.0f%% - estimate was optimistic, continuing", rc.Name, st.goalTotals().Pct(), st.Goal)
		}
	}

	if st.Status == "in-progress" && st.Reason == "" || st.Status == "planned" {
		// queue exhausted or goal estimate hit: close out with a verified figure
		if err := fullMeasure(ctx, o, st, rc, "verify"); err != nil {
			return err
		}
		finalize(o, st)
	} else if st.Status == "paused-budget" || strings.HasPrefix(st.Reason, "pilot") {
		_ = fullMeasureQuiet(ctx, o, st, rc)
	}
	st.log("run:" + st.Status)
	if err := saveState(o, st); err != nil {
		return err
	}
	_ = writeFinal(o, st)
	say("[%s] %s - %.2f%% (goal %.0f%%) %s", rc.Name, strings.ToUpper(st.Status), st.goalTotals().Pct(), st.Goal, st.Reason)
	if o.pilot > 0 {
		pilotStats(rc.Name, st)
	}
	return nil
}

func fullMeasureQuiet(ctx context.Context, o *opts, st *RepoState, rc RepoCfg) error {
	reason := st.Reason
	err := fullMeasure(ctx, o, st, rc, "checkpoint")
	st.Reason = reason
	return err
}

func pilotStats(name string, st *RepoState) {
	var cost float64
	var gained, n int
	for _, it := range st.Items {
		if it.Status == "done" || it.Status == "blocked" {
			cost += it.CostUSD
			gained += it.Gained
			n++
		}
	}
	if n == 0 || cost == 0 {
		return
	}
	say("[%s] PILOT: %d item(s), $%.2f total = $%.2f/item, %.1f stmts gained per $1. Set est_usd_per_item=%.2f in the config.", name, n, cost, cost/float64(n), float64(gained)/cost, cost/float64(n))
}

func min3(a, b, c float64) float64 {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

func modelFor(o *opts, it *Item, attempt int) string {
	if attempt > 1 || it.Hard || it.Uncovered > 300 {
		return o.cfg.Models.Escalate
	}
	return o.cfg.Models.Default
}

func findModule(st *RepoState, dir string) Module {
	for _, m := range st.Modules {
		if m.Dir == dir {
			return m
		}
	}
	return Module{Dir: dir}
}

// pkgRelToModule converts a repo-relative package dir into a module-relative one.
func pkgRelToModule(modDir, pkg string) string {
	if modDir == "." || modDir == "" {
		return pkg
	}
	return strings.TrimPrefix(strings.TrimPrefix(pkg, modDir), "/")
}

func measureItem(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, tag string) (*Measurement, error) {
	mod := findModule(st, it.Module)
	rel := pkgRelToModule(it.Module, it.Pkg)
	if rel == "" || rel == "." {
		rel = "."
	}
	flags := append([]string(nil), rc.TestFlags...)
	if o.cfg.Race && tag == "post" {
		flags = append(flags, "-race")
	}
	return measure(ctx, rc.Path, mod, rel, flags, filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-"+tag+".log"))
}

func runItem(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, b *budget, git bool) error {
	before, err := measureItem(ctx, o, rc, st, it, "pre")
	if err != nil {
		it.Status, it.Reason = "blocked", "package does not build/test at baseline: "+err.Error()
		say("[%s] BLOCKED %s: %s", rc.Name, it.ID, it.Reason)
		return nil
	}
	preCov := coverOf(before.Rows, it.Files)
	it.PreCovered = preCov
	recordPreComplexity(o, rc, it)
	it.PkgBefore = pctOf(before.Rows, it.Files)
	snap := snapshot(rc.Path, o.cfg.OutDir)
	failNote := ""

	for attempt := 1; attempt <= o.cfg.MaxAttempts; attempt++ {
		left := min3(o.cfg.Budget.MaxUSDPerRepo-st.SpendUSD, b.totalLeft(), o.cfg.Budget.MaxUSDPerItem)
		if left < 0.05 {
			it.Reason = "stopped: budget exhausted mid-item"
			break
		}
		it.Attempts = attempt
		model := modelFor(o, it, attempt)
		say("[%s] %s attempt %d/%d (model %s, cap $%.2f)", rc.Name, it.ID, attempt, o.cfg.MaxAttempts, model, left)
		cost, cerr := callClaude(ctx, o, rc, st, it, before, model, left, failNote)
		it.CostUSD += cost
		st.SpendUSD += cost
		b.add(cost)
		if fe, ok := cerr.(*fatalErr); ok {
			it.Attempts--
			revertAll(rc.Path, snap, o.cfg.OutDir)
			return fe
		}
		if cerr != nil {
			failNote = "the previous attempt failed to complete: " + cerr.Error()
		}

		if ok, fn := verifyItem(ctx, o, rc, st, it, preCov, snap, git); ok {
			say("[%s]   item $%.2f, repo $%.2f/%.0f, run $%.2f/%.0f", rc.Name, it.CostUSD, st.SpendUSD, o.cfg.Budget.MaxUSDPerRepo, b.spent, o.cfg.Budget.MaxUSDTotal)
			return nil
		} else {
			failNote = fn
		}
	}
	// failed: remove whatever this item left behind so the tree stays green
	if git {
		revertAll(rc.Path, snap, o.cfg.OutDir)
	}
	it.Status = "blocked"
	if it.Reason == "" {
		it.Reason = "no verified coverage gain after " + fmt.Sprint(o.cfg.MaxAttempts) + " attempts: " + firstLine(failNote)
	}
	say("[%s] BLOCKED %s: %s", rc.Name, it.ID, it.Reason)
	st.log("blocked:" + it.ID)
	return nil
}

// verifyItem applies the guard rails and acceptance tests to whatever the
// author (headless claude, or the in-session agent) left in the tree. On
// success it records the item as done. On failure it returns feedback for the
// next attempt and leaves the tree as is (the caller decides to retry/revert).
func verifyItem(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, preCov int, snap map[string]string, git bool) (bool, string) {
	if git {
		if bad := revertForbidden(rc.Path, snap, o.cfg.OutDir, o.cfg.AllowDeps); len(bad) > 0 {
			say("[%s]   reverted forbidden edits: %s", rc.Name, strings.Join(bad, ", "))
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s: non-test files were edited %v; reverted by gocov", it.ID, bad))
		}
	}
	after, merr := measureItem(ctx, o, rc, st, it, "post")
	if merr != nil || after.TestExit != 0 {
		return false, "go test failed after your changes. Fix the tests (do not weaken them). Tail of log:\n" + tailFile(logOf(after, merr), 40)
	}
	mod := findModule(st, it.Module)
	rel := "./" + pkgRelToModule(it.Module, it.Pkg)
	fargs := append([]string{"-count=3"}, rc.TestFlags...)
	flog := filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-flake.log")
	if code := goTest(ctx, filepath.Join(rc.Path, filepath.FromSlash(mod.Dir)), append(fargs, rel), flog); code != 0 {
		return false, "tests are FLAKY or fail on repeat (-count=3). Remove time/ordering/shared-state dependence. Tail:\n" + tailFile(flog, 30)
	}
	if lim := o.cfg.MaxComplexity; lim > 0 && git {
		newTests := changedTestFiles(rc.Path, snap, o.cfg.OutDir)
		pre := map[string]bool{}
		for _, k := range it.PreOver {
			pre[k] = true
		}
		var over []FuncComplexity
		for _, f := range complexityOver(complexityOf(rc.Path, newTests), lim, o.cfg.ComplexityMetric) {
			if !pre[f.File+":"+f.Name] {
				over = append(over, f)
			}
		}
		if len(over) > 0 {
			return false, testComplexityFeedback(over, lim, o.cfg.ComplexityMetric)
		}
	}
	gain := coverOf(after.Rows, it.Files) - preCov
	if gain < 1 {
		return false, fmt.Sprintf("tests pass but covered 0 new statements in %v. Target the uncovered ranges listed.", it.Files)
	}
	it.Gained = gain
	it.PkgAfter = pctOf(after.Rows, it.Files)
	it.Status, it.Reason = "done", ""
	it.TestFiles = changedTestFiles(rc.Path, snap, o.cfg.OutDir)
	applyGain(st, it.Files, gain, after.Rows)
	if o.commit && git {
		gitCommit(rc.Path, it)
	}
	g := st.goalTotals()
	say("[%s] DONE %s pkg %.1f%% -> %.1f%% (+%d stmts) | repo %.2f%% -> goal %.0f%% (%.2f to go, estimate)",
		rc.Name, it.ID, it.PkgBefore, it.PkgAfter, gain, g.Pct(), st.Goal, st.Goal-g.Pct())
	st.log("done:" + it.ID)
	return true, ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func logOf(m *Measurement, err error) string {
	if m != nil {
		return m.Log
	}
	return ""
}

func coverOf(rows []FileCov, files []string) int {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	n := 0
	for _, r := range rows {
		if set[r.File] {
			n += r.Covered
		}
	}
	return n
}

func pctOf(rows []FileCov, files []string) float64 {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	var t Totals
	for _, r := range rows {
		if set[r.File] {
			t.Stmts += r.Stmts
			t.Covered += r.Covered
		}
	}
	return t.Pct()
}

// applyGain updates the live row estimates; a fresh full run replaces them.
func applyGain(st *RepoState, files []string, gain int, after []FileCov) {
	post := map[string]FileCov{}
	for _, r := range after {
		post[r.File] = r
	}
	left := gain
	for i := range st.Rows {
		p, ok := post[st.Rows[i].File]
		if !ok || left <= 0 {
			continue
		}
		inItem := false
		for _, f := range files {
			if f == st.Rows[i].File {
				inItem = true
			}
		}
		if !inItem {
			continue
		}
		room := st.Rows[i].Stmts - st.Rows[i].Covered
		add := p.Covered
		if add > room {
			add = room
		}
		if add > left {
			add = left
		}
		st.Rows[i].Covered += add
		left -= add
	}
}

// ------------------------------------------------------------- claude -p ----

func claudeArgs(o *opts, model string, usd float64) []string {
	return []string{
		"-p",
		"--output-format", "json",
		"--model", model,
		"--permission-mode", "dontAsk", // deny anything not pre-approved by --allowedTools (unattended)
		"--max-budget-usd", fmt.Sprintf("%.2f", usd),
		"--no-session-persistence",
		"--allowedTools", "Read,Edit,Write,Glob,Grep,Bash(go test *),Bash(go vet *),Bash(go build *),Bash(go list *),Bash(gofmt *),Bash(git diff *),Bash(git status *)",
	}
}

func claudeArgsString(o *opts, model string, usd float64) string {
	a := claudeArgs(o, model, usd)
	for i, s := range a {
		if strings.ContainsAny(s, " ()*") {
			a[i] = `"` + s + `"`
		}
	}
	return "claude " + strings.Join(a, " ") + "  < prompt"
}

// fatalErr marks problems retrying cannot fix (auth, API outage, bad flags).
type fatalErr struct{ msg string }

func (e *fatalErr) Error() string { return e.msg }

type claudeResult struct {
	TerminalReason string  `json:"terminal_reason"`
	Result         string  `json:"result"`
	IsError        bool    `json:"is_error"`
	Subtype        string  `json:"subtype"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	NumTurns       int     `json:"num_turns"`
}

func callClaude(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, before *Measurement, model string, usd float64, failNote string) (float64, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(o.cfg.Budget.ItemTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", claudeArgs(o, model, usd)...)
	cmd.Dir = filepath.Join(rc.Path, filepath.FromSlash(it.Module))
	cmd.Stdin = strings.NewReader(buildPrompt(o, rc, st, it, before, failNote))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	_ = os.WriteFile(filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-claude.json"), stdout.Bytes(), 0o644)
	var res claudeResult
	cost := 0.0
	if jerr := json.Unmarshal(stdout.Bytes(), &res); jerr == nil {
		cost = res.TotalCostUSD
		if res.IsError && (res.TerminalReason == "api_error" || cost == 0) {
			return cost, &fatalErr{fmt.Sprintf("claude -p could not run: %s (no model call was made; fix this, then re-run to resume)", firstLine(res.Result))}
		}
		if res.IsError || strings.HasPrefix(res.Subtype, "error") {
			return cost, fmt.Errorf("claude reported %s", res.Subtype)
		}
	} else if err != nil && cctx.Err() == nil {
		return cost, &fatalErr{fmt.Sprintf("claude -p failed to start: %v %s", err, firstLine(stderr.String()))}
	}
	if err != nil {
		return cost, fmt.Errorf("claude failed: %v %s", err, firstLine(stderr.String()))
	}
	return cost, nil
}

const rules = `RULES (non-negotiable):
- Write ONLY *_test.go files (and files under testdata/). Never edit production code, go.mod or go.sum. If a branch cannot be reached without changing production code, SKIP it and say so in your final message.
- Behavioral tests only: table-driven subtests, assert real outputs / state / errors. No assertion-free tests, no tests that only call a function, no self-fulfilling mocks, no trivial getter/setter tests, no permanent t.Skip.
- Deterministic: no sleeps, no wall-clock, no network, no shared global state; use t.TempDir(), injected clocks, small consumer-side fakes.
- Match the package's existing test style (black-box vs white-box, helpers, testify only if already a dependency). Reuse existing fixtures.
- MUTATION CHECK for each new test function that guards branching logic: temporarily break the branch it protects, run the test and confirm it FAILS, then restore the original code EXACTLY and confirm it passes. Leave no mutation behind.
- Run the package tests until green. Keep the final message under 8 lines: files written, behaviors covered, anything skipped and why.`

func buildPrompt(o *opts, rc RepoCfg, st *RepoState, it *Item, before *Measurement, failNote string) string {
	var sb strings.Builder
	mod := findModule(st, it.Module)
	fmt.Fprintf(&sb, "Raise the unit-test coverage of ONE Go package with real, behavior-verifying tests.\n\n")
	fmt.Fprintf(&sb, "Package dir (repo-relative): %s\nModule: %s (go %s, testify in go.mod: %v)\nCurrent package coverage: %.1f%% (%d uncovered statements). Repo goal: %.0f%%.\n",
		it.Pkg, mod.Path, mod.GoVersion, mod.Testify, it.PkgBefore, it.Uncovered, st.Goal)
	if len(rc.TestFlags) > 0 {
		fmt.Fprintf(&sb, "Always run tests with these extra flags: %s\n", strings.Join(rc.TestFlags, " "))
	}
	if mod.GoVersion != "" && !goAtLeast122(mod.GoVersion) {
		sb.WriteString("go.mod is below 1.22: if you use t.Parallel in table loops, copy the loop variable (tc := tc).\n")
	}
	sb.WriteString("\nUNCOVERED line ranges per file (prioritize largest, most logic-heavy first; skip trivial ones):\n")
	files := append([]string(nil), it.Files...)
	sort.Slice(files, func(i, j int) bool { return len(before.Uncov[files[i]]) > len(before.Uncov[files[j]]) })
	for i, f := range files {
		if i >= 8 {
			fmt.Fprintf(&sb, "- ... %d more files in the package\n", len(files)-i)
			break
		}
		var rs []string
		for j, r := range before.Uncov[f] {
			if j >= 40 {
				rs = append(rs, "...")
				break
			}
			if r.Start == r.End {
				rs = append(rs, fmt.Sprint(r.Start))
			} else {
				rs = append(rs, fmt.Sprintf("%d-%d", r.Start, r.End))
			}
		}
		fmt.Fprintf(&sb, "- %s: %s\n", path.Base(f), strings.Join(rs, ", "))
	}
	sb.WriteString("\nStart by reading the source files above and any existing *_test.go in the package.")
	if dir := skillRefsDir(); dir != "" {
		fmt.Fprintf(&sb, " Reference guidance (read only if needed): %s.", filepath.Join(dir, "junk-patterns.md"))
	}
	sb.WriteString("\n\n")
	sb.WriteString(rules)
	if lim := o.cfg.MaxComplexity; lim > 0 {
		fmt.Fprintf(&sb, "\n- COMPLEXITY LIMIT: every function you write, including test functions, subtests' parent functions and helpers, must stay at or below %d (%s). Keep each Test function small: one scenario group per top-level Test, a flat table-driven loop with a single if per case, repeated setup/assertions moved into helpers. The tool measures this and REJECTS the work if a function is over the limit.", lim, o.cfg.ComplexityMetric)
	}
	if failNote != "" {
		sb.WriteString("\n\nPREVIOUS ATTEMPT FEEDBACK - fix this first:\n" + failNote + "\n")
	}
	return sb.String()
}

// skillRefsDir locates references/ relative to the running binary or source
// tree (scripts/gocov/ or scripts/bin/ -> ../../references).
func skillRefsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	for _, base := range []string{filepath.Dir(exe), "."} {
		d, _ := filepath.Abs(base)
		for i := 0; i < 4; i++ {
			c := filepath.Join(d, "references")
			if _, err := os.Stat(filepath.Join(c, "junk-patterns.md")); err == nil {
				return c
			}
			d = filepath.Dir(d)
		}
	}
	return ""
}

func goAtLeast122(v string) bool {
	var maj, min int
	fmt.Sscanf(v, "%d.%d", &maj, &min)
	return maj > 1 || (maj == 1 && min >= 22)
}

func tailFile(p string, n int) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ------------------------------------------------------------------- git ----

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func isGit(root string) bool {
	out, err := git(root, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

func gitCheckoutBranch(root, branch string) error {
	if _, err := git(root, "rev-parse", "--verify", branch); err == nil {
		_, err = git(root, "checkout", branch)
		return err
	}
	out, err := git(root, "checkout", "-b", branch)
	if err != nil {
		return fmt.Errorf("git checkout -b %s: %s", branch, out)
	}
	return nil
}

func gitCommit(root string, it *Item) {
	if len(it.TestFiles) == 0 {
		return
	}
	_, _ = git(root, append([]string{"add", "--"}, it.TestFiles...)...)
	_, _ = git(root, "commit", "-m", fmt.Sprintf("test(%s): add behavioral tests (+%d covered statements)", it.Pkg, it.Gained), "--", ".")
}

// changed returns repo-relative path -> content hash for every modified or
// untracked file (ignoring the gocov output dir).
func snapshot(root, outDir string) map[string]string {
	out, err := git(root, "status", "--porcelain", "-z", "-uall")
	res := map[string]string{}
	if err != nil {
		return res
	}
	outAbs, _ := filepath.Abs(outDir)
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], e[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++
		}
		abs := filepath.Join(root, filepath.FromSlash(p))
		if outAbs != "" && strings.HasPrefix(abs, outAbs) {
			continue
		}
		h := "deleted"
		if data, err := os.ReadFile(abs); err == nil {
			s := sha1.Sum(data)
			h = hex.EncodeToString(s[:])
		}
		res[p] = h
	}
	return res
}

func dirtyFiles(root, outDir string) []string {
	var out []string
	for p := range snapshot(root, outDir) {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func delta(root, outDir string, snap map[string]string) []string {
	now := snapshot(root, outDir)
	var out []string
	for p, h := range now {
		if old, ok := snap[p]; !ok || old != h {
			out = append(out, p)
		}
	}
	for p := range snap {
		if _, ok := now[p]; !ok {
			out = append(out, p) // reverted to HEAD since snapshot; harmless
		}
	}
	sort.Strings(out)
	return out
}

func isAllowedPath(p string, allowDeps bool) bool {
	if strings.HasSuffix(p, "_test.go") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "testdata" {
			return true
		}
	}
	if allowDeps && (path.Base(p) == "go.mod" || path.Base(p) == "go.sum") {
		return true
	}
	return false
}

func restore(root, p string) {
	if _, err := git(root, "ls-files", "--error-unmatch", "--", p); err == nil {
		_, _ = git(root, "checkout", "--", p)
		return
	}
	_ = os.Remove(filepath.Join(root, filepath.FromSlash(p)))
}

func revertForbidden(root string, snap map[string]string, outDir string, allowDeps bool) []string {
	var bad []string
	for _, p := range delta(root, outDir, snap) {
		if _, existed := snap[p]; existed {
			continue
		}
		if !isAllowedPath(p, allowDeps) {
			restore(root, p)
			bad = append(bad, p)
		}
	}
	return bad
}

func revertAll(root string, snap map[string]string, outDir string) {
	for _, p := range delta(root, outDir, snap) {
		if _, existed := snap[p]; existed {
			continue
		}
		restore(root, p)
	}
}

func changedTestFiles(root string, snap map[string]string, outDir string) []string {
	var out []string
	for _, p := range delta(root, outDir, snap) {
		if isAllowedPath(p, false) {
			out = append(out, p)
		}
	}
	return out
}

// authProbe makes one trivial, capped call so a missing login fails in seconds
// with instructions, instead of after planning and baselining everything.
func authProbe(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "-p", "Reply with the single word ok.", "--output-format", "json", "--max-budget-usd", "0.05", "--no-session-persistence", "--model", "haiku")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	_ = cmd.Run()
	var res claudeResult
	if json.Unmarshal(out.Bytes(), &res) == nil && !res.IsError {
		return nil
	}
	why := strings.TrimSpace(res.Result)
	if why == "" {
		why = firstLine(errb.String())
	}
	return fmt.Errorf(`headless mode cannot use Claude: %s

A child "claude -p" process cannot borrow the login of the Claude Desktop app or of the session that launched it. Pick one - none needs a new login if you are inside Claude Desktop/CLI:
  1. EASIEST, no login: use the session itself. Run the skill interactively and let the session write the tests, with gocov guarding it:
       gocov begin  -config C -repo R -item ID   (prints the task prompt)
       ... the session / its subagents write the *_test.go files ...
       gocov finish -config C -repo R -item ID   (verifies, accepts or gives feedback)
  2. Run gocov from a normal terminal where the standalone CLI is logged in ("claude auth status" shows loggedIn:true). If it says false, run "claude" once and /login (one-time).
  3. Unattended / CI: set ANTHROPIC_API_KEY (a Console API key); no OAuth involved.`, why)
}

// ---- begin / finish: the same guard rails around an author we do not spawn ----

func itemByID(st *RepoState, id string) (*Item, error) {
	for i := range st.Items {
		if st.Items[i].ID == id {
			return &st.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no item %q (ids are listed in PLAN.md)", id)
}

func oneRepo(o *opts) (RepoCfg, *RepoState, error) {
	rs := o.repos()
	if len(rs) != 1 {
		return RepoCfg{}, nil, fmt.Errorf("select exactly one repo with -repo NAME")
	}
	st, err := loadState(o, rs[0].Name)
	return rs[0], st, err
}

func snapPath(o *opts, rc RepoCfg, it *Item) string {
	return filepath.Join(repoDir(o, rc.Name), "work", slug(it.ID)+".snap.json")
}

// cmdBegin: pre-measure, snapshot the tree, write the focused task prompt.
func cmdBegin(ctx context.Context, o *opts, id string) error {
	rc, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	it, err := itemByID(st, id)
	if err != nil {
		return err
	}
	if st.Status == "planned" && isGit(rc.Path) {
		if dirty := dirtyFiles(rc.Path, o.cfg.OutDir); len(dirty) > 0 {
			return fmt.Errorf("working tree has uncommitted changes (e.g. %s); commit or stash first", dirty[0])
		}
		if rc.Branch != "" {
			if err := gitCheckoutBranch(rc.Path, rc.Branch); err != nil {
				return err
			}
		}
	}
	before, err := measureItem(ctx, o, rc, st, it, "pre")
	if err != nil {
		return err
	}
	it.PreCovered = coverOf(before.Rows, it.Files)
	recordPreComplexity(o, rc, it)
	it.PkgBefore = pctOf(before.Rows, it.Files)
	snap := snapshot(rc.Path, o.cfg.OutDir)
	_ = os.MkdirAll(filepath.Dir(snapPath(o, rc, it)), 0o755)
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(snapPath(o, rc, it), data, 0o644); err != nil {
		return err
	}
	prompt := buildPrompt(o, rc, st, it, before, "")
	pp := filepath.Join(repoDir(o, rc.Name), "work", slug(it.ID)+".prompt.md")
	if err := os.WriteFile(pp, []byte(prompt), 0o644); err != nil {
		return err
	}
	st.Status = "in-progress"
	if err := saveState(o, st); err != nil {
		return err
	}
	_ = os.Remove(strings.TrimSuffix(pp, ".prompt.md") + ".feedback.md")
	if o.quiet {
		fmt.Println(pp)
		return nil
	}
	fmt.Printf("Task prompt for %s written to %s\nWrite the *_test.go files, then run: gocov finish -repo %s -item %s\n\n%s\n", it.ID, pp, rc.Name, it.ID, prompt)
	return nil
}

// cmdFinish: verify what was written (revert forbidden edits, tests green, flake
// check, real gain). Success marks done; failure returns feedback, or with
// -abandon reverts the attempt and marks the item blocked.
func cmdFinish(ctx context.Context, o *opts, id string, abandon bool, cost float64) error {
	rc, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	it, err := itemByID(st, id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(snapPath(o, rc, it))
	if err != nil {
		return fmt.Errorf("no begin snapshot for %s - run `gocov begin` first", it.ID)
	}
	snap := map[string]string{}
	_ = json.Unmarshal(data, &snap)
	g := isGit(rc.Path)
	it.Attempts++
	it.CostUSD += cost
	st.SpendUSD += cost
	if abandon {
		if g {
			revertAll(rc.Path, snap, o.cfg.OutDir)
		}
		it.Status, it.Reason = "blocked", "abandoned by author after "+fmt.Sprint(it.Attempts)+" attempt(s)"
		st.log("blocked:" + it.ID)
		fmt.Println("BLOCKED", it.ID, "- attempt reverted")
		return saveState(o, st)
	}
	ok, fb := verifyItem(ctx, o, rc, st, it, it.PreCovered, snap, g)
	st.Verified = false
	if err := saveState(o, st); err != nil {
		return err
	}
	if ok {
		_ = os.Remove(feedbackPath(o, rc, it))
		return nil
	}
	_ = os.WriteFile(feedbackPath(o, rc, it), []byte(fb+"\n"), 0o644)
	exitCode = 3
	fmt.Printf("NOT ACCEPTED (attempt %d/%d). Fix and run `gocov finish` again, or `gocov finish -abandon` to revert and block:\n%s\n", it.Attempts, o.cfg.MaxAttempts, fb)
	return nil
}

// recordPreComplexity remembers which test functions in the package were
// already over the limit, so only newly introduced ones are blamed.
func recordPreComplexity(o *opts, rc RepoCfg, it *Item) {
	it.PreOver = nil
	if o.cfg.MaxComplexity <= 0 {
		return
	}
	dir := filepath.Join(rc.Path, filepath.FromSlash(it.Pkg))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, path.Join(it.Pkg, e.Name()))
		}
	}
	for _, f := range complexityOver(complexityOf(rc.Path, files), o.cfg.MaxComplexity, o.cfg.ComplexityMetric) {
		it.PreOver = append(it.PreOver, f.File+":"+f.Name)
	}
}

func feedbackPath(o *opts, rc RepoCfg, it *Item) string {
	return filepath.Join(repoDir(o, rc.Name), "work", slug(it.ID)+".feedback.md")
}

// cmdQueue prints the IDs of queued items, one per line, so a plain shell
// loop can drive the work (the article's fan-out pattern).
func cmdQueue(o *opts) error {
	_, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	for _, it := range st.Items {
		if it.Status == "queued" {
			fmt.Println(it.ID)
		}
	}
	return nil
}

// cmdGoal reports whether the running estimate has reached the goal (exit 0)
// or not (exit 10). The estimate is verified later by `gocov verify`.
func cmdGoal(o *opts) error {
	_, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	pct := st.goalTotals().Pct()
	fmt.Printf("estimate %.2f%% goal %.0f%%\n", pct, st.Goal)
	if pct < st.Goal {
		exitCode = 10
	}
	return nil
}
