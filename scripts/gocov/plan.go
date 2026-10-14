package main

// report / plan / status / verify commands and their markdown output.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

func cmdReport(ctx context.Context, o *opts) error {
	for _, rc := range o.repos() {
		if old, err := loadState(o, rc.Name); err == nil && !o.force {
			for _, it := range old.Items {
				if it.Status == "done" || it.Status == "blocked" {
					return fmt.Errorf("repo %q already has progress in %s; use -force to re-baseline (this resets the plan and progress)", rc.Name, statePath(o, rc.Name))
				}
			}
		}
		fmt.Printf("== %s: discovering modules in %s\n", rc.Name, rc.Path)
		mods, err := discoverModules(rc.Path)
		if err != nil || len(mods) == 0 {
			return fmt.Errorf("%s: no go.mod found under %s", rc.Name, rc.Path)
		}
		st := &RepoState{Name: rc.Name, Path: rc.Path, Goal: rc.Goal, GoalBasis: rc.GoalBasis, Scope: rc.Paths, Modules: mods, UncovBlocks: map[string][]UBlock{}}
		for _, m := range mods {
			fmt.Printf("== %s: measuring module %s (go %s) - this can take a while\n", rc.Name, m.Dir, m.GoVersion)
			meas, err := measure(ctx, rc.Path, m, "", rc.TestFlags, filepath.Join(repoDir(o, rc.Name), "logs", "baseline-"+slug(m.Dir)+".log"))
			if err != nil {
				st.Warnings = append(st.Warnings, "UNMEASURED: "+err.Error())
				continue
			}
			if meas.TestExit != 0 {
				st.Warnings = append(st.Warnings, fmt.Sprintf("module %q: go test exited %d at baseline - RED suite (pre-existing failures; not caused by new tests). See %s", m.Dir, meas.TestExit, meas.Log))
			}
			if meas.Fallback {
				st.Warnings = append(st.Warnings, fmt.Sprintf("module %q: -coverpkg=./... failed to build; fell back to per-package coverage. Untested packages are missing from the denominator, so SonarQube would read LOWER than this.", m.Dir))
			}
			st.absorb(meas)
		}
		st.BaseFailing = st.Failing
		applyExcludes(st, rc)
		st.BuildTagTests = countBuildTagged(rc.Path)
		if o.cfg.MaxComplexity > 0 {
			st.ComplexProd, st.ComplexTests = scanRepoComplexity(rc.Path, o.cfg.MaxComplexity, o.cfg.ComplexityMetric)
		}
		st.LintHints = lintComplexityHints(rc.Path)
		st.BaseRaw = sumRows(st.Rows, nil, false)
		st.BaseAdj = sumRows(st.Rows, nil, true)
		st.BaseGoal = st.goalTotals()
		st.Status = "baselined"
		st.Verified = true
		st.log("baseline")
		if err := saveState(o, st); err != nil {
			return err
		}
		if err := writeReport(o, st); err != nil {
			return err
		}
		fmt.Printf("== %s: baseline %.2f%% (goal metric, %s) - report: %s\n", rc.Name, st.BaseGoal.Pct(), st.GoalBasis, filepath.Join(repoDir(o, rc.Name), "COVERAGE_REPORT.md"))
	}
	return writeSummary(o)
}

func applyExcludes(st *RepoState, rc RepoCfg) {
	for i := range st.Rows {
		for _, pat := range rc.Exclude {
			if matchGlob(pat, st.Rows[i].File) {
				st.Rows[i].Class = "generated"
			}
		}
	}
}

func countBuildTagged(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") && strings.Contains(readHead(p, 5), "//go:build") {
			n++
		}
		return nil
	})
	return n
}

func slug(s string) string {
	if s == "." || s == "" {
		return "root"
	}
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", ".", "_", "*", "_", "?", "_", " ", "_").Replace(s)
}

// ------------------------------------------------------------------ plan ----

