package main

// Tests of `gocov run -parallel N` and of run tracking (runs[], per-item
// timing, events.jsonl). The author is the fake claude from run_test.go: it
// writes a prepared test file per package, so no model is ever called.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// gradePkg is a package with one 6-statement function nobody tests.
func gradePkg(name string) string {
	return fmt.Sprintf(`package %s

func Grade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	case score >= 70:
		return "C"
	case score >= 60:
		return "D"
	}
	return "F"
}
`, name)
}

func gradeTestFor(name string) string {
	return strings.Replace(gradeTest, "package calc", "package "+name, 1)
}

// parFixture: four independent packages p1..p4 (6 statements each) and a
// dependent pair, top (7) importing base (9). The plan ranks base, top, p1,
// p2, p3, p4, so with parallel 3 a dependency-blind wave would hold base and top.
var parFixture = map[string]string{
	"go.mod": "module example.com/par\n\ngo 1.22\n",
	"base/base.go": `package base

func Level(v int) string {
	switch {
	case v >= 90:
		return "high"
	case v >= 50:
		return "mid"
	case v >= 10:
		return "low"
	case v >= 0:
		return "tiny"
	}
	return "neg"
}

func Clamp(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
`,
	"top/top.go": `package top

import "example.com/par/base"

func Describe(v int) string {
	if v > 1000 {
		return "huge"
	}
	if v > 500 {
		return "big"
	}
	if v < -1000 {
		return "very negative"
	}
	return "level " + base.Level(v)
}
`,
	"p1/p1.go": gradePkg("p1"),
	"p2/p2.go": gradePkg("p2"),
	"p3/p3.go": gradePkg("p3"),
	"p4/p4.go": gradePkg("p4"),
}

var parTests = map[string]string{
	"base": `package base

import "testing"

func TestLevel(t *testing.T) {
	for in, want := range map[int]string{95: "high", 60: "mid", 20: "low", 5: "tiny", -1: "neg"} {
		if got := Level(in); got != want {
			t.Errorf("Level(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestClamp(t *testing.T) {
	if Clamp(-3) != 0 || Clamp(4) != 4 {
		t.Error("Clamp does not clamp at 0")
	}
}
`,
	"top": `package top

import "testing"

func TestDescribe(t *testing.T) {
	for in, want := range map[int]string{2000: "huge", 700: "big", -2000: "very negative", 60: "level mid"} {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%d) = %q, want %q", in, got, want)
		}
	}
}
`,
	"p1": gradeTestFor("p1"),
	"p2": gradeTestFor("p2"),
	"p3": gradeTestFor("p3"),
	"p4": gradeTestFor("p4"),
}

var parItems = []string{"root:base", "root:top", "root:p1", "root:p2", "root:p3", "root:p4"}

// newParFixture makes the repo, plans it with every package queued and puts
// the fake claude in place, writing each package's prepared test.
func newParFixture(t *testing.T, goal float64, parallel int) (*opts, string, string) {
	t.Helper()
	o, dir := newFixtureFrom(t, parFixture, goal)
	o.cfg.Parallel = parallel
	reportAndPlan(t, o)
	log := fakeClaude(t)
	src := t.TempDir()
	for pkg, body := range parTests {
		mustRun(t, "write test source", os.WriteFile(filepath.Join(src, pkg+".go.txt"), []byte(body), 0o644))
	}
	t.Setenv("FAKE_CLAUDE_TESTS", src)
	t.Setenv("GOFLAGS", "")
	return o, dir, log
}

type event struct {
	T        string   `json:"t"`
	Run      string   `json:"run"`
	Ev       string   `json:"ev"`
	Item     string   `json:"item"`
	Items    []string `json:"items"`
	Worker   int      `json:"worker"`
	Wave     int      `json:"wave"`
	Status   string   `json:"status"`
	Started  string   `json:"started"`
	CapUSD   float64  `json:"cap_usd"`
	CostUSD  float64  `json:"cost_usd"`
	SHA      string   `json:"sha"`
	Driver   string   `json:"driver"`
	Parallel int      `json:"parallel"`
	Verified float64  `json:"verified_pct"`
}

