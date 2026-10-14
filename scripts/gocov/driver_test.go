package main

// Tests of `gocov next-call`, the helper that lets fanout.sh / fanout.ps1 take
// models, caps and timeouts from the config by the same rules as `gocov run`.
// They only read and write state.json: no go, git or model calls.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nextCallOpts saves a state with one item per kind next-call distinguishes.
func nextCallOpts(t *testing.T, repoSpent float64) *opts {
	t.Helper()
	cfg := &Config{OutDir: t.TempDir(), Repos: []RepoCfg{{Name: "demo", Path: t.TempDir()}}}
	cfg.Budget.MaxUSDTotal, cfg.Budget.MaxUSDPerRepo, cfg.Budget.MaxUSDPerItem = 5, 3, 1.5
	cfg.Budget.ItemTimeoutMin, cfg.Budget.TimeLimitMin = 10, 60
	cfg.Models.Default, cfg.Models.Escalate = "small", "big"
	cfg.defaults()
	o := &opts{cfg: cfg}
	st := &RepoState{Name: "demo", Path: cfg.Repos[0].Path, Goal: 80, Status: "in-progress", SpendUSD: repoSpent, Items: []Item{
		{ID: "root:plain", Module: ".", Pkg: "plain", Uncovered: 40, Status: "queued"},
		{ID: "root:hard", Module: ".", Pkg: "hard", Uncovered: 40, Hard: true, Status: "queued"},
		{ID: "root:big", Module: ".", Pkg: "big", Uncovered: 301, Status: "queued"},
		{ID: "svc:api", Module: "svc", Pkg: "svc/api", Uncovered: 40, Status: "queued"},
	}}
	mustRun(t, "saveState", saveState(o, st))
	return o
}

// capture runs cmd and returns its trimmed stdout and exit code.
func capture(t *testing.T, name string, cmd func() error) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	mustRun(t, "pipe", err)
	stdout := os.Stdout
	os.Stdout = w
	exitCode = 0
	cerr := cmd()
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	code := exitCode
	exitCode = 0
	mustRun(t, name, cerr)
	return strings.TrimSpace(string(out)), code
}

// nextCall runs cmdNextCall and returns its stdout and exit code.
func nextCall(t *testing.T, o *opts, id string, attempt int, runSpent float64, elapsed int) (string, int) {
	t.Helper()
	return capture(t, "next-call", func() error { return cmdNextCall(o, id, attempt, runSpent, elapsed) })
}

func TestNextCallFollowsRunRules(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		attempt   int
		repoSpent float64
		runSpent  float64
		want      string // "MODEL CAP TIMEOUT" (the work dir is checked separately)
	}{
		{"default model, per-item cap", "root:plain", 1, 0, 0, "small 1.50 600"},
		{"retry escalates", "root:plain", 2, 0, 0, "big 1.50 600"},
		{"hard package escalates", "root:hard", 1, 0, 0, "big 1.50 600"},
		{"over 300 uncovered escalates", "root:big", 1, 0, 0, "big 1.50 600"},
		{"cap shrinks to repo budget left", "root:plain", 1, 2.2, 0, "small 0.80 600"},
		{"cap shrinks to run budget left", "root:plain", 1, 0, 4.6, "small 0.40 600"},
		{"cap is whole cents, rounded down", "root:plain", 1, 2.95, 0.123, "small 0.05 600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := nextCallOpts(t, tc.repoSpent)
			out, code := nextCall(t, o, tc.id, tc.attempt, tc.runSpent, 0)
			f := strings.Fields(out)
			if code != 0 || len(f) < 4 || strings.Join(f[:3], " ") != tc.want {
				t.Errorf("next-call = %q (exit %d), want %q + work dir, exit 0", out, code, tc.want)
			}
		})
	}
}

func TestNextCallWorkDirIsTheModule(t *testing.T) {
	o := nextCallOpts(t, 0)
	out, _ := nextCall(t, o, "svc:api", 1, 0, 0)
	want := filepath.Join(o.cfg.Repos[0].Path, "svc")
	if f := strings.SplitN(out, " ", 4); len(f) != 4 || f[3] != want {
		t.Errorf("next-call = %q, want work dir %q last", out, want)
	}
}

func TestNextCallStops(t *testing.T) {
	cases := []struct {
		name      string
		attempt   int
		repoSpent float64
		elapsed   int
		dryRun    bool
		code      int
		pause     bool // repo marked paused-budget
	}{
		{"attempts used", 3, 0, 0, false, 5, false},
		{"repo budget used", 1, 2.97, 0, false, 4, true},
		{"repo budget used, dry run saves nothing", 1, 2.97, 0, true, 4, false},
		{"time limit before an item", 1, 0, 3600, false, 4, true},
		{"time limit not checked between attempts", 2, 0, 3600, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := nextCallOpts(t, tc.repoSpent)
			o.dryRun = tc.dryRun
			out, code := nextCall(t, o, "root:plain", tc.attempt, 0, tc.elapsed)
			if code != tc.code {
				t.Errorf("next-call = %q, exit %d, want exit %d", out, code, tc.code)
			}
			if st := state(t, o); (st.Status == "paused-budget") != tc.pause || tc.pause && st.Reason != out {
				t.Errorf("repo status %q reason %q after %q; want paused-budget=%v with the printed reason", st.Status, st.Reason, out, tc.pause)
			}
		})
	}
}
