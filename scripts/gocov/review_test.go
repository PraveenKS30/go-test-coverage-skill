package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadReviewCountsFindingsAndVerdict(t *testing.T) {
	o := &opts{cfg: &Config{OutDir: t.TempDir()}}
	_ = os.MkdirAll(repoDir(o, "r"), 0o755)

	if got := readReview(o, "r"); got.Exists {
		t.Fatalf("no file must mean not run, got %+v", got)
	}
	body := "Verdict: CHANGES NEEDED\n\n### BLOCKING - WEAK - a_test.go:10\nx\nFix: y\n\n### blocking - JUNK - b_test.go:3\nz\n\n### OPTIONAL - UNPROTECTED - c_test.go:1\nw\n\nplain text mentioning BLOCKING is not a finding\n"
	if err := os.WriteFile(reviewFile(o, "r"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readReview(o, "r")
	if !got.Exists || got.Verdict != "CHANGES NEEDED" {
		t.Errorf("verdict = %+v", got)
	}
	// Headings are matched case-sensitively on BLOCKING; the lowercase one and
	// the prose line must not count.
	if got.Blocking != 1 || got.Optional != 1 {
		t.Errorf("blocking=%d optional=%d, want 1 and 1", got.Blocking, got.Optional)
	}
	if !strings.Contains(got.line(), "1 blocking") {
		t.Errorf("line = %q", got.line())
	}
}

func TestReviewPromptListsOnlyDoneItemsAndForbidsEdits(t *testing.T) {
	o := &opts{cfg: &Config{OutDir: t.TempDir(), MaxComplexity: 15, ComplexityMetric: "both"}}
	st := &RepoState{Name: "r", Goal: 80, GoalBasis: "adjusted", Items: []Item{
		{Pkg: "pkg/a", Status: "done", TestFiles: []string{"pkg/a/a_test.go"}, Gained: 12, Attempts: 1},
		{Pkg: "pkg/b", Status: "blocked", TestFiles: []string{"pkg/b/b_test.go"}},
		{Pkg: "pkg/c", Status: "queued"},
	}}
	if files := reviewTestFiles(st); len(files) != 1 || files[0] != "pkg/a/a_test.go" {
		t.Fatalf("files to review = %v", files)
	}
	p := buildReviewPrompt(o, RepoCfg{Path: filepath.Join("x", "r")}, st)
	for _, want := range []string{"pkg/a/a_test.go", "Do NOT edit any file", "Verdict: PASS", "### BLOCKING", "report none rather than padding"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "pkg/b/b_test.go") || strings.Contains(p, "pkg/c") {
		t.Error("prompt must only cover done items")
	}
}