// readEvents parses events.jsonl; every line must be a whole JSON object.
func readEvents(t *testing.T, o *opts) []event {
	t.Helper()
	f, err := os.Open(filepath.Join(repoDir(o, "demo"), "events.jsonl"))
	mustRun(t, "open events.jsonl", err)
	defer f.Close()
	var evs []event
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Ev == "" || e.T == "" || e.Run == "" {
			t.Fatalf("events.jsonl line %d is not a whole event (%v): %s", n, err, sc.Text())
		}
		evs = append(evs, e)
	}
	return evs
}

func evTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	mustRun(t, "parse event time", err)
	return tm
}

// itemSpans pairs each item's item-start with its item-end. Every item that
// started must have ended.
func itemSpans(t *testing.T, evs []event) map[string]span {
	t.Helper()
	open := map[string]time.Time{}
	spans := map[string]span{}
	for _, e := range evs {
		switch e.Ev {
		case "item-start":
			open[e.Item] = evTime(t, e.T)
		case "item-end":
			s, ok := open[e.Item]
			if !ok {
				t.Fatalf("item-end of %s without an item-start", e.Item)
			}
			spans[e.Item] = span{s, evTime(t, e.T)}
			delete(open, e.Item)
		}
	}
	if len(open) > 0 {
		t.Fatalf("items started but never ended: %v", open)
	}
	return spans
}

func spanList(m map[string]span) []span {
	var out []span
	for _, s := range m {
		out = append(out, s)
	}
	return out
}