func cmdPlan(o *opts) error {
	var totalItems, queuedItems int
	for _, rc := range o.repos() {
		st, err := loadState(o, rc.Name)
		if err != nil {
			return err
		}
		buildPlan(o, st)
		// "planned" means nothing written yet (begin/run then require a clean
		// tree); a re-plan after progress must not reset that.
		st.Status = "planned"
		if st.hasProgress() {
			st.Status = "in-progress"
		}
		st.log("planned")
		if err := saveState(o, st); err != nil {
			return err
		}
		if err := writePlan(o, st); err != nil {
			return err
		}
		for _, it := range st.Items {
			totalItems++
			if it.Status == "queued" {
				queuedItems++
			}
		}
	}
	est := estimate(o, queuedItems)
	fmt.Printf("\nPLAN TOTAL: %d candidate packages, %d queued\n", totalItems, queuedItems)
	fmt.Printf("ESTIMATE : ~$%.2f and ~%.0f min of Claude time (calibrate with `gocov run -pilot 3`)\n", est.usd, est.min)
	fmt.Printf("BUDGET   : total $%.2f, per repo $%.2f, per item $%.2f\n", o.cfg.Budget.MaxUSDTotal, o.cfg.Budget.MaxUSDPerRepo, o.cfg.Budget.MaxUSDPerItem)
	fmt.Println("Review PLAN.md in each repo output dir, then approve with `gocov run`.")
	return writeSummary(o)
}

type estimateT struct{ usd, min float64 }

func estimate(o *opts, items int) estimateT {
	return estimateT{float64(items) * o.cfg.EstUSDPerItem, float64(items) * o.cfg.EstMinPerItem}
}

// buildPlan ranks packages by value score and queues only as many as the goal
// plausibly needs; the rest become "reserve" (visible, not executed). Re-running
// it (e.g. after a pilot) keeps done/blocked items as they are - their gain is
// already in the coverage figures - and carries attempts and spend over to the
// packages it re-ranks.
func buildPlan(o *opts, st *RepoState) {
	var settled []Item
	prev := map[string]Item{}
	for _, it := range st.Items {
		if it.Status == "done" || it.Status == "blocked" {
			settled = append(settled, it)
		}
		prev[it.ID] = it
	}
	type key struct{ mod, pkg string }
	groups := map[key]*Item{}
	hardStmts := map[key]int{}
	for _, r := range st.Rows {
		if r.Class != "" || r.Uncovered() <= 0 || !inScope(st.Scope, r.File) {
			continue
		}
		k := key{r.Module, r.Pkg}
		if p, ok := prev[slug(k.mod)+":"+k.pkg]; ok && (p.Status == "done" || p.Status == "blocked") {
			continue
		}
		it := groups[k]
		if it == nil {
			it = &Item{Module: r.Module, Pkg: r.Pkg}
			groups[k] = it
		}
		it.Files = append(it.Files, r.File)
		it.Stmts += r.Stmts
		it.Uncovered += r.Uncovered()
		if r.Hard {
			hardStmts[k] += r.Uncovered()
		}
	}
	var items []Item
	for k, it := range groups {
		if it.Uncovered < o.cfg.MinUncovered {
			continue
		}
		it.Hard = hardStmts[k]*2 > it.Uncovered
		factor, gain := 1.0, 0.7
		if it.Hard {
			factor, gain = 0.5, 0.35
		}
		it.Score = float64(it.Uncovered) * factor
		it.ProjectedGain = int(math.Round(float64(it.Uncovered) * gain))
		sort.Strings(it.Files)
		it.ID = slug(k.mod) + ":" + k.pkg
		it.Status = "reserve"
		if p, ok := prev[it.ID]; ok {
			it.Attempts, it.CostUSD = p.Attempts, p.CostUSD
		}
		items = append(items, *it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].ID < items[j].ID
	})

	g := st.goalTotals()
	target := st.Goal + o.cfg.GoalMargin
	if target > 100 {
		target = 100
	}
	need := int(math.Ceil(target*float64(g.Stmts)/100)) - g.Covered
	proj := g.Covered
	for i := range items {
		if need <= 0 {
			break
		}
		items[i].Status = "queued"
		need -= items[i].ProjectedGain
		proj += items[i].ProjectedGain
	}
	if g.Stmts > 0 {
		st.ProjectedPct = float64(proj) * 100 / float64(g.Stmts)
	}
	if g.Pct() >= st.Goal {
		for i := range items {
			items[i].Status = "reserve"
		}
		st.ProjectedPct = g.Pct()
	}
	st.Items = append(settled, items...)
}

// hasProgress reports whether any item has been worked to a final result.
func (s *RepoState) hasProgress() bool {
	for _, it := range s.Items {
		if it.Status == "done" || it.Status == "blocked" {
			return true
		}
	}
	return false
}

