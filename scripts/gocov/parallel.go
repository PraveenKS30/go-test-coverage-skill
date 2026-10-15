package main

// `gocov run -parallel N`: several packages of one repo at once.
//
// The unit of work stays one package (its test files share a namespace, so a
// package is never split). Each item runs in its own git worktree, cut from
// the work branch's tip, so the snapshot, the mutation check and the revert
// of production edits see only that item's changes. Items go out in waves of
// up to N that do not import each other (go list), drawing worker slots from
// one pool shared by all repos, so jobs x N never multiplies. One goroutine
// per repo (the coordinator) owns its RepoState and state.json: workers send
// results back and never save. Accepted items are committed in their worktree
// and cherry-picked onto the work branch one at a time; a conflict re-queues
// the item to run again from the new tip. After each wave a fresh full run
// (checkpoint) credits indirect coverage and stops the dispatch at the goal.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// workerPool hands out worker slots 1..n; one pool caps all repos of a run.
type workerPool struct{ slots chan int }

func newPool(n int) *workerPool {
	if n < 1 {
		n = 1
	}
	p := &workerPool{slots: make(chan int, n)}
	for i := 1; i <= n; i++ {
		p.slots <- i
	}
	return p
}

func (p *workerPool) acquire() int  { return <-p.slots }
func (p *workerPool) release(w int) { p.slots <- w }

// throttleGo gives each of n workers its share of the CPUs for go builds
// (GOFLAGS=-p=cores/n, unless GOFLAGS already sets -p) and returns a function
// that restores GOFLAGS. The workers' go test / vet and the claude calls'
// own go commands all inherit it.
func throttleGo(n int) func() {
	old, had := os.LookupEnv("GOFLAGS")
	if strings.Contains(old, "-p=") {
		return func() {}
	}
	k := runtime.NumCPU() / n
	if k < 1 {
		k = 1
	}
	_ = os.Setenv("GOFLAGS", strings.TrimSpace(old+" -p="+strconv.Itoa(k)))
	return func() {
		if had {
			_ = os.Setenv("GOFLAGS", old)
		} else {
			_ = os.Unsetenv("GOFLAGS")
		}
	}
}

// repoLocks serializes the git commands that write a repo's shared refs
// (worktree add/remove, commits, cherry-picks) across its workers.
var repoLocks sync.Map

