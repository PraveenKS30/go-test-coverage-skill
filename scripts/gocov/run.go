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
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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

// budget is the run-wide spend and time limit, shared by the repos run in
// parallel (jobs > 1) and the items run in parallel (parallel > 1). A call's
// cap is reserved before it starts and settled to its real cost after, so two
// calls can never be granted the same dollars. It also holds the rate-limit
// back-off that every worker waits out.
type budget struct {
	mu         sync.Mutex
	spent      float64 // settled: what finished calls cost
	reserved   float64 // caps of calls still running
	deadline   time.Time
	cfg        *Config
	pauseUntil time.Time // rate-limit back-off: no call starts before this
}

// repoAcct is one repo's spend in this run, guarded by budget.mu: its items
// may run in parallel, so its cap is reserved and settled like the run's.
type repoAcct struct{ spent, reserved float64 }

// reserveFor reserves the next call's cap against the run and the repo.
func (b *budget) reserveFor(r *repoAcct) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	left := callCap(b.cfg, r.spent+r.reserved, b.spent+b.reserved)
	if left < minCallUSD {
		return left, false
	}
	b.reserved += left
	r.reserved += left
	return left, true
}

// settleFor releases a reservation made by reserveFor and charges the cost.
func (b *budget) settleFor(r *repoAcct, reserved, cost float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserved -= reserved
	b.spent += cost
	r.reserved -= reserved
	r.spent += cost
}

// nextCap is the cap a call starting now would get.
func (b *budget) nextCap(r *repoAcct) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return callCap(b.cfg, r.spent+r.reserved, b.spent+b.reserved)
}

func (b *budget) repoSpent(r *repoAcct) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return r.spent
}

// backoff holds back every new call for d (a rate limit is account-wide).
func (b *budget) backoff(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until := time.Now().Add(d); until.After(b.pauseUntil) {
		b.pauseUntil = until
	}
}