func writePlan(o *opts, st *RepoState) error {
	var sb strings.Builder
	g := st.goalTotals()
	written := "**No tests have been written yet.**"
	if st.hasProgress() {
		written = "Re-plan: done/blocked items are kept as they are and their gain is already in the current figure."
	}
	fmt.Fprintf(&sb, "# Test plan - %s\n\nGenerated %s. %s\n\n", st.Name, time.Now().Format("2006-01-02 15:04"), written)
	fmt.Fprintf(&sb, "- Goal: **%.0f%%** (%s basis%s)\n- Current: **%.2f%%** (%d/%d statements)\n- Plan projects: **~%.1f%%** after queued items (estimate; verified by a fresh full run at the end)\n",
		st.Goal, st.GoalBasis, scopeNote(st), g.Pct(), g.Covered, g.Stmts, st.ProjectedPct)
	queued := 0
	for _, it := range st.Items {
		if it.Status == "queued" {
			queued++
		}
	}
	est := estimate(o, queued)
	fmt.Fprintf(&sb, "- Queued items: **%d** -> estimate ~$%.2f, ~%.0f min (calibrate with `-pilot 3`)\n\n", queued, est.usd, est.min)
	if st.ProjectedPct < st.Goal && g.Pct() < st.Goal {
		fmt.Fprintf(&sb, "> **WARNING - goal likely unreachable with unit tests alone.** Even testing every candidate package the projection is %.1f%% < %.0f%%. Remaining gap is generated code, `main` wiring, hard-to-test I/O, or needs production refactors. Decide whether to lower the goal or approve refactors.\n\n", st.ProjectedPct, st.Goal)
	}
	sb.WriteString("## Work items (ranked by value score)\n\n| # | Status | Item ID | Uncovered | Score | Proj. gain | Notes |\n|---|---|---|---:|---:|---:|---|\n")
	for i, it := range st.Items {
		note := ""
		if it.Hard {
			note = "hard (cgo/exec/syscall) - tests may need seams"
		}
		fmt.Fprintf(&sb, "| %d | %s | `%s` | %d | %.0f | %d | %s |\n", i+1, it.Status, it.ID, it.Uncovered, it.Score, it.ProjectedGain, note)
	}
	sb.WriteString("\n`queued` = will run on approval. `reserve` = visible but not needed for the goal (promote by editing state or raising the goal and re-running `plan`).\n\n")
	sb.WriteString("## Never targeted\n\n")
	sb.WriteString(excludedTable(st))
	return os.WriteFile(filepath.Join(repoDir(o, st.Name), "PLAN.md"), []byte(sb.String()), 0o644)
}

func itemName(it Item) string {
	if it.Module != "." && it.Module != "" {
		return it.Pkg
	}
	return it.Pkg
}

func scopeNote(st *RepoState) string {
	if len(st.Scope) == 0 {
		return ", whole repo"
	}
	return ", scope: " + strings.Join(st.Scope, ", ")
}

func excludedTable(st *RepoState) string {
	cls := map[string]*Totals{}
	for _, r := range st.Rows {
		if r.Class == "" {
			continue
		}
		t := cls[r.Class]
		if t == nil {
			t = &Totals{}
			cls[r.Class] = t
		}
		t.Stmts += r.Stmts
		t.Covered += r.Covered
	}
	if len(cls) == 0 {
		return "(none)\n"
	}
	var sb strings.Builder
	sb.WriteString("| Class | Statements | Covered | Treatment |\n|---|---:|---:|---|\n")
	for _, c := range []string{"generated", "vendor", "main"} {
		t, ok := cls[c]
		if !ok {
			continue
		}
		treat := map[string]string{
			"generated": "never test; removed from the 'adjusted' goal metric (machine-generated only)",
			"vendor":    "never test; removed from the 'adjusted' goal metric",
			"main":      "composition root - not planned; STILL counted in the goal metric",
		}[c]
		fmt.Fprintf(&sb, "| %s | %d | %d | %s |\n", c, t.Stmts, t.Covered, treat)
	}
	return sb.String()
}

// ---------------------------------------------------------------- report ----

