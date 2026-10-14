package main

// End-to-end tests of the report -> plan -> begin -> finish -> verify workflow
// on a tiny generated repo. They need `go` and `git` on PATH but no model: the
// "author" is the test itself, writing _test.go files between begin and finish.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture layout: calc has no tests of its own, but api's test covers
// calc.Classify (9 of calc's 15 statements) through -coverpkg. calc.Grade
// (the other 6) is covered by nothing.
var fixture = map[string]string{
	"go.mod": "module example.com/demo\n\ngo 1.22\n",
	"calc/calc.go": `package calc

func Classify(n int) string {
	if n < 0 {
		return "neg"
	}
	if n == 0 {
		return "zero"
	}
	if n < 10 {
		return "small"
	}
	if n < 100 {
		return "medium"
	}
	return "large"
}

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
`,
	"api/api.go": `package api

import "example.com/demo/calc"

func Describe(n int) string { return "value is " + calc.Classify(n) }
`,
	"api/api_test.go": `package api

import "testing"

func TestDescribe(t *testing.T) {
	for in, want := range map[int]string{-1: "value is neg", 0: "value is zero", 5: "value is small", 50: "value is medium", 500: "value is large"} {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%d) = %q, want %q", in, got, want)
		}
	}
}
`,
	"util/util.go": "package util\n\nfunc Double(n int) int { return n * 2 }\n",
	"util/util_test.go": `package util

import "testing"

func TestDouble(t *testing.T) {
	if Double(2) != 4 {
		t.Error("Double(2) != 4")
	}
}
`,
}

// Re-covers only what api's test already covers: zero real gain.
const classifyTest = `package calc

import "testing"

func TestClassify(t *testing.T) {
	for in, want := range map[int]string{-1: "neg", 0: "zero", 5: "small", 50: "medium", 500: "large"} {
		if got := Classify(in); got != want {
			t.Errorf("Classify(%d) = %q, want %q", in, got, want)
		}
	}
}
`

// Covers all of Grade: 6 statements nobody covered before.
const gradeTest = `package calc

import "testing"

func TestGrade(t *testing.T) {
	for in, want := range map[int]string{95: "A", 85: "B", 75: "C", 65: "D", 10: "F"} {
		if got := Grade(in); got != want {
			t.Errorf("Grade(%d) = %q, want %q", in, got, want)
		}
	}
}
`

const calcItem = "root:calc"

func newFixture(t *testing.T, goal float64) (*opts, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go test on a generated repo; skipped with -short")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	dir := t.TempDir()
	for name, body := range fixture {
		writeFile(t, dir, name, body)
	}
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "fixture")
	cfg := &Config{OutDir: t.TempDir(), MinUncovered: 1, Repos: []RepoCfg{{Name: "demo", Path: dir, Goal: goal}}}
	cfg.defaults()
	return &opts{cfg: cfg}, dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := git(dir, args...); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mustRun(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func state(t *testing.T, o *opts) *RepoState {
	t.Helper()
	st, err := loadState(o, "demo")
	mustRun(t, "loadState", err)
	return st
}

func item(t *testing.T, st *RepoState, id string) Item {
	t.Helper()
	it, err := itemByID(st, id)
	mustRun(t, "itemByID", err)
	return *it
}

// finish runs cmdFinish and returns its exit code (0 accepted, 3 not accepted).
func finish(t *testing.T, o *opts, id string) int {
	t.Helper()
	exitCode = 0
	mustRun(t, "finish", cmdFinish(context.Background(), o, id, false, 0))
	code := exitCode
	exitCode = 0
	return code
}

func reportAndPlan(t *testing.T, o *opts) {
	t.Helper()
	mustRun(t, "report", cmdReport(context.Background(), o))
	mustRun(t, "plan", cmdPlan(o))
}

func TestFinishCreditsOnlyNewlyCoveredBlocks(t *testing.T) {
	o, dir := newFixture(t, 95)
	ctx := context.Background()
	reportAndPlan(t, o)
	base := state(t, o).goalTotals()
	if base.Stmts != 17 || base.Covered != 11 {
		t.Fatalf("baseline = %d/%d, want 11/17 (fixture changed?)", base.Covered, base.Stmts)
	}

	mustRun(t, "begin", cmdBegin(ctx, o, calcItem))
	writeFile(t, dir, "calc/classify_test.go", classifyTest)
	if code := finish(t, o, calcItem); code != 3 {
		t.Fatalf("a test that only re-covers api's coverage must be rejected, finish exit = %d", code)
	}
	if got := state(t, o).goalTotals(); got != base {
		t.Errorf("estimate moved on a rejected item: %+v, want %+v", got, base)
	}

	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Fatalf("Grade test must be accepted, finish exit = %d", code)
	}
	st := state(t, o)
	if it := item(t, st, calcItem); it.Status != "done" || it.Gained != 6 {
		t.Errorf("item = %s with %d gained, want done with 6 (Grade only)", it.Status, it.Gained)
	}
	estimate := st.goalTotals()

	mustRun(t, "verify", cmdVerify(ctx, o))
	if verified := state(t, o).goalTotals(); estimate != verified {
		t.Errorf("estimate %+v != verified %+v", estimate, verified)
	}
}