func repoLock(root string) *sync.Mutex {
	m, _ := repoLocks.LoadOrStore(filepath.Clean(root), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ------------------------------------------------------------ dependencies ----

// depGraph tells which items must not run in the same wave: one imports the
// other (directly or not), or its tests do.
type depGraph struct {
	ip      map[string]string          // item ID -> import path
	imports map[string]map[string]bool // import path -> what it or its tests depend on
}

func (g *depGraph) related(a, b string) bool {
	pa, pb := g.ip[a], g.ip[b]
	if pa == "" || pb == "" {
		return false
	}
	return g.imports[pa][pb] || g.imports[pb][pa]
}

type listedPkg struct {
	ImportPath   string
	Deps         []string
	TestImports  []string
	XTestImports []string
}

// loadDeps runs `go list -e -json ./...` once per module.
func loadDeps(ctx context.Context, rc RepoCfg, st *RepoState) *depGraph {
	g := &depGraph{ip: map[string]string{}, imports: map[string]map[string]bool{}}
	args := []string{"list", "-e", "-json"}
	for _, f := range rc.TestFlags {
		if strings.HasPrefix(f, "-tags") {
			args = append(args, f)
		}
	}
	for _, m := range st.Modules {
		cmd := exec.CommandContext(ctx, "go", append(args, "./...")...)
		cmd.Dir = filepath.Join(rc.Path, filepath.FromSlash(m.Dir))
		out, _ := cmd.Output() // -e: packages with errors are still listed
		var pkgs []listedPkg
		for dec := json.NewDecoder(bytes.NewReader(out)); ; {
			var p listedPkg
			if dec.Decode(&p) != nil {
				break
			}
			pkgs = append(pkgs, p)
		}
		deps := map[string][]string{}
		for _, p := range pkgs {
			deps[p.ImportPath] = p.Deps
		}
		for _, p := range pkgs {
			set := map[string]bool{}
			for _, d := range p.Deps {
				set[d] = true
			}
			for _, ti := range append(append([]string(nil), p.TestImports...), p.XTestImports...) {
				set[ti] = true
				for _, d := range deps[ti] {
					set[d] = true
				}
			}
			delete(set, p.ImportPath) // an external test package imports its own package
			g.imports[p.ImportPath] = set
		}
	}
	for _, it := range st.Items {
		m := findModule(st, it.Module)
		ip := m.Path
		if rel := pkgRelToModule(it.Module, it.Pkg); rel != "" && rel != "." {
			ip += "/" + rel
		}
		g.ip[it.ID] = ip
	}
	return g
}

// pickWave returns the indexes of up to n queued items, in plan order, no two
// of which are related.
func pickWave(st *RepoState, g *depGraph, n int) []int {
	var wave []int
	for i, it := range st.Items {
		if len(wave) == n {
			break
		}
		if it.Status != "queued" {
			continue
		}
		ok := true
		for _, j := range wave {
			if g.related(it.ID, st.Items[j].ID) {
				ok = false
				break
			}
		}
		if ok {
			wave = append(wave, i)
		}
	}
	return wave
}

// --------------------------------------------------------------- worktrees ----

func worktreeOf(o *opts, rc RepoCfg, id string) (dir, branch string) {
	dir, _ = filepath.Abs(filepath.Join(repoDir(o, rc.Name), "wt", slug(id)))
	return dir, "cov/" + slug(id)
}

// addWorktree cuts the item's worktree and branch from the work branch's tip
// and returns the worktree dir, its branch and the commit it starts from.
func addWorktree(o *opts, rc RepoCfg, id string) (dir, branch, base string, err error) {
	dir, branch = worktreeOf(o, rc, id)
	l := repoLock(rc.Path)
	l.Lock()
	defer l.Unlock()
	_ = dropWorktree(rc.Path, dir, branch) // a killed run's leftovers
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", "", err
	}
	base = headSHA(rc.Path)
	if out, err := git(rc.Path, "worktree", "add", "-b", branch, dir, base); err != nil {
		return "", "", "", fmt.Errorf("git worktree add: %v: %s", err, firstLine(strings.TrimSpace(out)))
	}
	return dir, branch, base, nil
}

// dropWorktree removes an item's worktree and branch. The caller holds the
// repo lock.
func dropWorktree(root, dir, branch string) error {
	_, _ = git(root, "worktree", "remove", "--force", dir)
	var err error
	if _, serr := os.Stat(dir); serr == nil {
		err = os.RemoveAll(dir)
	}
	_, _ = git(root, "worktree", "prune")
	_, _ = git(root, "branch", "-D", branch)
	return err
}

// cherryPick applies an item's commit to the work branch and returns the new
// commit, or aborts and returns why it did not apply.
func cherryPick(root, sha string) (string, error) {
	l := repoLock(root)
	l.Lock()
	defer l.Unlock()
	if out, err := git(root, "cherry-pick", sha); err != nil {
		_, _ = git(root, "cherry-pick", "--abort")
		return "", fmt.Errorf("%v: %s", err, firstLine(strings.TrimSpace(out)))
	}
	return headSHA(root), nil
}

// ----------------------------------------------------------------- workers ----

// workerView is the private copy of the state a worker reads and writes: the
// coordinator's state is never shared with a worker.
func (s *RepoState) workerView(it *Item) *RepoState {
	v := &RepoState{Name: s.Name, Path: s.Path, Goal: s.Goal, GoalBasis: s.GoalBasis, Scope: s.Scope, Modules: s.Modules,
		Rows: append([]FileCov(nil), s.Rows...), UncovBlocks: map[string][]UBlock{}, partial: true}
	for _, f := range it.Files {
		v.UncovBlocks[f] = append([]UBlock(nil), s.UncovBlocks[f]...)
	}
	return v
}

// itemMsg goes from a worker to its repo's coordinator: first when the item
// starts (res nil), then with its result.
type itemMsg struct {
	idx      int
	worker   int
	at       time.Time
	worktree string
	branch   string
	res      *itemResult
}

type itemResult struct {
	item      Item
	newly     map[string][]UBlock // blocks the item's tests newly cover
	warnings  []string
	err       error  // the repo must pause (auth, limit, worktree); the item stays queued
	sha       string // the commit of the accepted tests in the item's worktree
	commitErr error
	finished  time.Time
}

// runWorker works one item in its own worktree and reports to the coordinator.
func runWorker(ctx context.Context, o *opts, rc RepoCfg, view *RepoState, it Item, idx int, env itemEnv, pool *workerPool, msgs chan<- itemMsg) {
	w := pool.acquire()
	defer pool.release(w)
	env.worker = w
	res := &itemResult{}
	defer func() {
		res.item, res.finished = it, time.Now()
		msgs <- itemMsg{idx: idx, worker: w, res: res}
	}()
	wt, br, base, err := addWorktree(o, rc, it.ID)
	if err != nil {
		env.b.settleFor(env.acct, env.pre, 0)
		res.err = &fatalErr{"could not create the item's git worktree: " + err.Error()}
		return
	}
	msgs <- itemMsg{idx: idx, worker: w, at: time.Now(), worktree: wt, branch: br}
	rcW := rc
	rcW.Path, rcW.tag = wt, fmt.Sprintf("%s w%d", rc.Name, w)
	ow := *o
	ow.commit = false // the coordinator merges the item's commit instead
	before := map[string][]UBlock{}
	for _, f := range it.Files {
		before[f] = view.UncovBlocks[f]
	}
	res.err = runItem(ctx, &ow, rcW, view, &it, true, env)
	res.newly = removedBlocks(before, view.UncovBlocks)
	res.warnings = view.Warnings
	if res.err != nil || it.Status != "done" {
		return
	}
	l := repoLock(rc.Path)
	l.Lock()
	cerr := gitCommit(wt, &it)
	sha := headSHA(wt)
	l.Unlock()
	switch {
	case cerr != nil:
		res.commitErr = cerr
	case sha == "" || sha == base:
		res.commitErr = fmt.Errorf("nothing was committed")
	default:
		res.sha = sha
	}
}

// removedBlocks returns, per file, the blocks in before that after lacks.
func removedBlocks(before, after map[string][]UBlock) map[string][]UBlock {
	out := map[string][]UBlock{}
	for f, bs := range before {
		left := map[string]bool{}
		for _, b := range after[f] {
			left[b.Span] = true
		}
		for _, b := range bs {
			if !left[b.Span] {
				out[f] = append(out[f], b)
			}
		}
	}
	return out
}

// ------------------------------------------------------------- coordinator ----

// runParallel is runSequential's counterpart for parallel > 1.
func runParallel(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, b *budget, acct *repoAcct, rt *runTrack, ev *eventLog) (bool, error) {
	if err := ensureBlocks(ctx, o, st, rc); err != nil {
		return false, err
	}
	g := loadDeps(ctx, rc, st)
	_, _ = git(rc.Path, "worktree", "prune")
	n := o.cfg.Parallel
	processed, wave := 0, 0
	fresh := false
	retries := map[string]int{}
	for {
		limit := n
		if o.pilot > 0 {
			if processed >= o.pilot {
				st.Status = "in-progress"
				st.Reason = fmt.Sprintf("pilot of %d item(s) finished; re-run without -pilot to continue", o.pilot)
				break
			}
			limit = min(n, o.pilot-processed)
		}
		if b.timeUp() {
			st.Status, st.Reason = "paused-budget", "time limit reached; re-run to resume"
			break
		}
		cand := pickWave(st, g, limit)
		if len(cand) == 0 {
			break
		}
		// reserve each item's first call at dispatch: what one wave may spend
		// is held before any of it starts
		var take []int
		var caps []float64
		for _, i := range cand {
			c, ok := b.reserveFor(acct)
			if !ok {
				break
			}
			take, caps = append(take, i), append(caps, c)
		}
		if len(take) == 0 {
			st.Status, st.Reason = "paused-budget", budgetReason(o.cfg, b.nextCap(acct), b.repoSpent(acct), b.committed())
			break
		}
		wave++
		processed += len(take)
		st.Status, st.Reason = "in-progress", ""
		ids := make([]string, len(take))
		for k, i := range take {
			ids[k] = st.Items[i].ID
		}
		ev.emit("wave-start", map[string]any{"wave": wave, "items": ids})
		say("[%s] wave %d: %s", rc.Name, wave, strings.Join(ids, ", "))
		merged, fatal := runWave(ctx, o, rc, st, b, acct, rt, ev, wave, take, caps, retries)
		st.Verified = false
		if fatal != nil {
			st.Status, st.Reason = "paused-error", fatal.Error()
			_ = saveState(o, st)
			say("[%s] PAUSED: %s", rc.Name, st.Reason)
			return fresh, fatal
		}
		if merged == 0 && fresh {
			continue // nothing reached the work branch: the last checkpoint still holds
		}
		if err := fullMeasure(ctx, o, st, rc, "checkpoint"); err != nil {
			return false, err
		}
		fresh = true
		pct := st.goalTotals().Pct()
		ev.emit("checkpoint", map[string]any{"wave": wave, "verified_pct": round2(pct), "goal": st.Goal})
		st.log(fmt.Sprintf("checkpoint:wave%d", wave))
		_ = saveState(o, st)
		say("[%s] checkpoint after wave %d: verified %.2f%% (goal %.0f%%)", rc.Name, wave, pct, st.Goal)
		if pct >= st.Goal {
			break
		}
	}
	return fresh, nil
}

// runWave runs one wave and merges its results as they arrive. It returns
// how many items reached the work branch and the first error that must pause
// the repo; in-flight items always finish first.
func runWave(ctx context.Context, o *opts, rc RepoCfg, st *RepoState, b *budget, acct *repoAcct, rt *runTrack, ev *eventLog, wave int, take []int, caps []float64, retries map[string]int) (int, error) {
	msgs := make(chan itemMsg, 2*len(take))
	for k, i := range take {
		it := st.Items[i]
		go runWorker(ctx, o, rc, st.workerView(&it), it, i, itemEnv{b: b, acct: acct, ev: ev, pre: caps[k]}, rt.pool, msgs)
	}
	merged := 0
	var fatal error
	for pending := len(take); pending > 0; {
		m := <-msgs
		it := &st.Items[m.idx]
		if m.res == nil {
			startItem(it, rt.id, wave, m.worker, m.worktree, m.branch, ev, m.at)
			_ = saveState(o, st)
			continue
		}
		pending--
		ok, err := mergeResult(o, rc, st, it, m, ev, retries)
		if ok {
			merged++
		}
		if err != nil && fatal == nil {
			fatal = err
		}
	}
	return merged, fatal
}

// mergeResult takes a worker's result into the state; an accepted item is
// cherry-picked onto the work branch. It reports whether the item merged.
func mergeResult(o *opts, rc RepoCfg, st *RepoState, it *Item, m itemMsg, ev *eventLog, retries map[string]int) (bool, error) {
	r := m.res
	keep, prevCost := *it, it.CostUSD
	*it = r.item
	// the coordinator's placement and timing stand; the worker's copy predates them
	it.Run, it.StartedAt, it.Wave, it.Worker, it.Worktree, it.Branch = keep.Run, keep.StartedAt, keep.Wave, keep.Worker, keep.Worktree, keep.Branch
	it.FinishedAt = ts(r.finished)
	st.SpendUSD += it.CostUSD - prevCost
	st.Warnings = append(st.Warnings, r.warnings...)
	label := fmt.Sprintf("%s w%d", rc.Name, m.worker)
	status, merged := it.Status, false
	var fatal error
	switch {
	case r.err != nil:
		fatal, status = r.err, "paused"
	case it.Status == "done" && r.commitErr != nil:
		w := fmt.Sprintf("%s: tests accepted but NOT committed in its worktree, so not merged: %v", it.ID, r.commitErr)
		it.Status, it.Reason, status = "blocked", w, "blocked"
		st.Warnings = append(st.Warnings, w)
		say("[%s] BLOCKED %s", label, w)
	case it.Status == "done":
		sha, err := cherryPick(rc.Path, r.sha)
		if err != nil {
			retries[it.ID]++
			ev.emit("merge-conflict", map[string]any{"item": it.ID, "error": err.Error()})
			if retries[it.ID] == 1 {
				it.Status, it.Reason, it.Gained, it.TestFiles, status = "queued", "", 0, nil, "requeued"
				say("[%s] %s did not merge onto the work branch (%v): queued again, to run from the new tip", label, it.ID, err)
			} else {
				it.Status, it.Reason, status = "blocked", "its tests did not merge onto the work branch twice: "+err.Error(), "blocked"
				say("[%s] BLOCKED %s: %s", label, it.ID, it.Reason)
			}
			break
		}
		it.MergeSHA, merged = sha, true
		applyGain(st, r.newly)
		it.PkgAfter = pctOf(st.Rows, it.Files)
		ev.emit("merge", map[string]any{"item": it.ID, "sha": sha})
		g := st.goalTotals()
		say("[%s] MERGED %s at %.10s (+%d stmts) | repo %.2f%% -> goal %.0f%% (estimate)", label, it.ID, sha, it.Gained, g.Pct(), st.Goal)
	}
	switch it.Status {
	case "done":
		st.log("done:" + it.ID)
	case "blocked":
		st.log("blocked:" + it.ID)
	}
	dir, branch := worktreeOf(o, rc, it.ID)
	l := repoLock(rc.Path)
	l.Lock()
	if err := dropWorktree(rc.Path, dir, branch); err != nil {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%s: could not remove its worktree %s: %v", it.ID, dir, err))
	}
	l.Unlock()
	endItemEvent(ev, it, status)
	_ = saveState(o, st)
	return merged, fatal
}