func writeReport(o *opts, st *RepoState) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Coverage report - %s\n\nGenerated %s. Path: `%s`\n\n", st.Name, time.Now().Format("2006-01-02 15:04"), st.Path)
	sb.WriteString("## Headline\n\n| Metric | Statements | Covered | Coverage |\n|---|---:|---:|---:|\n")
	row := func(name string, t Totals) {
		fmt.Fprintf(&sb, "| %s | %d | %d | %.2f%% |\n", name, t.Stmts, t.Covered, t.Pct())
	}
	row("Repo - raw (everything)", st.BaseRaw)
	row("Repo - adjusted (generated+vendor removed)", st.BaseAdj)
	if len(st.Scope) > 0 {
		row("Scope ("+strings.Join(st.Scope, ", ")+") - "+st.GoalBasis, st.BaseGoal)
	}
	fmt.Fprintf(&sb, "\nGoal: **%.0f%%** on the **%s** metric%s. Gap to goal: **%.2f points**.\n\n", st.Goal, st.GoalBasis, scopeNote(st), st.Goal-st.BaseGoal.Pct())
	if len(st.Warnings) > 0 {
		sb.WriteString("## Warnings\n\n")
		for _, w := range st.Warnings {
			fmt.Fprintf(&sb, "- %s\n", w)
		}
		sb.WriteString("\n")
	}
	if st.BuildTagTests > 0 {
		fmt.Fprintf(&sb, "> %d `_test.go` file(s) carry `//go:build` tags and are skipped by a default `go test ./...`. Their coverage is NOT in these numbers (pass `test_flags` to include them).\n\n", st.BuildTagTests)
	}
	writeComplexity(&sb, o, st)
	sb.WriteString("## Modules\n\n| Dir | Module | Go | testify |\n|---|---|---|---|\n")
	for _, m := range st.Modules {
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %v |\n", m.Dir, m.Path, m.GoVersion, m.Testify)
	}

	files := append([]FileCov(nil), st.Rows...)
	sort.Slice(files, func(i, j int) bool { return files[i].Uncovered() > files[j].Uncovered() })
	sb.WriteString("\n## Top files by uncovered statements\n\n| File | Stmts | Covered | Uncovered | % | Class |\n|---|---:|---:|---:|---:|---|\n")
	n := 0
	for _, f := range files {
		if n >= 30 || f.Uncovered() == 0 {
			break
		}
		if !inScope(st.Scope, f.File) {
			continue
		}
		pct := 0.0
		if f.Stmts > 0 {
			pct = float64(f.Covered) * 100 / float64(f.Stmts)
		}
		fmt.Fprintf(&sb, "| `%s` | %d | %d | %d | %.1f | %s |\n", f.File, f.Stmts, f.Covered, f.Uncovered(), pct, f.Class)
		n++
	}

	pk := map[string]*Totals{}
	for _, f := range st.Rows {
		if !inScope(st.Scope, f.File) {
			continue
		}
		t := pk[f.Pkg]
		if t == nil {
			t = &Totals{}
			pk[f.Pkg] = t
		}
		t.Stmts += f.Stmts
		t.Covered += f.Covered
	}
	var pkgs []string
	for p := range pk {
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool {
		a, b := pk[pkgs[i]], pk[pkgs[j]]
		return a.Stmts-a.Covered > b.Stmts-b.Covered
	})
	sb.WriteString("\n## Top packages by uncovered statements\n\n| Package | Stmts | Covered | Uncovered | % |\n|---|---:|---:|---:|---:|\n")
	for i, p := range pkgs {
		if i >= 20 {
			break
		}
		t := pk[p]
		fmt.Fprintf(&sb, "| `%s` | %d | %d | %d | %.1f |\n", p, t.Stmts, t.Covered, t.Stmts-t.Covered, t.Pct())
	}

	sb.WriteString("\n## Zero-coverage files (a default profile hides these; SonarQube counts them at 0%)\n\n")
	zero := 0
	for _, f := range files {
		if f.Covered == 0 && f.Stmts > 0 && inScope(st.Scope, f.File) {
			if zero < 40 {
				fmt.Fprintf(&sb, "- `%s` (%d stmts)%s\n", f.File, f.Stmts, classTag(f.Class))
			}
			zero++
		}
	}
	if zero == 0 {
		sb.WriteString("(none)\n")
	} else if zero > 40 {
		fmt.Fprintf(&sb, "- ... and %d more\n", zero-40)
	}
	sb.WriteString("\n## Excluded / special code\n\n" + excludedTable(st))
	sb.WriteString("\n_Next: `gocov plan` to rank and plan the work. Nothing has been written to the repo._\n")
	return os.WriteFile(filepath.Join(repoDir(o, st.Name), "COVERAGE_REPORT.md"), []byte(sb.String()), 0o644)
}