// waitGate waits out a back-off another worker started.
func (b *budget) waitGate(ctx context.Context) {
	for {
		b.mu.Lock()
		d := time.Until(b.pauseUntil)
		b.mu.Unlock()
		if d <= 0 {
			return
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
}

// rateBackoff is the first rate-limit back-off; it doubles up to maxBackoffs
// times for one attempt, then the repo pauses.
var rateBackoff = 30 * time.Second

const maxBackoffs = 3

func (b *budget) runSpent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// committed is the settled spend plus the caps of calls still running: the
// part of max_usd_total a new call cannot be granted.
func (b *budget) committed() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent + b.reserved
}

// reserve computes the next call's cap against the committed run spend and a
// repo spend of repoSpent and, if it is at least minCallUSD, holds it until
// settle. It returns the cap and whether it was reserved.
func (b *budget) reserve(repoSpent float64) (float64, bool) {
	return b.reserveFor(&repoAcct{spent: repoSpent})
}

// settle releases a reservation and charges what the call actually cost.
func (b *budget) settle(reserved, cost float64) {
	b.settleFor(&repoAcct{}, reserved, cost)
}
func (b *budget) timeUp() bool {
	return !b.deadline.IsZero() && time.Now().After(b.deadline)
}

// runTrack is what the repos of one `gocov run` share besides the budget.
type runTrack struct {
	id, driver string
	pool       *workerPool // the global cap of parallel items, across all repos
}

func cmdRun(ctx context.Context, o *opts) error {
	if o.cfg.Parallel > 1 && o.cfg.AllowDeps {
		return fmt.Errorf("allow_deps cannot be combined with parallel > 1: items in separate worktrees editing go.mod/go.sum would conflict when merged; set parallel to 1 or allow_deps to false")
	}
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
	rt := &runTrack{id: newRunID(), driver: driverRun, pool: newPool(o.cfg.Parallel)}
	if o.cfg.Parallel > 1 && !o.dryRun {
		defer throttleGo(o.cfg.Parallel)()
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
			if err := runRepo(ctx, o, rc, b, rt); err != nil {
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

func runRepo(ctx context.Context, o *opts, rc RepoCfg, b *budget, rt *runTrack) (err error) {
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
	parallel := o.cfg.Parallel > 1
	if !git && !o.dryRun && parallel {
		return fmt.Errorf("%s is not a git repo; parallel > 1 needs git (each item works in its own worktree)", rc.Path)
	}
	if !git && !rc.AllowNonGit && !o.dryRun {
		return fmt.Errorf("%s is not a git repo; refusing to let an agent edit it without a way to revert (set allow_non_git:true to override)", rc.Path)
	}
	if git && !o.dryRun && (st.Status == "planned" || parallel) {
		if dirty := dirtyFiles(rc.Path, o.cfg.OutDir); len(dirty) > 0 {
			if parallel {
				return fmt.Errorf("working tree has uncommitted changes (%d files, e.g. %s); parallel > 1 merges each item as a commit onto the work branch, so commit or stash them first", len(dirty), dirty[0])
			}
			return fmt.Errorf("working tree has uncommitted changes (%d files, e.g. %s); commit or stash first so generated work is cleanly separable", len(dirty), dirty[0])
		}
		if st.Status == "planned" && rc.Branch != "" {
			if err := gitCheckoutBranch(rc.Path, rc.Branch); err != nil {
				return err
			}
		}
	}
	acct := &repoAcct{spent: st.SpendUSD}
	if o.dryRun {
		return runDryRun(ctx, o, rc, st, b, acct)
	}

	now := time.Now()
	st.resetInterrupted()
	st.addRun(o, rt.id, rt.driver, o.cfg.Parallel, now)
	ev := newEventLog(o, rc.Name, rt.id)
	ev.emitAt(now, "run-start", runStartFields(st.runByID(rt.id)))
	if err := saveState(o, st); err != nil {
		return err
	}
	ended := false
	defer func() { // a paused or failed run still gets its end recorded
		if !ended {
			st.endRun(rt.id, ev, time.Now())
			_ = saveState(o, st)
		}
	}()

	var fresh bool // st holds a full run made after the last item: close-out reuses it
	if parallel {
		fresh, err = runParallel(ctx, o, rc, st, b, acct, rt, ev)
	} else {
		fresh, err = runSequential(ctx, o, rc, st, b, acct, git, rt, ev)
	}
	if err != nil {
		return err
	}

	if st.Status == "in-progress" && st.Reason == "" || st.Status == "planned" {
		// queue exhausted or goal estimate hit: close out with a verified figure
		if !fresh {
			if err := fullMeasure(ctx, o, st, rc, "verify"); err != nil {
				return err
			}
		}
		finalize(o, st)
	} else if !fresh && (st.Status == "paused-budget" || strings.HasPrefix(st.Reason, "pilot")) {
		_ = fullMeasureQuiet(ctx, o, st, rc)
	}
	st.log("run:" + st.Status)
	st.endRun(rt.id, ev, time.Now())
	ended = true
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

// runDryRun prints the claude command and prompt of the first queued item.
// Nothing is saved.
func runDryRun(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, b *budget, acct *repoAcct) error {
	for i := range st.Items {
		it := &st.Items[i]
		if it.Status != "queued" {
			continue
		}
		left := b.nextCap(acct)
		if left < minCallUSD {
			say("[%s] DRY RUN: %s", rc.Name, budgetReason(o.cfg, left, st.SpendUSD, b.committed()))
			return nil
		}
		prompt, err := dryRunPrompt(ctx, o, rc, st, it)
		if err != nil {
			return err
		}
		say("---- DRY RUN (%s, item %s) ----\ncommand: %s\n\n%s", rc.Name, it.ID, claudeArgsString(o, modelFor(o, it, 1), left), prompt)
		return nil
	}
	say("[%s] DRY RUN: no queued item", rc.Name)
	return nil
}

// startItem records that an item started (state and events.jsonl).
func startItem(it *Item, run string, wave, worker int, wt, branch string, ev *eventLog, at time.Time) {
	it.Run, it.StartedAt, it.FinishedAt = run, ts(at), ""
	it.Wave, it.Worker, it.Worktree, it.Branch, it.MergeSHA = wave, worker, wt, branch, ""
	ev.emitAt(at, "item-start", map[string]any{"item": it.ID, "worker": worker, "wave": wave, "worktree": wt, "branch": branch})
}

func endItemEvent(ev *eventLog, it *Item, status string) {
	ev.emit("item-end", map[string]any{"item": it.ID, "worker": it.Worker, "status": status, "gained": it.Gained,
		"cost_usd": it.CostUSD, "reason": it.Reason})
}

// runSequential works the queue one item at a time in the repo's own tree
// (parallel = 1): each item is a wave of one, worker 1.
func runSequential(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, b *budget, acct *repoAcct, git bool, rt *runTrack, ev *eventLog) (bool, error) {
	processed, wave := 0, 0
	fresh := false
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
		if left := b.nextCap(acct); left < minCallUSD {
			st.Status, st.Reason = "paused-budget", budgetReason(o.cfg, left, b.repoSpent(acct), b.committed())
			break
		}
		st.Status = "in-progress"
		st.Reason = ""
		processed++
		fresh = false
		wave++
		now := time.Now()
		ev.emitAt(now, "wave-start", map[string]any{"wave": wave, "items": []string{it.ID}})
		startItem(it, rt.id, wave, 1, "", currentBranch(rc.Path), ev, now)
		_ = saveState(o, st)
		head := headSHA(rc.Path)
		rcI := rc
		rcI.tag = rc.Name + " w1"
		ferr := runItem(ctx, o, rcI, st, it, git, itemEnv{b: b, acct: acct, ev: ev, worker: 1})
		it.FinishedAt = ts(time.Now())
		if ferr != nil {
			endItemEvent(ev, it, "paused")
			st.Status, st.Reason = "paused-error", ferr.Error()
			_ = saveState(o, st)
			say("[%s] PAUSED: %s", rc.Name, st.Reason)
			return fresh, ferr
		}
		if h := headSHA(rc.Path); it.Status == "done" && git && h != head {
			it.MergeSHA = h
			ev.emit("merge", map[string]any{"item": it.ID, "sha": h})
		}
		endItemEvent(ev, it, it.Status)
		st.Verified = false
		_ = saveState(o, st)

		if st.goalTotals().Pct() >= st.Goal {
			say("[%s] estimate reached goal - verifying with a fresh full run...", rc.Name)
			if err := fullMeasure(ctx, o, st, rc, "verify"); err != nil {
				return fresh, err
			}
			fresh = true
			ev.emit("checkpoint", map[string]any{"wave": wave, "verified_pct": round2(st.goalTotals().Pct()), "goal": st.Goal})
			if st.goalTotals().Pct() >= st.Goal {
				break
			}
			say("[%s] verified %.2f%% < goal %.0f%% - estimate was optimistic, continuing", rc.Name, st.goalTotals().Pct(), st.Goal)
		}
	}
	return fresh, nil
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

// minCallUSD is the smallest per-call cap worth starting a claude call with.
const minCallUSD = 0.05

// callCap is the --max-budget-usd for the next claude call: the smallest of
// the per-item cap and what is left of the repo and run budgets, rounded down
// to whole cents (so $0.25 - $0.20 is $0.05, not 0.0499...).
func callCap(cfg *Config, repoSpent, runSpent float64) float64 {
	return math.Floor(min3(cfg.Budget.MaxUSDPerRepo-repoSpent, cfg.Budget.MaxUSDTotal-runSpent, cfg.Budget.MaxUSDPerItem)*100+1e-6) / 100
}

func budgetReason(cfg *Config, left, repoSpent, runSpent float64) string {
	return fmt.Sprintf("budget cap reached (next call allowed $%.2f; per-item cap $%.2f, repo spend $%.2f/$%.2f, run spend $%.2f/$%.2f); raise the cap in the config and re-run to resume", left, cfg.Budget.MaxUSDPerItem, repoSpent, cfg.Budget.MaxUSDPerRepo, runSpent, cfg.Budget.MaxUSDTotal)
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

// itemEnv is what runItem needs besides the item: the budget and the repo's
// account, the event log and the worker slot it runs in.
type itemEnv struct {
	b      *budget
	acct   *repoAcct
	ev     *eventLog
	worker int
	pre    float64 // cap already reserved for attempt 1 when the item was dispatched; 0 = reserve here
}

func runItem(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, git bool, env itemEnv) error {
	b, pre := env.b, env.pre
	defer func() {
		if pre > 0 { // dispatched, but no call was made
			b.settleFor(env.acct, pre, 0)
		}
	}()
	if git {
		if err := requireCleanProduction(o, rc); err != nil {
			return &fatalErr{err.Error()}
		}
	}
	if err := ensureBlocks(ctx, o, st, rc); err != nil {
		return err
	}
	if err := preMeasure(ctx, o, rc, st, it); err != nil {
		say("[%s] BLOCKED %s: %s", rc.label(), it.ID, it.Reason)
		return nil
	}
	snap := snapshot(rc.Path, o.cfg.OutDir)
	failNote := ""
	backoffs := 0

	for attempt := 1; attempt <= o.cfg.MaxAttempts; attempt++ {
		b.waitGate(ctx)
		left, ok := pre, pre > 0
		if pre > 0 {
			pre = 0
		} else {
			left, ok = b.reserveFor(env.acct)
		}
		if !ok {
			it.Reason = "stopped: budget exhausted mid-item"
			break
		}
		it.Attempts = attempt
		model := modelFor(o, it, attempt)
		say("[%s] %s attempt %d/%d (model %s, cap $%.2f)", rc.label(), it.ID, attempt, o.cfg.MaxAttempts, model, left)
		started := time.Now()
		cost, cerr := callClaude(ctx, o, rc, st, it, model, left, failNote)
		it.CostUSD += cost
		st.SpendUSD += cost
		b.settleFor(env.acct, left, cost)
		recordAttempt(it, env, attempt, model, left, cost, started, cerr)
		var rl *rateLimitErr
		if errors.As(cerr, &rl) {
			if backoffs++; backoffs > maxBackoffs {
				revertAll(rc.Path, snap, o.cfg.OutDir)
				return &fatalErr{fmt.Sprintf("paused: rate or usage limit - claude -p was still rate limited after %d back-offs (%s); re-run later to resume", maxBackoffs, rl.msg)}
			}
			d := rateBackoff << (backoffs - 1)
			say("[%s]   rate limited (%s): all workers back off %v, then attempt %d is retried", rc.label(), rl.msg, d, attempt)
			b.backoff(d)
			attempt-- // a rate-limited call did no work: it does not use up an attempt
			continue
		}
		if fe, ok := cerr.(*fatalErr); ok {
			it.Attempts--
			revertAll(rc.Path, snap, o.cfg.OutDir)
			return fe
		}
		if cerr != nil {
			failNote = "the previous attempt failed to complete: " + cerr.Error()
		}

		if ok, fn := verifyItem(ctx, o, rc, st, it, snap, git); ok {
			say("[%s]   item $%.2f, repo $%.2f/%.0f, run $%.2f/%.0f", rc.label(), it.CostUSD, b.repoSpent(env.acct), o.cfg.Budget.MaxUSDPerRepo, b.runSpent(), o.cfg.Budget.MaxUSDTotal)
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
	say("[%s] BLOCKED %s: %s", rc.label(), it.ID, it.Reason)
	st.log("blocked:" + it.ID)
	return nil
}

// recordAttempt adds a claude call to the item's attempt log and events.jsonl.
func recordAttempt(it *Item, env itemEnv, n int, model string, capUSD, cost float64, started time.Time, err error) {
	a := Attempt{N: n, Run: env.ev.runID(), Worker: env.worker, Model: model, CapUSD: capUSD, CostUSD: cost, Started: ts(started), Ended: ts(time.Now())}
	if err != nil {
		a.Error = firstLine(err.Error())
	}
	it.AttemptLog = append(it.AttemptLog, a)
	env.ev.emit("attempt", map[string]any{"item": it.ID, "worker": env.worker, "attempt": n, "model": model, "cap_usd": capUSD,
		"cost_usd": cost, "started": a.Started, "error": a.Error})
}

// dryRunPrompt checks that the item's package builds and returns the task
// prompt a real run would send. It changes nothing on disk except the log, and
// st only in memory (callers do not save it).
func dryRunPrompt(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item) (string, error) {
	if err := ensureBlocks(ctx, o, st, rc); err != nil {
		return "", err
	}
	if _, err := measureItem(ctx, o, rc, st, it, "dry"); err != nil {
		return "", err
	}
	it.PkgBefore = pctOf(st.Rows, it.Files)
	return buildPrompt(o, rc, st, it, ""), nil
}

// preMeasure measures the item's package before anything is written. A
// package that does not build or test at baseline cannot be worked on: it is
// marked blocked so no driver retries it forever.
func preMeasure(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item) error {
	before, err := measureItem(ctx, o, rc, st, it, "pre")
	if err != nil {
		it.Status, it.Reason = "blocked", "package does not build/test at baseline: "+err.Error()
		st.log("blocked:" + it.ID)
		return err
	}
	it.PreCovered = coverOf(before.Rows, it.Files)
	recordPreComplexity(o, rc, it)
	it.PkgBefore = pctOf(st.Rows, it.Files)
	return nil
}

// verifyItem applies the guard rails and acceptance tests to whatever the
// author (headless claude, or the in-session agent) left in the tree. On
// success it records the item as done. On failure it returns feedback for the
// next attempt and leaves the tree as is (the caller decides to retry/revert).
func verifyItem(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, snap map[string]string, git bool) (bool, string) {
	if git {
		if bad := revertForbidden(rc.Path, o.cfg.OutDir, o.cfg.AllowDeps); len(bad) > 0 {
			say("[%s]   reverted forbidden edits: %s", rc.label(), strings.Join(bad, ", "))
			st.Warnings = append(st.Warnings, fmt.Sprintf("%s: non-test files were edited %v; reverted by gocov", it.ID, bad))
		}
	}
	after, merr := measureItem(ctx, o, rc, st, it, "post")
	if merr != nil || after.TestExit != 0 {
		return false, "go test failed after your changes. Fix the tests (do not weaken them). Tail of log:\n" + tailFile(logOf(after, merr), 40)
	}
	modAbs := filepath.Join(rc.Path, filepath.FromSlash(it.Module))
	rel := pkgArg(it)
	fargs := append([]string{"-count=3", "-shuffle=on"}, rc.TestFlags...)
	flog := filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-flake.log")
	if code := goTest(ctx, modAbs, append(fargs, rel), flog); code != 0 {
		return false, "tests are FLAKY, order-dependent or fail on repeat (-count=3 -shuffle=on). Remove time/ordering/shared-state dependence: each test must pass alone and in any order. Tail:\n" + tailFile(flog, 30)
	}
	var newTests []string
	if git {
		newTests = changedTestFiles(rc.Path, snap, o.cfg.OutDir)
		vlog := filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-vet.log")
		if bad := vetFindings(ctx, rc, modAbs, rel, newTests, vlog); len(bad) > 0 {
			return false, "go vet flags your new test files. Fix every finding (do not suppress vet):\n" + strings.Join(bad, "\n")
		}
	}
	if lim := o.cfg.MaxComplexity; lim > 0 && git {
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
	newly := newlyCovered(st, it.Files, after)
	gain := 0
	for _, bs := range newly {
		for _, b := range bs {
			gain += b.Stmts
		}
	}
	if gain < 1 {
		return false, fmt.Sprintf("tests pass but covered 0 new statements in %v. Statements other packages' tests already cover do not count; target the uncovered ranges listed.", it.Files)
	}
	it.Gained = gain
	it.Status, it.Reason = "done", ""
	it.TestFiles = changedTestFiles(rc.Path, snap, o.cfg.OutDir)
	applyGain(st, newly)
	it.PkgAfter = pctOf(st.Rows, it.Files)
	if o.commit && git {
		if err := gitCommit(rc.Path, it); err != nil {
			w := fmt.Sprintf("%s: tests accepted but NOT committed: %v", it.ID, err)
			say("[%s]   WARNING %s", rc.label(), w)
			st.Warnings = append(st.Warnings, w)
		}
	}
	if st.partial { // a parallel worker sees only its own item: the repo figure comes at merge
		say("[%s] DONE %s pkg %.1f%% -> %.1f%% (+%d stmts), waiting to merge", rc.label(), it.ID, it.PkgBefore, it.PkgAfter, gain)
	} else {
		g := st.goalTotals()
		say("[%s] DONE %s pkg %.1f%% -> %.1f%% (+%d stmts) | repo %.2f%% -> goal %.0f%% (%.2f to go, estimate)",
			rc.label(), it.ID, it.PkgBefore, it.PkgAfter, gain, g.Pct(), st.Goal, st.Goal-g.Pct())
	}
	st.log("done:" + it.ID)
	return true, ""
}

// pkgArg is the item's package as a go command argument in its module dir.
func pkgArg(it *Item) string {
	rel := pkgRelToModule(it.Module, it.Pkg)
	if rel == "" || rel == "." {
		return "."
	}
	return "./" + rel
}

// vetFindings runs go vet on the item's package and returns the findings in
// files (repo-relative): the ones the author wrote. Findings in production
// code or older tests are not the author's to fix and are ignored.
func vetFindings(ctx context.Context, rc RepoCfg, modAbs, pkg string, files []string, logPath string) []string {
	if len(files) == 0 {
		return nil
	}
	mine := map[string]bool{}
	for _, f := range files {
		mine[filepath.Clean(filepath.Join(rc.Path, filepath.FromSlash(f)))] = true
	}
	args := []string{"vet"}
	for _, f := range rc.TestFlags {
		if strings.HasPrefix(f, "-tags") { // the only test flag that changes what vet builds
			args = append(args, f)
		}
	}
	cmd := exec.CommandContext(ctx, "go", append(args, pkg)...)
	cmd.Dir = modAbs
	out, err := cmd.CombinedOutput()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	_ = os.WriteFile(logPath, out, 0o644)
	if err == nil {
		return nil
	}
	var bad []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		m := vetPosRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		f := m[1]
		if !filepath.IsAbs(f) {
			f = filepath.Join(modAbs, f)
		}
		if mine[filepath.Clean(f)] {
			bad = append(bad, line)
		}
	}
	return bad
}

// vetPosRe matches the file of a "file.go:line:col: message" vet finding.
var vetPosRe = regexp.MustCompile(`^(.+?\.go):\d+(?::\d+)?: `)

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

// newlyCovered returns, per item file, the blocks still uncovered in the goal
// metric that `after` hit. The item run only sees the package's own tests, but
// the goal metric is a -coverpkg run where other packages' tests count too;
// matching blocks (not counts) keeps statements those tests already cover from
// being credited a second time.
func newlyCovered(st *RepoState, files []string, after *Measurement) map[string][]UBlock {
	out := map[string][]UBlock{}
	for _, f := range files {
		for _, b := range st.UncovBlocks[f] {
			if after.Hit[f+":"+b.Span] {
				out[f] = append(out[f], b)
			}
		}
	}
	return out
}

// applyGain moves newly covered blocks into the live row figures; a fresh full
// run replaces them. The estimate can only be low (a new test may also cover
// other packages), never high. Only blocks still uncovered count: a parallel
// item's blocks may have been credited by a checkpoint since it started.
func applyGain(st *RepoState, newly map[string][]UBlock) {
	for f, hit := range newly {
		done := map[string]bool{}
		for _, b := range hit {
			done[b.Span] = true
		}
		var left []UBlock
		n := 0
		for _, b := range st.UncovBlocks[f] {
			if done[b.Span] {
				n += b.Stmts
			} else {
				left = append(left, b)
			}
		}
		st.UncovBlocks[f] = left
		for i := range st.Rows {
			if st.Rows[i].File == f {
				st.Rows[i].Covered += n
			}
		}
	}
}

// uncovRanges lists, per file, the line ranges still uncovered in the goal metric.
func uncovRanges(st *RepoState, files []string) map[string][]Range {
	out := map[string][]Range{}
	for _, f := range files {
		var rs []Range
		for _, b := range st.UncovBlocks[f] {
			rs = append(rs, spanLines(b.Span))
		}
		if len(rs) > 0 {
			out[f] = mergeRanges(rs)
		}
	}
	return out
}

// ensureBlocks migrates state written before v2.5, which has no block-level
// data: one fresh full run gives the exact uncovered blocks to credit against.
func ensureBlocks(ctx context.Context, o *opts, st *RepoState, rc RepoCfg) error {
	if st.UncovBlocks != nil {
		return nil
	}
	say("[%s] state has no block-level coverage (written before v2.5) - one fresh full run to add it", rc.Name)
	return fullMeasureQuiet(ctx, o, st, rc)
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

// waitDelay bounds the wait for a killed claude's output pipes: a tool process
// it started can outlive it and hold them open, which would hang the run.
const waitDelay = 10 * time.Second

// killedAtTimeout reports whether a call's context expired (item_timeout_min).
// A killed call reports no cost, so callers charge its whole cap: the most it
// may have spent, and never less than it spent.
func killedAtTimeout(cctx context.Context) bool {
	return errors.Is(cctx.Err(), context.DeadlineExceeded)
}

// rateLimitErr is a claude call refused by a rate limit: retried after a
// back-off, without using up an attempt.
type rateLimitErr struct{ msg string }

func (e *rateLimitErr) Error() string { return "rate limited: " + e.msg }

var rateLimitRe = regexp.MustCompile(`(?i)rate[ _-]?limit|\b429\b|overloaded|too many requests`)

type claudeResult struct {
	TerminalReason string  `json:"terminal_reason"`
	Result         string  `json:"result"`
	IsError        bool    `json:"is_error"`
	Subtype        string  `json:"subtype"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	NumTurns       int     `json:"num_turns"`
}

func callClaude(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item, model string, usd float64, failNote string) (float64, error) {
	cctx, cancel := context.WithTimeout(ctx, o.cfg.itemTimeout())
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", claudeArgs(o, model, usd)...)
	cmd.Dir = filepath.Join(rc.Path, filepath.FromSlash(it.Module))
	cmd.Stdin = strings.NewReader(buildPrompt(o, rc, st, it, failNote))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	_ = os.WriteFile(filepath.Join(repoDir(o, rc.Name), "logs", slug(it.ID)+"-claude.json"), stdout.Bytes(), 0o644)
	if killedAtTimeout(cctx) {
		say("[%s]   claude killed at the %v item_timeout_min: charging its cap $%.2f (a killed call reports no cost)", rc.Name, o.cfg.itemTimeout(), usd)
		return usd, fmt.Errorf("claude was killed at the %v time limit before finishing; work in smaller steps and run the tests sooner", o.cfg.itemTimeout())
	}
	var res claudeResult
	cost := 0.0
	if jerr := json.Unmarshal(stdout.Bytes(), &res); jerr == nil {
		cost = res.TotalCostUSD
		switch {
		case res.IsError && rateLimitRe.MatchString(res.Result):
			return cost, &rateLimitErr{firstLine(res.Result)}
		case res.IsError && cost == 0:
			// the auth probe at the start of the run passed: mid-run this is a
			// rate or usage limit, not a login problem
			return cost, &fatalErr{fmt.Sprintf("paused: rate or usage limit - claude -p returned an error without spending anything: %s; wait, then re-run to resume (the item stays queued)", firstLine(res.Result))}
		case res.IsError && res.TerminalReason == "api_error":
			return cost, &fatalErr{fmt.Sprintf("claude -p hit an API error: %s; re-run to resume", firstLine(res.Result))}
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

func buildPrompt(o *opts, rc RepoCfg, st *RepoState, it *Item, failNote string) string {
	var sb strings.Builder
	mod := findModule(st, it.Module)
	fmt.Fprintf(&sb, "Raise the unit-test coverage of ONE Go package with real, behavior-verifying tests.\n\n")
	fmt.Fprintf(&sb, "Working directory: %s (the module root; run go commands from here)\nPackage dir: %s (relative to the module root; %s from the repo root)\nModule: %s (go %s, testify in go.mod: %v)\nCurrent package coverage: %.1f%% (%d uncovered statements). Repo goal: %.0f%%.\n",
		filepath.Join(rc.Path, filepath.FromSlash(it.Module)), pkgArg(it), it.Pkg, mod.Path, mod.GoVersion, mod.Testify, it.PkgBefore, it.Uncovered, st.Goal)
	if len(rc.TestFlags) > 0 {
		fmt.Fprintf(&sb, "Always run tests with these extra flags: %s\n", strings.Join(rc.TestFlags, " "))
	}
	if mod.GoVersion != "" && !goAtLeast122(mod.GoVersion) {
		sb.WriteString("go.mod is below 1.22: if you use t.Parallel in table loops, copy the loop variable (tc := tc).\n")
	}
	sb.WriteString("\nUNCOVERED line ranges per file (prioritize largest, most logic-heavy first; skip trivial ones). Lines not listed are already covered, possibly by other packages' tests, and earn nothing:\n")
	uncov := uncovRanges(st, it.Files)
	var files []string
	for _, f := range it.Files {
		if len(uncov[f]) > 0 {
			files = append(files, f)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return len(uncov[files[i]]) > len(uncov[files[j]]) })
	for i, f := range files {
		if i >= 8 {
			fmt.Fprintf(&sb, "- ... %d more files in the package\n", len(files)-i)
			break
		}
		var rs []string
		for j, r := range uncov[f] {
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

// gitCommit commits the item's test files and nothing else: other changes in
// the tree (the user's, out_dir inside the repo) stay uncommitted.
func gitCommit(root string, it *Item) error {
	var files []string
	for _, p := range it.TestFiles {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
		if _, terr := git(root, "cat-file", "-e", "HEAD:"+p); err == nil || terr == nil {
			files = append(files, p) // on disk, or a tracked file the item deleted
		}
	}
	if len(files) == 0 {
		return nil
	}
	if out, err := git(root, append([]string{"add", "-A", "--"}, files...)...); err != nil {
		return fmt.Errorf("git add: %v: %s", err, strings.TrimSpace(out))
	}
	if _, err := git(root, append([]string{"diff", "--cached", "--quiet", "HEAD", "--"}, files...)...); err == nil {
		return nil // nothing of this item's left to commit
	}
	msg := fmt.Sprintf("test(%s): add behavioral tests (+%d covered statements)", it.Pkg, it.Gained)
	if out, err := git(root, append([]string{"commit", "-q", "-m", msg, "--only", "--"}, files...)...); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
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
	if rootAbs, _ := filepath.Abs(root); within(rootAbs, outAbs) {
		outAbs = "" // the tree itself is in out_dir (a parallel item's worktree): nothing to skip
	}
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
		if outAbs != "" && within(abs, outAbs) {
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

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	if dir == "" {
		return false
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return strings.EqualFold(p, dir) || strings.HasPrefix(strings.ToLower(p), strings.ToLower(dir)+string(filepath.Separator))
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

// restore puts p back to HEAD (not the index, so a staged edit cannot
// survive), or deletes it if HEAD does not have it.
func restore(root, p string) {
	if _, err := git(root, "cat-file", "-e", "HEAD:"+p); err == nil {
		_, _ = git(root, "checkout", "HEAD", "--", p)
		return
	}
	_, _ = git(root, "rm", "--cached", "-q", "--ignore-unmatch", "--", p)
	_ = os.Remove(filepath.Join(root, filepath.FromSlash(p)))
}

// dirtyProduction lists changed files an author may not touch (anything but
// *_test.go, testdata/ and, with allowDeps, go.mod/go.sum).
func dirtyProduction(root, outDir string, allowDeps bool) []string {
	var out []string
	for _, p := range dirtyFiles(root, outDir) {
		if !isAllowedPath(p, allowDeps) {
			out = append(out, p)
		}
	}
	return out
}

// requireCleanProduction refuses to start an item while production files
// differ from HEAD: they could not be told apart from the author's edits, and
// a leftover mutation would be tested against instead of the real code.
func requireCleanProduction(o *opts, rc RepoCfg) error {
	if dirty := dirtyProduction(rc.Path, o.cfg.OutDir, o.cfg.AllowDeps); len(dirty) > 0 {
		return fmt.Errorf("non-test files differ from HEAD (e.g. %s - a mutation-check edit that was not restored?); restore them with `git checkout HEAD -- <file>` or commit them, then retry", dirty[0])
	}
	return nil
}

// revertForbidden restores every non-test file that differs from HEAD. Items
// only start with production clean (requireCleanProduction), so each one was
// changed during this item - including a mutation the author did not undo.
func revertForbidden(root, outDir string, allowDeps bool) []string {
	bad := dirtyProduction(root, outDir, allowDeps)
	for _, p := range bad {
		restore(root, p)
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
	if o.dryRun {
		return beginDryRun(ctx, o, rc, st, it)
	}
	if isGit(rc.Path) {
		if st.Status == "planned" {
			if dirty := dirtyFiles(rc.Path, o.cfg.OutDir); len(dirty) > 0 {
				return fmt.Errorf("working tree has uncommitted changes (e.g. %s); commit or stash first", dirty[0])
			}
			if rc.Branch != "" {
				if err := gitCheckoutBranch(rc.Path, rc.Branch); err != nil {
					return err
				}
			}
		}
		if err := requireCleanProduction(o, rc); err != nil {
			return err
		}
	}
	if err := ensureBlocks(ctx, o, st, rc); err != nil {
		return err
	}
	ev := startSessionItem(o, st, it)
	if err := preMeasure(ctx, o, rc, st, it); err != nil {
		endSessionItem(st, it, ev)
		if serr := saveState(o, st); serr != nil {
			return serr
		}
		fmt.Printf("BLOCKED %s: %s\n", it.ID, it.Reason)
		exitCode = 3
		return nil
	}
	snap := snapshot(rc.Path, o.cfg.OutDir)
	_ = os.MkdirAll(filepath.Dir(snapPath(o, rc, it)), 0o755)
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(snapPath(o, rc, it), data, 0o644); err != nil {
		return err
	}
	prompt := buildPrompt(o, rc, st, it, "")
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

// beginDryRun writes the prompt begin would write and nothing else: no branch
// checkout, no snapshot, no state save. A "planned" repo must stay planned, or
// the real begin would skip the clean-tree check and the branch checkout.
func beginDryRun(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, it *Item) error {
	prompt, err := dryRunPrompt(ctx, o, rc, st, it)
	if err != nil {
		return err
	}
	pp := filepath.Join(repoDir(o, rc.Name), "work", slug(it.ID)+".dry-run.prompt.md")
	_ = os.MkdirAll(filepath.Dir(pp), 0o755)
	if err := os.WriteFile(pp, []byte(prompt), 0o644); err != nil {
		return err
	}
	if o.quiet {
		fmt.Println(pp)
		return nil
	}
	fmt.Printf("DRY RUN - nothing saved, tree and branch untouched. Task prompt for %s written to %s\n\n%s\n", it.ID, pp, prompt)
	return nil
}

// cmdFinish: verify what was written (revert forbidden edits, tests green, flake
// check, real gain). Success marks done; failure returns feedback, or with
// -abandon reverts the attempt and marks the item blocked.
func cmdFinish(ctx context.Context, o *opts, id string, abandon bool, cost float64, model string, capUSD float64) error {
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
	ev := startSessionItem(o, st, it) // a no-op unless finish runs without its begin in this run
	it.Attempts++
	it.CostUSD += cost
	st.SpendUSD += cost
	if abandon {
		if g {
			revertAll(rc.Path, snap, o.cfg.OutDir)
		}
		it.Status, it.Reason = "blocked", "abandoned by author after "+fmt.Sprint(it.Attempts)+" attempt(s)"
		st.log("blocked:" + it.ID)
		endSessionItem(st, it, ev)
		fmt.Println("BLOCKED", it.ID, "- attempt reverted")
		return saveState(o, st)
	}
	now := ts(time.Now())
	it.AttemptLog = append(it.AttemptLog, Attempt{N: it.Attempts, Run: it.Run, Worker: 1, Model: model, CapUSD: capUSD, CostUSD: cost, Ended: now})
	ev.emit("attempt", map[string]any{"item": it.ID, "worker": 1, "attempt": it.Attempts, "model": model, "cap_usd": capUSD, "cost_usd": cost})
	head := ""
	if g {
		head = headSHA(rc.Path)
	}
	ok, fb := verifyItem(ctx, o, rc, st, it, snap, g)
	st.Verified = false
	if ok {
		if h := headSHA(rc.Path); g && h != head {
			it.MergeSHA = h
			ev.emit("merge", map[string]any{"item": it.ID, "sha": h})
		}
		endSessionItem(st, it, ev)
	} else if r := st.runByID(it.Run); r != nil {
		st.measureRun(r, time.Now())
	}
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

// cmdNextCall tells a fan-out script how to make the next claude call for an
// item, by the same rules `gocov run` uses. It prints
// "MODEL CAP_USD TIMEOUT_SEC WORKDIR" (WORKDIR last: it may contain spaces)
// and exits 0, or prints why not and exits 4 (budget or time limit reached:
// stop the run; the repo is marked paused-budget) or 5 (max_attempts used:
// abandon the item). With -dry-run nothing is saved. runSpent and elapsed are the script's own run totals,
// the counterparts of gocov run's run budget and time limit.
func cmdNextCall(o *opts, id string, attempt int, runSpent float64, elapsed int) error {
	rc, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	it, err := itemByID(st, id)
	if err != nil {
		return err
	}
	if attempt < 1 {
		return fmt.Errorf("usage: gocov next-call -repo R -item ID -attempt K [-spent USD] [-elapsed SEC]")
	}
	if attempt > o.cfg.MaxAttempts {
		fmt.Printf("%s: all %d attempt(s) used (max_attempts)\n", it.ID, o.cfg.MaxAttempts)
		exitCode = 5
		return nil
	}
	pause := ""
	// like gocov run, the time limit is checked before an item, not between attempts
	if lim := o.cfg.Budget.TimeLimitMin; lim > 0 && attempt == 1 && elapsed >= lim*60 {
		pause = "time limit reached; re-run to resume"
	} else if left := callCap(o.cfg, st.SpendUSD, runSpent); left < minCallUSD {
		pause = budgetReason(o.cfg, left, st.SpendUSD, runSpent)
	} else {
		fmt.Printf("%s %.2f %d %s\n", modelFor(o, it, attempt), left, int(math.Ceil(o.cfg.itemTimeout().Seconds())), filepath.Join(rc.Path, filepath.FromSlash(it.Module)))
		return nil
	}
	fmt.Println(pause)
	exitCode = 4
	if o.dryRun {
		return nil
	}
	st.Status, st.Reason = "paused-budget", pause
	st.log("run:paused-budget")
	return saveState(o, st)
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