// captureOut runs fn with stdout captured (read while fn runs, so a long
// output cannot fill the pipe).
func captureOut(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	mustRun(t, "pipe", err)
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	ferr := fn()
	os.Stdout = stdout
	w.Close()
	return <-done, ferr
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func TestParallelRunWavesIsolationMergeAndState(t *testing.T) {
	o, dir, log := newParFixture(t, 100, 3)
	base := gitOut(t, dir, "rev-parse", "HEAD")
	// p1's author leaves a mutation of p2's code in its tree, and holds it
	// there until p2 has been verified and merged
	mutant := filepath.Join(t.TempDir(), "mutant.go")
	mustRun(t, "write mutant", os.WriteFile(mutant, []byte(strings.Replace(gradePkg("p2"), `return "F"`, `return "MUTANT"`, 1)), 0o644))
	t.Setenv("FAKE_CLAUDE_MUTATE_PKG", "p1")
	t.Setenv("FAKE_CLAUDE_MUTATE_FILE", "p2/p2.go")
	t.Setenv("FAKE_CLAUDE_MUTANT", mutant)
	t.Setenv("FAKE_CLAUDE_WAIT_FILE", filepath.Join(repoDir(o, "demo"), "events.jsonl"))
	t.Setenv("FAKE_CLAUDE_WAIT_FOR", `"ev":"item-end",`+`"item":"root:p2"`)
	t.Setenv("FAKE_CLAUDE_SLEEP", "500ms")

	out, err := captureOut(t, func() error { return cmdRun(context.Background(), o) })
	mustRun(t, "run:\n"+out, err)
	st := state(t, o)
	if st.Status != "achieved" || st.goalTotals().Pct() != 100 {
		t.Fatalf("status %s at %.2f%% (%s), want achieved at 100%%\n%s", st.Status, st.goalTotals().Pct(), st.Reason, out)
	}
	if n := len(modelCalls(t, log)); n != 6 {
		t.Errorf("%d model calls, want 6 (one per package)", n)
	}
	evs := readEvents(t, o)

	t.Run("waves keep dependent packages apart", func(t *testing.T) {
		var waves [][]string
		for _, e := range evs {
			if e.Ev == "wave-start" {
				waves = append(waves, e.Items)
			}
		}
		want := [][]string{{"root:base", "root:p1", "root:p2"}, {"root:top", "root:p3", "root:p4"}}
		if fmt.Sprint(waves) != fmt.Sprint(want) {
			t.Errorf("waves = %v, want %v (top imports base: never in base's wave)", waves, want)
		}
	})

	t.Run("one worker's mutation never reaches another", func(t *testing.T) {
		if it := item(t, st, "root:p2"); it.Status != "done" {
			t.Errorf("p2 = %s (%s): its tests ran against p1's mutant", it.Status, it.Reason)
		}
		if got := gitOut(t, dir, "show", "HEAD:p2/p2.go"); got != strings.TrimSpace(gradePkg("p2")) {
			t.Errorf("the work branch has a changed p2/p2.go:\n%s", got)
		}
		if w := strings.Join(st.Warnings, "\n"); !strings.Contains(w, "root:p1") || !strings.Contains(w, "p2/p2.go") {
			t.Errorf("warnings should record that p1's edit of p2/p2.go was reverted: %q", w)
		}
		if s := gitOut(t, dir, "status", "--porcelain"); s != "" {
			t.Errorf("work tree not clean after the run: %q", s)
		}
	})

	t.Run("accepted items merge one commit each onto the work branch", func(t *testing.T) {
		if n := gitOut(t, dir, "rev-list", "--count", base+"..HEAD"); n != "6" {
			t.Errorf("%s commits on the work branch, want 6", n)
		}
		if m := gitOut(t, dir, "rev-list", "--merges", base+"..HEAD"); m != "" {
			t.Errorf("merge commits on the work branch (want a linear history): %s", m)
		}
		for _, id := range parItems {
			it := item(t, st, id)
			pkg := strings.TrimPrefix(id, "root:")
			files := gitOut(t, dir, "show", "--name-only", "--format=", it.MergeSHA)
			if it.Status != "done" || files != pkg+"/cov_test.go" {
				t.Errorf("%s: status %s, merge %q holds %q; want done, one commit with %s/cov_test.go", id, it.Status, it.MergeSHA, files, pkg)
			}
		}
		if wt := gitOut(t, dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
			t.Errorf("item worktrees left behind:\n%s", wt)
		}
		if br := gitOut(t, dir, "branch", "--list", "cov/*"); br != "" {
			t.Errorf("item branches left behind: %s", br)
		}
	})

	t.Run("state is consistent", func(t *testing.T) {
		var items, attempts, evCost float64
		for _, it := range st.Items {
			items += it.CostUSD
			for _, a := range it.AttemptLog {
				attempts += a.CostUSD
			}
			if it.StartedAt == "" || it.FinishedAt == "" || it.Worker < 1 || it.Worker > 3 || it.Wave < 1 || it.Worktree == "" || it.Branch == "" {
				t.Errorf("%s lacks its timing or placement: %+v", it.ID, it)
			}
		}
		for _, e := range evs {
			if e.Ev == "attempt" {
				evCost += e.CostUSD
			}
		}
		if !near(st.SpendUSD, items) || !near(items, attempts) || !near(items, evCost) || !near(st.SpendUSD, 0.6) {
			t.Errorf("spend: repo $%.2f, items $%.2f, attempt logs $%.2f, attempt events $%.2f; want all $0.60", st.SpendUSD, items, attempts, evCost)
		}
		estimate := st.goalTotals()
		mustRun(t, "verify", cmdVerify(context.Background(), o))
		if v := state(t, o).goalTotals(); v != estimate {
			t.Errorf("state %+v differs from a fresh verify %+v", estimate, v)
		}
	})

	t.Run("the run record shows it really ran in parallel", func(t *testing.T) {
		r := st.Runs[len(st.Runs)-1]
		if r.Driver != driverRun || r.Parallel != 3 || r.Jobs != o.cfg.Jobs || r.Models != o.cfg.Models || r.Budget != o.cfg.Budget || r.SkillVersion != skillVersion {
			t.Errorf("run record %+v does not match the flags", r)
		}
		if r.Ended == "" || r.Items != 6 || r.Waves != 2 || r.MaxConcurrencyObserved < 2 || r.Speedup <= 0 {
			t.Errorf("run record %+v: want ended, 6 items in 2 waves, max concurrency >= 2, a measured speed-up", r)
		}
		spans := itemSpans(t, evs)
		if len(spans) != 6 || maxOverlap(spanList(spans)) < 2 {
			t.Errorf("events.jsonl: %d item spans, max overlap %d; want 6 and >= 2", len(spans), maxOverlap(spanList(spans)))
		}
		if !regexp.MustCompile(`\[demo w[123]\] root:\w+ attempt 1/`).MatchString(out) {
			t.Errorf("console lines should carry the worker slot, e.g. [demo w2] root:p1 attempt 1/2:\n%s", out)
		}
		final, _ := os.ReadFile(filepath.Join(repoDir(o, "demo"), "FINAL_REPORT.md"))
		if !strings.Contains(string(final), "## Run") || !strings.Contains(string(final), "| 3 | 2 | 2 | 6 |") {
			t.Errorf("FINAL_REPORT.md lacks the Run section (parallel 3, jobs 2, waves 2, items 6):\n%s", final)
		}
	})
}

func TestParallelRunStopsDispatchAtCheckpoint(t *testing.T) {
	o, _, log := newParFixture(t, 50, 3)
	mustRun(t, "run", cmdRun(context.Background(), o))
	st := state(t, o)
	// wave 1 (base, p1, p2) covers 21 of 40 statements: 52.5% >= 50
	if st.Status != "achieved" || len(modelCalls(t, log)) != 3 {
		t.Fatalf("status %s (%s) after %d model calls; want achieved after wave 1's 3", st.Status, st.Reason, len(modelCalls(t, log)))
	}
	for _, id := range []string{"root:top", "root:p3"} {
		if it := item(t, st, id); it.Status != "queued" || it.Attempts != 0 {
			t.Errorf("%s = %s with %d attempts; want left queued, never dispatched", id, it.Status, it.Attempts)
		}
	}
	var checkpoints []event
	for _, e := range readEvents(t, o) {
		if e.Ev == "checkpoint" {
			checkpoints = append(checkpoints, e)
		}
		if e.Ev == "wave-start" && e.Wave > 1 {
			t.Errorf("wave %d was dispatched after the goal was verified", e.Wave)
		}
	}
	if len(checkpoints) != 1 || checkpoints[0].Wave != 1 || checkpoints[0].Verified != 52.5 {
		t.Errorf("checkpoints = %+v, want one after wave 1 at 52.5%%", checkpoints)
	}
}

func TestParallelBudgetNeverExceedsCap(t *testing.T) {
	o, _, _ := newParFixture(t, 100, 3)
	o.cfg.Budget.MaxUSDPerItem, o.cfg.Budget.MaxUSDPerRepo, o.cfg.Budget.MaxUSDTotal = 0.5, 10, 1.0
	t.Setenv("FAKE_CLAUDE_COST", "0.3")
	t.Setenv("FAKE_CLAUDE_SLEEP", "300ms")
	mustRun(t, "run", cmdRun(context.Background(), o))
	st := state(t, o)
	if st.Status != "paused-budget" || st.SpendUSD > 1.0+1e-9 {
		t.Fatalf("status %s, spend $%.2f; want paused-budget within the $1.00 cap", st.Status, st.SpendUSD)
	}
	// every call's cap was reserved while it ran: at no instant may the caps
	// of the calls in flight add up to more than max_usd_total
	type pt struct {
		t time.Time
		d float64
	}
	var pts []pt
	maxWave := map[int]int{}
	evs := readEvents(t, o)
	for _, e := range evs {
		switch e.Ev {
		case "attempt":
			pts = append(pts, pt{evTime(t, e.Started), e.CapUSD}, pt{evTime(t, e.T), -e.CapUSD})
		case "wave-start":
			maxWave[e.Wave] = len(e.Items)
		}
	}
	sort.Slice(pts, func(i, j int) bool {
		if !pts[i].t.Equal(pts[j].t) {
			return pts[i].t.Before(pts[j].t)
		}
		return pts[i].d < pts[j].d
	})
	var cur, peak float64
	for _, p := range pts {
		cur += p.d
		peak = max(peak, cur)
	}
	if peak > 1.0+1e-9 || maxWave[1] != 2 {
		t.Errorf("peak of caps in flight $%.2f (want <= $1.00); wave 1 dispatched %d items (want 2: the third could not reserve a cap)", peak, maxWave[1])
	}
}

func TestSequentialRunRecordsConcurrencyOne(t *testing.T) {
	o, _, _ := newParFixture(t, 100, 1)
	o.pilot = 3
	mustRun(t, "run", cmdRun(context.Background(), o))
	st := state(t, o)
	r := st.Runs[len(st.Runs)-1]
	if r.Parallel != 1 || r.Driver != driverRun || r.Items != 3 || r.Waves != 3 || r.MaxConcurrencyObserved != 1 {
		t.Errorf("run record %+v; want parallel 1, 3 items in 3 waves of one, max concurrency 1", r)
	}
	spans := itemSpans(t, readEvents(t, o))
	if len(spans) != 3 || maxOverlap(spanList(spans)) != 1 {
		t.Errorf("events.jsonl: %d item spans, max overlap %d; want 3 and 1", len(spans), maxOverlap(spanList(spans)))
	}
	for _, it := range st.Items {
		if it.StartedAt != "" && (it.Worker != 1 || it.Worktree != "") {
			t.Errorf("%s ran on worker %d in %q; want worker 1 in the repo itself", it.ID, it.Worker, it.Worktree)
		}
	}
}

func TestKilledRunLeavesUsableTrace(t *testing.T) {
	o, dir, _ := newParFixture(t, 100, 3)
	t.Setenv("FAKE_CLAUDE_SLEEP", "3s")
	bin := filepath.Join(scriptCopy(t), "bin", exeName("gocov"))
	cmd := exec.Command(bin, "run", "-config", writeConfig(t, o))
	mustRun(t, "start gocov", cmd.Start())
	evPath := filepath.Join(repoDir(o, "demo"), "events.jsonl")
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		data, _ := os.ReadFile(evPath)
		if strings.Count(string(data), `"ev":"attempt"`) > 0 || strings.Count(string(data), `"ev":"item-start"`) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("no item started within 90s; events:\n%s", data)
		}
	}
	mustRun(t, "kill gocov", cmd.Process.Kill())
	_ = cmd.Wait()
	time.Sleep(5 * time.Second) // let the orphaned fake claude calls finish

	evs := readEvents(t, o)
	if evs[0].Ev != "run-start" || evs[0].Parallel != 3 {
		t.Errorf("first event %+v, want run-start with parallel 3", evs[0])
	}
	for _, e := range evs {
		if e.Ev == "run-end" {
			t.Errorf("a killed run logged run-end: %+v", e)
		}
	}
	st := state(t, o)
	if r := st.Runs[len(st.Runs)-1]; r.Ended != "" {
		t.Errorf("the killed run's record says it ended: %+v", r)
	}
	status, _ := captureOut(t, func() error { return cmdStatus(o) })
	if !strings.Contains(status, "running (or killed") || !strings.Contains(status, "running w") {
		t.Errorf("status should show the unfinished run and its items in progress:\n%s", status)
	}

	t.Setenv("FAKE_CLAUDE_SLEEP", "")
	mustRun(t, "resume", cmdRun(context.Background(), o))
	if st := state(t, o); st.Status != "achieved" {
		t.Errorf("resumed run: status %s (%s), want achieved", st.Status, st.Reason)
	}
	if wt := gitOut(t, dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Errorf("worktrees of the killed run left behind:\n%s", wt)
	}
	itemSpans(t, readEvents(t, o)[len(evs):]) // the resumed run's trace is whole
}