func classTag(c string) string {
	if c == "" {
		return ""
	}
	return " [" + c + "]"
}

func writeSummary(o *opts) error {
	var sb strings.Builder
	sb.WriteString("# Coverage summary - all repos\n\n| Repo | Status | Baseline | Current | Goal | Spend | Items done/queued |\n|---|---|---:|---:|---:|---:|---|\n")
	var all Totals
	for _, rc := range o.cfg.Repos {
		st, err := loadState(o, rc.Name)
		if err != nil {
			continue
		}
		cur := st.goalTotals()
		all.Stmts += cur.Stmts
		all.Covered += cur.Covered
		done, tot := 0, 0
		for _, it := range st.Items {
			if it.Status == "reserve" {
				continue
			}
			tot++
			if it.Status == "done" {
				done++
			}
		}
		fmt.Fprintf(&sb, "| %s | %s | %.2f%% | %.2f%% | %.0f%% | $%.2f | %d/%d |\n", st.Name, st.Status, st.BaseGoal.Pct(), cur.Pct(), st.Goal, st.SpendUSD, done, tot)
	}
	fmt.Fprintf(&sb, "\nCombined (statement-weighted, never an average of percentages): **%.2f%%** (%d/%d)\n", all.Pct(), all.Covered, all.Stmts)
	return os.WriteFile(filepath.Join(o.cfg.OutDir, "SUMMARY.md"), []byte(sb.String()), 0o644)
}

// ---------------------------------------------------------------- status ----

func cmdStatus(o *opts) error {
	fmt.Printf("%-18s %-14s %9s %9s %6s %8s %s\n", "REPO", "STATUS", "BASELINE", "CURRENT", "GOAL", "SPEND", "ITEMS done/queued")
	for _, rc := range o.repos() {
		st, err := loadState(o, rc.Name)
		if err != nil {
			fmt.Printf("%-18s %s\n", rc.Name, "(no state - run report)")
			continue
		}
		cur := st.goalTotals()
		done, tot, blocked := 0, 0, 0
		for _, it := range st.Items {
			if it.Status == "reserve" {
				continue
			}
			tot++
			if it.Status == "done" {
				done++
			}
			if it.Status == "blocked" {
				blocked++
			}
		}
		tag := ""
		if !st.Verified {
			tag = " (estimate)"
		}
		fmt.Printf("%-18s %-14s %8.2f%% %8.2f%% %5.0f%% %8s %d/%d (%d blocked)%s\n", st.Name, st.Status, st.BaseGoal.Pct(), cur.Pct(), st.Goal, fmt.Sprintf("$%.2f", st.SpendUSD), done, tot, blocked, tag)
		if st.Reason != "" {
			fmt.Printf("%-18s   reason: %s\n", "", st.Reason)
		}
	}
	return nil
}

// ---------------------------------------------------------------- verify ----

// fullMeasure re-runs every module with the same flags as the baseline.
func fullMeasure(ctx context.Context, o *opts, st *RepoState, rc RepoCfg, tag string) error {
	var meas []*Measurement
	for _, m := range st.Modules {
		ms, err := measure(ctx, rc.Path, m, "", rc.TestFlags, filepath.Join(repoDir(o, rc.Name), "logs", tag+"-"+slug(m.Dir)+".log"))
		if err != nil {
			return err
		}
		meas = append(meas, ms)
	}
	st.Rows, st.UncovBlocks, st.Failing = nil, nil, nil
	for _, ms := range meas {
		st.absorb(ms)
	}
	applyExcludes(st, rc)
	st.Verified = true
	fullRuns.Add(1)
	return nil
}

// fullRuns counts the fresh full runs this process made (tests check that a
// verified result is reused instead of measured twice).
var fullRuns atomic.Int64

func cmdVerify(ctx context.Context, o *opts) error {
	for _, rc := range o.repos() {
		st, err := loadState(o, rc.Name)
		if err != nil {
			return err
		}
		fmt.Printf("== %s: fresh full run\n", rc.Name)
		if err := fullMeasure(ctx, o, st, rc, "verify"); err != nil {
			return err
		}
		finalize(o, st)
		if err := saveState(o, st); err != nil {
			return err
		}
		if err := writeFinal(o, st); err != nil {
			return err
		}
		fmt.Printf("== %s: %.2f%% vs goal %.0f%% -> %s\n", st.Name, st.goalTotals().Pct(), st.Goal, st.Status)
		if rv := readReview(o, st.Name); rv.Blocking > 0 {
			fmt.Printf("   WARNING: adversarial review has %d unresolved BLOCKING finding(s) - coverage is met but do not call this done until they are fixed (REVIEW.md)\n", rv.Blocking)
		} else if !rv.Exists {
			fmt.Println("   note: adversarial review not run - `gocov review` (recommended before handing off)")
		}
	}
	return writeSummary(o)
}

