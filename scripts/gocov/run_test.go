package main

// Tests of `gocov run`, the fan-out scripts and the guard rails around an
// item. The model is a fake `claude` built from fakeClaudeSrc and put first on
// PATH: it logs every call and sleeps, writes a test file or reports a cost as
// the FAKE_CLAUDE_* variables say. No real model is ever called.

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const fakeClaudeSrc = `package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	args := strings.Join(os.Args[1:], " ")
	wd, _ := os.Getwd()
	if f, err := os.OpenFile(os.Getenv("FAKE_CLAUDE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "cwd=%s args=%s\n", wd, args)
		f.Close()
	}
	if strings.Contains(args, "Reply with the single word ok.") { // gocov's auth probe
		fmt.Print(` + "`" + `{"is_error":false,"result":"ok","total_cost_usd":0}` + "`" + `)
		return
	}
	io.Copy(io.Discard, os.Stdin)
	if d, err := time.ParseDuration(os.Getenv("FAKE_CLAUDE_SLEEP")); err == nil {
		time.Sleep(d)
	}
	if src := os.Getenv("FAKE_CLAUDE_COPY"); src != "" {
		data, _ := os.ReadFile(src)
		dst := filepath.FromSlash(os.Getenv("FAKE_CLAUDE_DEST"))
		os.MkdirAll(filepath.Dir(dst), 0o755)
		os.WriteFile(dst, data, 0o644)
	}
	cost := os.Getenv("FAKE_CLAUDE_COST")
	if cost == "" {
		cost = "0.1"
	}
	fmt.Printf(` + "`" + `{"is_error":false,"subtype":"success","result":"done","total_cost_usd":%s}` + "`" + `, cost)
}
`

var (
	fakeOnce sync.Once
	fakeDir  string
	fakeErr  error
)