func TestPromptListsOnlyLinesUncoveredInGoalMetric(t *testing.T) {
	o, _ := newFixture(t, 95)
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	data, err := os.ReadFile(filepath.Join(repoDir(o, "demo"), "work", slug(calcItem)+".prompt.md"))
	mustRun(t, "read prompt", err)
	// Classify (lines 3-17) is covered through api; only Grade's blocks (20-30) remain.
	if p := string(data); !strings.Contains(p, "- calc.go: 20, 22, 24, 26, 28, 30\n") {
		t.Errorf("prompt should list only Grade's uncovered lines; got:\n%s", p)
	}
}

func TestReplanKeepsSettledItems(t *testing.T) {
	o, dir := newFixture(t, 95)
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(context.Background(), o, calcItem))
	writeFile(t, dir, "calc/grade_test.go", gradeTest)
	if code := finish(t, o, calcItem); code != 0 {
		t.Fatalf("finish exit = %d", code)
	}
	before := item(t, state(t, o), calcItem)

	mustRun(t, "re-plan", cmdPlan(o))
	st := state(t, o)
	after := item(t, st, calcItem)
	if after.Status != "done" || after.Attempts != before.Attempts || after.Gained != before.Gained || strings.Join(after.TestFiles, ",") != "calc/grade_test.go" {
		t.Errorf("re-plan changed a done item:\nbefore %+v\nafter  %+v", before, after)
	}
	if st.Status != "in-progress" {
		t.Errorf("status after re-plan with progress = %q, want in-progress (planned would make begin refuse the uncommitted tests)", st.Status)
	}
	if len(st.Items) != 1 {
		t.Errorf("done package was queued again: %d items", len(st.Items))
	}
}

func TestVerifyStatusTracksFailingPackages(t *testing.T) {
	o, dir := newFixture(t, 50)
	ctx := context.Background()
	writeFile(t, dir, "util/util_test.go", "package util\n\nimport \"testing\"\n\nfunc TestRed(t *testing.T) { t.Error(\"pre-existing failure\") }\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "red util")
	reportAndPlan(t, o)

	if got := strings.Join(state(t, o).BaseFailing, ","); got != "example.com/demo/util" {
		t.Errorf("baseline failing = %q, want only example.com/demo/util", got)
	}
	mustRun(t, "verify", cmdVerify(ctx, o))
	st := state(t, o)
	if st.Status != "achieved" || !strings.Contains(st.Reason, "pre-existing") || !strings.Contains(st.Reason, "example.com/demo/util") {
		t.Errorf("pre-existing failure only: status %q reason %q; want achieved, naming util as pre-existing", st.Status, st.Reason)
	}

	writeFile(t, dir, "api/broken_test.go", "package api\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Error(\"new failure\") }\n")
	mustRun(t, "verify", cmdVerify(ctx, o))
	st = state(t, o)
	if st.Status != "failing-tests" || !strings.Contains(st.Reason, "example.com/demo/api") || strings.Contains(st.Reason, "example.com/demo/util") {
		t.Errorf("new failure: status %q reason %q; want failing-tests naming only api", st.Status, st.Reason)
	}
}

func TestProductionEditsNeverSurviveFinish(t *testing.T) {
	o, dir := newFixture(t, 95)
	ctx := context.Background()
	reportAndPlan(t, o)
	mustRun(t, "begin", cmdBegin(ctx, o, calcItem))
	calc := filepath.Join(dir, "calc", "calc.go")
	orig, err := os.ReadFile(calc)
	mustRun(t, "read calc.go", err)
	mutant := strings.Replace(string(orig), `return "F"`, `return "MUTANT"`, 1)

	// A leftover mutation blocks the next begin instead of being snapshotted.
	writeFile(t, dir, "calc/calc.go", mutant)
	if err := cmdBegin(ctx, o, calcItem); err == nil || !strings.Contains(err.Error(), "calc/calc.go") {
		t.Fatalf("begin with a dirty production file: err = %v, want refusal naming calc/calc.go", err)
	}

	// A staged mutation plus a test that locks it in: finish must restore HEAD,
	// so the test fails against the real code and is not accepted.
	mustGit(t, dir, "add", "calc/calc.go")
	writeFile(t, dir, "calc/grade_test.go", strings.Replace(gradeTest, `10: "F"`, `10: "MUTANT"`, 1))
	if code := finish(t, o, calcItem); code != 3 {
		t.Errorf("test written against a mutant was accepted, finish exit = %d", code)
	}
	if out, _ := git(dir, "status", "--porcelain", "--", "calc/calc.go"); strings.TrimSpace(out) != "" {
		t.Errorf("calc.go not restored to HEAD after finish: %q", out)
	}
}

func TestFailingPkgsParsesOnlyPackageLines(t *testing.T) {
	log := filepath.Join(t.TempDir(), "go-test.log")
	body := "--- FAIL: TestX (0.00s)\nFAIL\ncoverage: 40.0% of statements\nFAIL\texample.com/a\t0.1s\nok  \texample.com/b\t0.1s\nFAIL\texample.com/c [build failed]\nFAIL\texample.com/a\t0.2s\nFAIL\n"
	if err := os.WriteFile(log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(failingPkgs(log), ","); got != "example.com/a,example.com/c" {
		t.Errorf("failingPkgs = %q, want example.com/a,example.com/c", got)
	}
}