// newFailures returns packages failing in the latest full run that were not
// already failing at baseline.
func (s *RepoState) newFailures() []string {
	base := map[string]bool{}
	for _, p := range s.BaseFailing {
		base[p] = true
	}
	var out []string
	for _, p := range s.Failing {
		if !base[p] {
			out = append(out, p)
		}
	}
	return out
}

// finalize sets the status from a VERIFIED measurement. A repo is achieved
// only if the goal is met AND no package fails that passed at baseline. Below
// the goal with queued items left (a pilot, a budget pause, a driver stopped
// early) the run is unfinished, not failed.
func finalize(o *opts, st *RepoState) {
	pct := st.goalTotals().Pct()
	counts := map[string]int{}
	for _, it := range st.Items {
		counts[it.Status]++
	}
	reserve, blocked, queued := counts["reserve"], counts["blocked"], counts["queued"]
	switch {
	case len(st.newFailures()) > 0:
		st.Status, st.Reason = "failing-tests", fmt.Sprintf("verified %.2f%% (goal %.0f%%) but tests now fail in %s, which passed at baseline - fix them before calling this done", pct, st.Goal, strings.Join(st.newFailures(), ", "))
	case pct >= st.Goal:
		st.Status, st.Reason = "achieved", fmt.Sprintf("verified %.2f%% >= goal %.0f%% by a fresh full run", pct, st.Goal)
		if len(st.Failing) > 0 {
			st.Reason += fmt.Sprintf("; still failing as at baseline (pre-existing, not caused by new tests): %s", strings.Join(st.Failing, ", "))
		}
	case queued > 0:
		// planned (begin/run still require a clean tree and check out the
		// branch) and paused-* (the cause still holds) stay as they are.
		switch st.Status {
		case "planned", "paused-budget", "paused-error":
		default:
			st.Status = "in-progress"
		}
		st.Reason = fmt.Sprintf("verified %.2f%% < goal %.0f%%; %d queued item(s) not run yet, %d done, %d blocked - re-run `gocov run` or the fan-out script to continue", pct, st.Goal, queued, counts["done"], blocked)
	default:
		st.Status = "not-achieved"
		st.Reason = fmt.Sprintf("verified %.2f%% < goal %.0f%%; %d reserve package(s) untried, %d blocked (see FINAL_REPORT.md)", pct, st.Goal, reserve, blocked)
		if reserve > 0 {
			st.Status = "needs-replan"
		}
	}
	st.log("verified:" + st.Status)
}