// fakeClaude puts the fake claude first on PATH and returns the file its calls
// are logged to, one line per call.
func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "gocov-fake-claude-")
		if fakeErr != nil {
			return
		}
		src := filepath.Join(fakeDir, "src")
		_ = os.MkdirAll(src, 0o755)
		_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fakeclaude\n\ngo 1.21\n"), 0o644)
		_ = os.WriteFile(filepath.Join(src, "main.go"), []byte(fakeClaudeSrc), 0o644)
		cmd := exec.Command("go", "build", "-o", filepath.Join(fakeDir, "bin", exeName("claude")), ".")
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeErr = &fatalErr{"build fake claude: " + err.Error() + "\n" + string(out)}
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	log := filepath.Join(t.TempDir(), "claude-calls.log")
	t.Setenv("PATH", filepath.Join(fakeDir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_LOG", log)
	for _, k := range []string{"FAKE_CLAUDE_SLEEP", "FAKE_CLAUDE_COPY", "FAKE_CLAUDE_DEST", "FAKE_CLAUDE_COST"} {
		t.Setenv(k, "")
	}
	return log
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, d := range []string{fakeDir, scriptsDir} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
	os.Exit(code)
}

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// modelCalls returns the logged calls that were not the auth probe.
func modelCalls(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" && !strings.Contains(l, "Reply with the single word ok.") {
			out = append(out, l)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// killConfig makes every claude call hang past a ~2s item timeout.
func killConfig(t *testing.T, o *opts) {
	t.Helper()
	o.cfg.Budget.MaxUSDPerItem, o.cfg.Budget.MaxUSDPerRepo, o.cfg.Budget.MaxUSDTotal = 0.5, 5, 5
	o.cfg.Budget.ItemTimeoutMin = 0.03 // 1.8s; next-call rounds it up to 2s
	o.cfg.MaxAttempts = 2
	t.Setenv("FAKE_CLAUDE_SLEEP", "60s")
}

func TestRunChargesKilledCallsTheirCap(t *testing.T) {
	o, _ := newFixture(t, 95)
	reportAndPlan(t, o)
	log := fakeClaude(t)
	killConfig(t, o)

	mustRun(t, "run", cmdRun(context.Background(), o))
	if n := len(modelCalls(t, log)); n != 2 {
		t.Fatalf("%d model calls, want 2 (both attempts)", n)
	}
	st := state(t, o)
	it := item(t, st, calcItem)
	if it.Status != "blocked" || !near(it.CostUSD, 1.0) || !near(st.SpendUSD, 1.0) {
		t.Errorf("item %s cost $%.2f, repo spend $%.2f; want blocked, two killed calls charged their $0.50 cap each ($1.00)", it.Status, it.CostUSD, st.SpendUSD)
	}
}

func TestRunVerifiesOnceWhenGoalReached(t *testing.T) {
	o, dir := newFixture(t, 95)
	reportAndPlan(t, o)
	log := fakeClaude(t)
	src := filepath.Join(t.TempDir(), "grade_test.go")
	mustRun(t, "write test source", os.WriteFile(src, []byte(gradeTest), 0o644))
	t.Setenv("FAKE_CLAUDE_COPY", src)
	t.Setenv("FAKE_CLAUDE_DEST", "calc/grade_test.go")

	before := fullRuns.Load()
	mustRun(t, "run", cmdRun(context.Background(), o))
	if calls := modelCalls(t, log); len(calls) != 1 || !strings.Contains(calls[0], "cwd="+dir) {
		t.Fatalf("model calls = %q, want one, run in the module dir %s", calls, dir)
	}
	if st := state(t, o); st.Status != "achieved" {
		t.Fatalf("status %q (%s), want achieved", st.Status, st.Reason)
	}
	if n := fullRuns.Load() - before; n != 1 {
		t.Errorf("%d fresh full runs after the goal was reached, want 1 (the in-loop verify is the close-out figure)", n)
	}
}

func TestBudgetReserveThenSettle(t *testing.T) {
	cfg := &Config{}
	cfg.Budget.MaxUSDTotal, cfg.Budget.MaxUSDPerRepo, cfg.Budget.MaxUSDPerItem = 2, 10, 1.5
	b := &budget{cfg: cfg}
	steps := []struct {
		name string
		do   func() (float64, bool)
		want float64
		ok   bool
	}{
		{"first call gets the per-item cap", func() (float64, bool) { return b.reserve(0) }, 1.5, true},
		{"second, while the first runs, gets only what is unreserved", func() (float64, bool) { return b.reserve(0) }, 0.5, true},
		{"third gets nothing", func() (float64, bool) { return b.reserve(0) }, 0, false},
		{"settling the first to $0.30 frees the rest of its cap", func() (float64, bool) { b.settle(1.5, 0.3); return b.reserve(0) }, 1.2, true},
	}
	for _, s := range steps {
		if got, ok := s.do(); !near(got, s.want) || ok != s.ok {
			t.Fatalf("%s: reserve = %.2f, %v; want %.2f, %v", s.name, got, ok, s.want, s.ok)
		}
	}
	if !near(b.runSpent(), 0.3) || !near(b.committed(), 2.0) {
		t.Errorf("spent %.2f committed %.2f, want 0.30 settled and 2.00 committed", b.runSpent(), b.committed())
	}
}

func TestBudgetConcurrentReservationsNeverExceedTotal(t *testing.T) {
	cfg := &Config{}
	cfg.Budget.MaxUSDTotal, cfg.Budget.MaxUSDPerRepo, cfg.Budget.MaxUSDPerItem = 5, 10, 0.5
	b := &budget{cfg: cfg}
	var mu sync.Mutex
	var granted float64
	var calls int
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, ok := b.reserve(0); ok {
				mu.Lock()
				granted += c
				calls++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if calls != 10 || !near(granted, 5) {
		t.Errorf("50 concurrent calls were granted %d caps totalling $%.2f; want 10 totalling $5.00 (max_usd_total)", calls, granted)
	}
}

func TestPromptGivesModuleRelativePackage(t *testing.T) {
	cfg := &Config{}
	cfg.defaults()
	root := t.TempDir()
	rc := RepoCfg{Name: "demo", Path: root}
	st := &RepoState{Goal: 80, Modules: []Module{{Dir: "svc", Path: "example.com/svc"}}}
	cases := []struct {
		name, module, pkg, wantDir, wantPkg string
	}{
		{"nested module", "svc", "svc/api", filepath.Join(root, "svc"), "Package dir: ./api (relative to the module root; svc/api from the repo root)"},
		{"package at a module root", "svc", "svc", filepath.Join(root, "svc"), "Package dir: . (relative to the module root; svc from the repo root)"},
		{"root module", ".", "calc", root, "Package dir: ./calc ("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := buildPrompt(&opts{cfg: cfg}, rc, st, &Item{Module: tc.module, Pkg: tc.pkg}, "")
			if !strings.Contains(p, "Working directory: "+tc.wantDir+" (") || !strings.Contains(p, tc.wantPkg) {
				t.Errorf("prompt should state the working directory %s and %q; got:\n%s", tc.wantDir, tc.wantPkg, p)
			}
		})
	}
}

// orderTest passes in source order, also repeated (Step8 resets), but fails
// in any other order: only -shuffle=on can catch it.
const orderTest = `package calc

import "testing"

var step int

func advance(t *testing.T, n int) {
	t.Helper()
	if step != n-1 {
		t.Fatalf("ran after step %d, want step %d first", step, n-1)
	}
	step = n
}

func TestStep1(t *testing.T) { advance(t, 1) }
func TestStep2(t *testing.T) { advance(t, 2) }
func TestStep3(t *testing.T) { advance(t, 3) }
func TestStep4(t *testing.T) { advance(t, 4) }
func TestStep5(t *testing.T) { advance(t, 5) }
func TestStep6(t *testing.T) { advance(t, 6) }
func TestStep7(t *testing.T) { advance(t, 7) }

func TestStep8(t *testing.T) {
	advance(t, 8)
	step = 0
	for in, want := range map[int]string{95: "A", 85: "B", 75: "C", 65: "D", 10: "F"} {
		if got := Grade(in); got != want {
			t.Errorf("Grade(%d) = %q, want %q", in, got, want)
		}
	}
}
`

// A self-assignment: vet's assign check flags it, go test's vet subset does not.
const vetBadTest = gradeTest + `
func TestGradeTop(t *testing.T) {
	g := Grade(95)
	g = g
	if g != "A" {
		t.Fatal(g)
	}
}
`

func TestFinishRejectsTestsThatDependOnOrder(t *testing.T) {
	o, dir := newFixture(t, 95)
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/order_test.go", orderTest)
	if code := finish(t, o, calcItem); code != 3 {
		t.Fatalf("order-dependent tests were accepted, finish exit = %d", code)
	}
	fb, _ := os.ReadFile(feedbackPath(o, o.cfg.Repos[0], &Item{ID: calcItem}))
	if !strings.Contains(string(fb), "-shuffle=on") {
		t.Errorf("feedback should name the shuffled repeat run; got:\n%s", fb)
	}
}

func TestFinishRejectsVetFindingsInNewTests(t *testing.T) {
	o, dir := newFixture(t, 95)
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", vetBadTest)
	if code := finish(t, o, calcItem); code != 3 {
		t.Fatalf("a test file go vet flags was accepted, finish exit = %d", code)
	}
	fb, _ := os.ReadFile(feedbackPath(o, o.cfg.Repos[0], &Item{ID: calcItem}))
	if !strings.Contains(string(fb), "grade_test.go") || !strings.Contains(string(fb), "self-assignment") {
		t.Errorf("feedback should quote vet's finding in grade_test.go; got:\n%s", fb)
	}
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Errorf("the fixed test was not accepted, finish exit = %d", code)
	}
}

func TestFinishIgnoresVetFindingsOutsideNewTests(t *testing.T) {
	o, dir := newFixture(t, 95)
	writeFile(t, dir, "calc/legacy.go", "package calc\n\nfunc legacy() int {\n\tx := 1\n\tx = x\n\treturn x\n}\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "legacy vet issue")
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		fb, _ := os.ReadFile(feedbackPath(o, o.cfg.Repos[0], &Item{ID: calcItem}))
		t.Errorf("a clean test was rejected for vet findings in production code, finish exit = %d:\n%s", code, fb)
	}
}

func TestFinishCommitsOnlyItsTestFiles(t *testing.T) {
	o, dir := newFixture(t, 95)
	o.commit = true
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	// the user's own edit to a tracked file before the item's snapshot: not the item's
	writeFile(t, dir, "util/util_test.go", "package util\n\n// the user's own edit, unrelated to the item\n")
	mustRun(t, "begin again", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Fatalf("finish exit = %d", code)
	}
	if out, _ := git(dir, "show", "--name-only", "--format=", "HEAD"); strings.TrimSpace(out) != "calc/grade_test.go" {
		t.Errorf("item commit contains %q, want only calc/grade_test.go", strings.TrimSpace(out))
	}
	if out, _ := git(dir, "status", "--porcelain", "--", "util/util_test.go"); !strings.HasPrefix(out, " M") {
		t.Errorf("the user's edit should stay uncommitted and unstaged; status %q", out)
	}
}

func TestFinishReportsCommitFailure(t *testing.T) {
	o, dir := newFixture(t, 95)
	o.commit = true
	reportAndPlan(t, o)
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	writeFile(t, dir, ".git/hooks/pre-commit", "#!/bin/sh\necho rejected by hook >&2\nexit 1\n")
	mustRun(t, "chmod hook", os.Chmod(hook, 0o755))
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Fatalf("finish exit = %d; verified tests stay accepted when only the commit fails", code)
	}
	st := state(t, o)
	if w := strings.Join(st.Warnings, "\n"); !strings.Contains(w, "NOT committed") || !strings.Contains(w, "rejected by hook") {
		t.Errorf("warnings %q should report the failed commit and git's reason", w)
	}
}