func TestParallelRefusesAllowDeps(t *testing.T) {
	cfg := &Config{Parallel: 2, AllowDeps: true}
	cfg.defaults()
	if err := cmdRun(context.Background(), &opts{cfg: cfg}); err == nil || !strings.Contains(err.Error(), "allow_deps") {
		t.Errorf("parallel 2 with allow_deps: err = %v, want a refusal naming allow_deps", err)
	}
}

func TestRunBacksOffOnRateLimit(t *testing.T) {
	o, _ := newFixture(t, 95)
	reportAndPlan(t, o)
	log := fakeClaude(t)
	src := filepath.Join(t.TempDir(), "grade_test.go")
	mustRun(t, "write test source", os.WriteFile(src, []byte(gradeTest), 0o644))
	t.Setenv("FAKE_CLAUDE_COPY", src)
	t.Setenv("FAKE_CLAUDE_DEST", "calc/grade_test.go")
	t.Setenv("FAKE_CLAUDE_RATELIMIT", filepath.Join(t.TempDir(), "limited-once"))
	old := rateBackoff
	rateBackoff = 10 * time.Millisecond
	defer func() { rateBackoff = old }()

	mustRun(t, "run", cmdRun(context.Background(), o))
	it := item(t, state(t, o), calcItem)
	if n := len(modelCalls(t, log)); n != 2 || it.Status != "done" || it.Attempts != 1 {
		t.Errorf("%d calls, item %s after %d attempt(s); want 2 calls, done after 1 attempt (a rate-limited call is retried, not counted)", n, it.Status, it.Attempts)
	}
	if len(it.AttemptLog) != 2 || !strings.Contains(it.AttemptLog[0].Error, "rate limited") {
		t.Errorf("attempt log %+v should show the rate-limited call, then the retry", it.AttemptLog)
	}
}

