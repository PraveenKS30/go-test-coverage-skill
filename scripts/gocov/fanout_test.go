package main

// Tests that drive scripts/fanout.sh and scripts/fanout.ps1 end to end with
// the fake claude (run_test.go). Each runs against a copy of the scripts with
// a freshly built gocov in bin/, so the real scripts/bin is never touched.
// A runner whose shell is missing is skipped.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	scriptsOnce sync.Once
	scriptsDir  string
	scriptsErr  error
)

// scriptCopy returns a directory holding fanout.sh, fanout.ps1 and bin/gocov.
func scriptCopy(t *testing.T) string {
	t.Helper()
	scriptsOnce.Do(func() {
		if scriptsDir, scriptsErr = os.MkdirTemp("", "gocov-scripts-"); scriptsErr != nil {
			return
		}
		for _, f := range []string{"fanout.sh", "fanout.ps1"} {
			data, err := os.ReadFile(filepath.Join("..", f))
			if err != nil {
				scriptsErr = err
				return
			}
			if scriptsErr = os.WriteFile(filepath.Join(scriptsDir, f), data, 0o755); scriptsErr != nil {
				return
			}
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(scriptsDir, "bin", exeName("gocov")), ".").CombinedOutput()
		if err != nil {
			scriptsErr = &fatalErr{"build gocov: " + err.Error() + "\n" + string(out)}
		}
	})
	if scriptsErr != nil {
		t.Fatal(scriptsErr)
	}
	return scriptsDir
}

// gitBash finds bash. On Windows that is Git's bash: the one on PATH may be
// WSL's, which cannot run a Windows gocov.
func gitBash() string {
	if runtime.GOOS != "windows" {
		p, _ := exec.LookPath("bash")
		return p
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return ""
	}
	root := filepath.Join(filepath.FromSlash(strings.TrimSpace(string(out))), "..", "..", "..")
	for _, c := range []string{filepath.Join(root, "bin", "bash.exe"), filepath.Join(root, "usr", "bin", "bash.exe")} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

type scriptRunner struct {
	name string
	cmd  func(dir, cfg string) *exec.Cmd
}

func scriptRunners() []scriptRunner {
	var rs []scriptRunner
	if bash := gitBash(); bash != "" {
		rs = append(rs, scriptRunner{"fanout.sh", func(dir, cfg string) *exec.Cmd {
			return exec.Command(bash, filepath.ToSlash(filepath.Join(dir, "fanout.sh")), "-c", filepath.ToSlash(cfg), "-r", "demo")
		}})
	}
	if runtime.GOOS == "windows" {
		if ps, err := exec.LookPath("powershell"); err == nil {
			rs = append(rs, scriptRunner{"fanout.ps1", func(dir, cfg string) *exec.Cmd {
				return exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(dir, "fanout.ps1"), "-Config", cfg, "-Repo", "demo")
			}})
		}
	}
	return rs
}

func writeConfig(t *testing.T, o *opts) string {
	t.Helper()
	data, err := json.Marshal(o.cfg)
	mustRun(t, "marshal config", err)
	p := filepath.Join(t.TempDir(), "cfg.json")
	mustRun(t, "write config", os.WriteFile(p, data, 0o644))
	return p
}

func TestFanoutScriptsChargeKilledCalls(t *testing.T) {
	runners := scriptRunners()
	if len(runners) == 0 {
		t.Skip("neither Git bash nor PowerShell found")
	}
	for _, r := range runners {
		t.Run(r.name, func(t *testing.T) {
			o, _ := newFixture(t, 95)
			reportAndPlan(t, o)
			log := fakeClaude(t)
			killConfig(t, o)
			cmd := r.cmd(scriptCopy(t), writeConfig(t, o))
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), "charging its cap $0.50") {
				t.Errorf("script output does not log the charge for a killed call:\n%s", out)
			}
			if n := len(modelCalls(t, log)); n != 2 {
				t.Fatalf("%d model calls, want 2 (both attempts); output:\n%s", n, out)
			}
			st := state(t, o)
			it := item(t, st, calcItem)
			if it.Status != "blocked" || !near(it.CostUSD, 1.0) || !near(st.SpendUSD, 1.0) {
				t.Errorf("item %s cost $%.2f, repo spend $%.2f; want blocked, two killed calls charged their $0.50 cap each ($1.00)\n%s", it.Status, it.CostUSD, st.SpendUSD, out)
			}
			if len(st.Runs) == 0 {
				t.Fatalf("the script left no run record\n%s", out)
			}
			run := st.Runs[len(st.Runs)-1]
			if run.Driver != r.name || run.Ended == "" || run.Items != 1 || run.MaxConcurrencyObserved != 1 || it.Run != run.ID {
				t.Errorf("run record %+v (item run %q); want driver %s, ended, holding the item, max concurrency 1", run, it.Run, r.name)
			}
			if a := it.AttemptLog; len(a) != 2 || a[0].Model != "sonnet" || a[1].Model != "opus" || !near(a[0].CapUSD, 0.5) || !near(a[0].CostUSD, 0.5) {
				t.Errorf("attempt log %+v; want both calls with their model, $0.50 cap and charged cost", a)
			}
		})
	}
}