func writeFinal(o *opts, st *RepoState) error {
	var sb strings.Builder
	g := st.goalTotals()
	fmt.Fprintf(&sb, "# Final report - %s\n\n**Status: %s** - %s\n\n", st.Name, strings.ToUpper(st.Status), st.Reason)
	fmt.Fprintf(&sb, "| | Before | After |\n|---|---:|---:|\n| Goal metric (%s%s) | %.2f%% | %.2f%% |\n| Raw (everything) | %.2f%% | %.2f%% |\n\nGoal: %.0f%%. Spend: $%.2f.\n\n",
		st.GoalBasis, scopeNote(st), st.BaseGoal.Pct(), g.Pct(), st.BaseRaw.Pct(), sumRows(st.Rows, nil, false).Pct(), st.Goal, st.SpendUSD)
	fmt.Fprintf(&sb, "Adversarial review: %s\n\n", readReview(o, st.Name).line())
	sb.WriteString("## Items\n\n| Package | Status | Pkg before -> after | Stmts gained | Attempts | Cost | Reason / test files |\n|---|---|---|---:|---:|---:|---|\n")
	for _, it := range st.Items {
		if it.Status == "reserve" {
			continue
		}
		extra := it.Reason
		if len(it.TestFiles) > 0 {
			extra += " " + strings.Join(it.TestFiles, ", ")
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %.1f%% -> %.1f%% | %d | %d | $%.2f | %s |\n", itemName(it), it.Status, it.PkgBefore, it.PkgAfter, it.Gained, it.Attempts, it.CostUSD, strings.TrimSpace(extra))
	}
	if len(st.Failing) > 0 {
		sb.WriteString("\n## Failing packages (latest full run)\n\n")
		base := map[string]bool{}
		for _, p := range st.BaseFailing {
			base[p] = true
		}
		for _, p := range st.Failing {
			origin := "NEW - passed at baseline"
			if base[p] {
				origin = "pre-existing - failing at baseline"
			}
			fmt.Fprintf(&sb, "- `%s` (%s)\n", p, origin)
		}
	}
	if len(st.Warnings) > 0 {
		sb.WriteString("\n## Pre-existing warnings\n\n")
		for _, w := range st.Warnings {
			fmt.Fprintf(&sb, "- %s\n", w)
		}
	}
	sb.WriteString("\n## Never targeted\n\n" + excludedTable(st))
	sb.WriteString("\n## History\n\n")
	for _, h := range st.History {
		fmt.Fprintf(&sb, "- %s `%s` -> %.2f%%\n", h.Time, h.Event, h.Pct)
	}
	return os.WriteFile(filepath.Join(repoDir(o, st.Name), "FINAL_REPORT.md"), []byte(sb.String()), 0o644)
}

var _ = path.Base

// cmdMark lets an in-session (interactive) run record per-item progress in the
// same state file the headless driver uses.
func cmdMark(o *opts, item, status, reason string) error {
	rs := o.repos()
	if len(rs) != 1 {
		return fmt.Errorf("mark needs exactly one repo (use -repo NAME)")
	}
	if item == "" || (status != "done" && status != "blocked" && status != "queued") {
		return fmt.Errorf("usage: gocov mark -repo R -item ID -status done|blocked|queued [-reason TEXT]")
	}
	st, err := loadState(o, rs[0].Name)
	if err != nil {
		return err
	}
	for i := range st.Items {
		if st.Items[i].ID == item {
			st.Items[i].Status, st.Items[i].Reason = status, reason
			if st.Status == "planned" {
				st.Status = "in-progress"
			}
			st.log(status + ":" + item)
			return saveState(o, st)
		}
	}
	return fmt.Errorf("no item %q in %s (ids are in PLAN.md / state.json)", item, rs[0].Name)
}

func writeComplexity(sb *strings.Builder, o *opts, st *RepoState) {
	lim := o.cfg.MaxComplexity
	if lim <= 0 {
		return
	}
	fmt.Fprintf(sb, "## Complexity (limit %d per function, metric: %s)\n\n", lim, o.cfg.ComplexityMetric)
	for _, h := range st.LintHints {
		fmt.Fprintf(sb, "- Repo lint/Sonar config: %s - if its limit differs from %d, set `max_complexity` to match.\n", h, lim)
	}
	if len(st.LintHints) > 0 {
		sb.WriteString("\n")
	}
	list := func(title string, fs []FuncComplexity) {
		if len(fs) == 0 {
			fmt.Fprintf(sb, "%s: none over the limit.\n\n", title)
			return
		}
		fmt.Fprintf(sb, "%s: **%d** over the limit (showing up to 15):\n\n", title, len(fs))
		for i, f := range fs {
			if i >= 15 {
				break
			}
			fmt.Fprintf(sb, "- `%s` - cyclomatic %d, cognitive %d\n", fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Name), f.Cyclomatic, f.Cognitive)
		}
		sb.WriteString("\n")
	}
	list("Production code (reported only - fixing it means a refactor, which needs your approval)", st.ComplexProd)
	list("Existing test code (reported only; new tests written by this skill are held to the limit)", st.ComplexTests)
}

func cmdComplexity(o *opts, limit int) error {
	if limit <= 0 {
		limit = o.cfg.MaxComplexity
	}
	for _, rc := range o.repos() {
		prod, tests := scanRepoComplexity(rc.Path, limit, o.cfg.ComplexityMetric)
		fmt.Printf("== %s: limit %d (%s)\n", rc.Name, limit, o.cfg.ComplexityMetric)
		for _, h := range lintComplexityHints(rc.Path) {
			fmt.Printf("   lint config: %s\n", h)
		}
		for _, f := range prod {
			fmt.Printf("   code  %s\n", f)
		}
		for _, f := range tests {
			fmt.Printf("   test  %s\n", f)
		}
		if len(prod)+len(tests) == 0 {
			fmt.Println("   no function is over the limit")
		}
	}
	return nil
}