func TestRunPausesOnZeroCostErrorMidRun(t *testing.T) {
	o, _ := newFixture(t, 95)
	reportAndPlan(t, o)
	fakeClaude(t)
	t.Setenv("FAKE_CLAUDE_ERROR", "Claude AI usage limit reached")
	err := cmdRun(context.Background(), o)
	st := state(t, o)
	it := item(t, st, calcItem)
	if err == nil || st.Status != "paused-error" || !strings.Contains(st.Reason, "paused: rate or usage limit") || it.Status != "queued" || it.Attempts != 0 {
		t.Errorf("err %v, status %s (%q), item %s after %d attempts; want paused on a rate or usage limit, item still queued and unattempted", err, st.Status, st.Reason, it.Status, it.Attempts)
	}
}

func TestBeginFinishRecordInSessionRun(t *testing.T) {
	o, dir := newFixture(t, 95)
	reportAndPlan(t, o)
	t.Setenv("GOCOV_RUN_ID", "")
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Fatalf("finish exit = %d", code)
	}
	st := state(t, o)
	it := item(t, st, calcItem)
	if len(st.Runs) != 1 || st.Runs[0].Driver != driverSession || it.Run != st.Runs[0].ID || it.FinishedAt == "" || len(it.AttemptLog) != 1 {
		t.Fatalf("runs %+v, item run %q finished %q attempts %+v; want one in-session run holding the item", st.Runs, it.Run, it.FinishedAt, it.AttemptLog)
	}
	var kinds []string
	for _, e := range readEvents(t, o) {
		kinds = append(kinds, e.Ev)
	}
	if got := strings.Join(kinds, " "); got != "run-start wave-start item-start attempt item-end" {
		t.Errorf("events = %s", got)
	}
	mustRun(t, "verify", cmdVerify(context.Background(), o))
	if r := state(t, o).Runs[0]; r.Ended == "" || r.MaxConcurrencyObserved != 1 || r.Items != 1 {
		t.Errorf("verify should close the in-session run: %+v", r)
	}
}

func TestPickWaveSkipsRelatedPackages(t *testing.T) {
	st := &RepoState{Items: []Item{{ID: "a", Status: "queued"}, {ID: "b", Status: "queued"}, {ID: "c", Status: "done"}, {ID: "d", Status: "queued"}, {ID: "e", Status: "queued"}}}
	g := &depGraph{ip: map[string]string{"a": "m/a", "b": "m/b", "c": "m/c", "d": "m/d", "e": "m/e"},
		imports: map[string]map[string]bool{"m/b": {"m/a": true}, "m/a": {}, "m/d": {}, "m/e": {}}}
	cases := []struct {
		n    int
		want string
	}{{1, "a"}, {2, "a d"}, {3, "a d e"}, {9, "a d e"}}
	for _, tc := range cases {
		var ids []string
		for _, i := range pickWave(st, g, tc.n) {
			ids = append(ids, st.Items[i].ID)
		}
		if got := strings.Join(ids, " "); got != tc.want {
			t.Errorf("pickWave(n=%d) = %q, want %q (b imports a; c is done)", tc.n, got, tc.want)
		}
	}
}

func TestMaxOverlap(t *testing.T) {
	at := func(s int) time.Time { return time.Unix(int64(s), 0) }
	cases := []struct {
		name  string
		spans []span
		want  int
	}{
		{"none", nil, 0},
		{"back to back is not overlap", []span{{at(0), at(5)}, {at(5), at(9)}}, 1},
		{"three at once", []span{{at(0), at(10)}, {at(1), at(4)}, {at(2), at(3)}, {at(6), at(7)}}, 3},
	}
	for _, tc := range cases {
		if got := maxOverlap(tc.spans); got != tc.want {
			t.Errorf("%s: maxOverlap = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSkillVersionMatchesSkillMD(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "SKILL.md"))
	mustRun(t, "read SKILL.md", err)
	if !regexp.MustCompile(`(?m)^\s*version: ` + regexp.QuoteMeta(skillVersion) + `\s*$`).Match(data) {
		t.Errorf("SKILL.md's metadata.version differs from skillVersion %s", skillVersion)
	}
}
