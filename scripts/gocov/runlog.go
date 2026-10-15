package main

// Run tracking, so what was written down shows how a run really went: one
// RunRecord per driver invocation in state.json (runs[]), per-item timing and
// placement on the items, and an append-only events.jsonl timeline per repo.
// Concurrency is measured from the items' start/end times, not taken from the
// setting.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// skillVersion is SKILL.md's metadata.version (a test keeps them equal).
const skillVersion = "2.8.0"

// Drivers named in run records.
const (
	driverRun     = "gocov run"
	driverSession = "in-session begin-finish"
)

// RunRecord is one invocation of a driver against one repo.
type RunRecord struct {
	ID           string    `json:"id"`
	Started      string    `json:"started"`
	Ended        string    `json:"ended,omitempty"` // empty while running, or if the run was killed
	Driver       string    `json:"driver"`
	Parallel     int       `json:"parallel"`
	Jobs         int       `json:"jobs"`
	Models       ModelsCfg `json:"models"`
	Budget       BudgetCfg `json:"budget"`
	SkillVersion string    `json:"skill_version"`
	Status       string    `json:"status,omitempty"` // repo status when the run ended

	// measured, from the items this run worked
	Items                  int     `json:"items"`
	Waves                  int     `json:"waves"`
	WallSec                float64 `json:"wall_sec"`     // run start to end
	ItemSecSum             float64 `json:"item_sec_sum"` // sum of item durations
	Speedup                float64 `json:"speedup"`      // item_sec_sum / wall_sec
	MaxConcurrencyObserved int     `json:"max_concurrency_observed"`
}

func ts(t time.Time) string { return t.Format(time.RFC3339Nano) }

func newRunID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

// addRun appends a run record and returns its ID.
func (s *RepoState) addRun(o *opts, id, driver string, parallel int, now time.Time) string {
	s.Runs = append(s.Runs, RunRecord{ID: id, Started: ts(now), Driver: driver, Parallel: parallel, Jobs: o.cfg.Jobs,
		Models: o.cfg.Models, Budget: o.cfg.Budget, SkillVersion: skillVersion})
	return id
}

func (s *RepoState) runByID(id string) *RunRecord {
	for i := range s.Runs {
		if s.Runs[i].ID == id {
			return &s.Runs[i]
		}
	}
	return nil
}

// resetInterrupted clears the in-flight marks of queued items a killed run
// left behind, so they are not shown as running. events.jsonl keeps the trace.
func (s *RepoState) resetInterrupted() {
	for i := range s.Items {
		it := &s.Items[i]
		if it.Status == "queued" && it.StartedAt != "" && it.FinishedAt == "" {
			it.StartedAt, it.Worker, it.Wave, it.Worktree, it.Branch = "", 0, 0, "", ""
		}
	}
}

type span struct{ start, end time.Time }

// maxOverlap is the largest number of spans open at one instant. A span that
// ends exactly when another starts does not overlap it.
func maxOverlap(spans []span) int {
	type pt struct {
		t time.Time
		d int
	}
	var pts []pt
	for _, s := range spans {
		pts = append(pts, pt{s.start, 1}, pt{s.end, -1})
	}
	sort.Slice(pts, func(i, j int) bool {
		if !pts[i].t.Equal(pts[j].t) {
			return pts[i].t.Before(pts[j].t)
		}
		return pts[i].d < pts[j].d
	})
	cur, best := 0, 0
	for _, p := range pts {
		cur += p.d
		if cur > best {
			best = cur
		}
	}
	return best
}

// measureRun fills r's measured fields from the items it worked; an item still
// running counts until now.
func (s *RepoState) measureRun(r *RunRecord, now time.Time) {
	var spans []span
	waves := 0
	for _, it := range s.Items {
		if it.Run != r.ID || it.StartedAt == "" {
			continue
		}
		st, err := time.Parse(time.RFC3339Nano, it.StartedAt)
		if err != nil {
			continue
		}
		end := now
		if e, err := time.Parse(time.RFC3339Nano, it.FinishedAt); err == nil {
			end = e
		}
		spans = append(spans, span{st, end})
		if it.Wave > waves {
			waves = it.Wave
		}
	}
	r.Items, r.Waves = len(spans), waves
	r.ItemSecSum = 0
	for _, sp := range spans {
		r.ItemSecSum += sp.end.Sub(sp.start).Seconds()
	}
	r.ItemSecSum = round2(r.ItemSecSum)
	r.MaxConcurrencyObserved = maxOverlap(spans)
	if start, err := time.Parse(time.RFC3339Nano, r.Started); err == nil {
		r.WallSec = round2(now.Sub(start).Seconds())
	}
	r.Speedup = 0
	if r.WallSec > 0 {
		r.Speedup = round2(r.ItemSecSum / r.WallSec)
	}
}

// endRun closes the run record and logs run-end.
func (s *RepoState) endRun(id string, ev *eventLog, now time.Time) {
	r := s.runByID(id)
	if r == nil {
		return
	}
	s.measureRun(r, now)
	r.Ended, r.Status = ts(now), s.Status
	ev.emitAt(now, "run-end", map[string]any{"status": s.Status, "item_count": r.Items, "waves": r.Waves, "wall_sec": r.WallSec,
		"item_sec_sum": r.ItemSecSum, "speedup": r.Speedup, "max_concurrency_observed": r.MaxConcurrencyObserved})
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// nextWave is the wave number of the next item a one-at-a-time driver starts
// in run id: each of its items is a wave of one.
func (s *RepoState) nextWave(id string) int {
	n := 0
	for _, it := range s.Items {
		if it.Run == id && it.Wave > n {
			n = it.Wave
		}
	}
	return n + 1
}

// ---------------------------------------------------------- events.jsonl ----

// eventLog appends one JSON object per line to <out_dir>/<repo>/events.jsonl.
// Lines are written whole and never rewritten, so a killed run still leaves a
// readable timeline; several processes (a fan-out script's gocov calls) may
// append to the same file.
type eventLog struct {
	mu   sync.Mutex
	path string
	run  string
}

func newEventLog(o *opts, repo, run string) *eventLog {
	return &eventLog{path: filepath.Join(repoDir(o, repo), "events.jsonl"), run: run}
}

func (e *eventLog) runID() string {
	if e == nil {
		return ""
	}
	return e.run
}

func (e *eventLog) emit(kind string, f map[string]any) { e.emitAt(time.Now(), kind, f) }

func (e *eventLog) emitAt(t time.Time, kind string, f map[string]any) {
	if e == nil {
		return
	}
	m := map[string]any{"t": ts(t), "run": e.run, "ev": kind}
	for k, v := range f {
		m[k] = v
	}
	line, err := json.Marshal(m)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(e.path), 0o755)
	fh, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fh.Write(append(line, '\n'))
	_ = fh.Close()
}

func runStartFields(r *RunRecord) map[string]any {
	return map[string]any{"driver": r.Driver, "parallel": r.Parallel, "jobs": r.Jobs, "models": r.Models,
		"budget": r.Budget, "skill_version": r.SkillVersion}
}

// ------------------------------------------- runs of begin/finish drivers ----

// sessionRun returns the run a begin/finish belongs to and its event log:
// $GOCOV_RUN_ID (a fan-out script's `gocov run-start`), else the open
// in-session run, else a new in-session run.
func sessionRun(o *opts, st *RepoState) (*RunRecord, *eventLog) {
	now := time.Now()
	if id := os.Getenv("GOCOV_RUN_ID"); id != "" {
		if r := st.runByID(id); r != nil && r.Ended == "" {
			return r, newEventLog(o, st.Name, id)
		}
	}
	for i := len(st.Runs) - 1; i >= 0; i-- {
		if r := &st.Runs[i]; r.Driver == driverSession && r.Ended == "" {
			return r, newEventLog(o, st.Name, r.ID)
		}
	}
	id := st.addRun(o, newRunID(), driverSession, 1, now)
	ev := newEventLog(o, st.Name, id)
	r := st.runByID(id)
	ev.emitAt(now, "run-start", runStartFields(r))
	return r, ev
}

// startSessionItem records that a one-at-a-time driver started it (begin).
// A begin repeated for an item already running in the same run keeps its start.
func startSessionItem(o *opts, st *RepoState, it *Item) *eventLog {
	r, ev := sessionRun(o, st)
	if it.Run == r.ID && it.StartedAt != "" && it.FinishedAt == "" {
		return ev
	}
	now := time.Now()
	it.Run, it.StartedAt, it.FinishedAt = r.ID, ts(now), ""
	it.Wave, it.Worker, it.Worktree, it.MergeSHA = st.nextWave(r.ID), 1, "", ""
	it.Branch = currentBranch(st.Path)
	ev.emitAt(now, "wave-start", map[string]any{"wave": it.Wave, "items": []string{it.ID}})
	ev.emitAt(now, "item-start", map[string]any{"item": it.ID, "worker": 1, "wave": it.Wave, "branch": it.Branch})
	return ev
}

// endSessionItem records the end of an item a begin/finish driver worked.
func endSessionItem(st *RepoState, it *Item, ev *eventLog) {
	now := time.Now()
	it.FinishedAt = ts(now)
	ev.emitAt(now, "item-end", map[string]any{"item": it.ID, "worker": it.Worker, "status": it.Status, "gained": it.Gained,
		"cost_usd": it.CostUSD, "reason": it.Reason})
	if r := st.runByID(it.Run); r != nil {
		st.measureRun(r, now)
	}
}

func currentBranch(root string) string {
	out, err := git(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func headSHA(root string) string {
	out, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// cmdRunStart opens a run record for a script driver and prints its ID.
func cmdRunStart(o *opts, driver string) error {
	if driver == "" {
		return fmt.Errorf("usage: gocov run-start -repo R -driver NAME")
	}
	_, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	now := time.Now()
	id := st.addRun(o, newRunID(), driver, 1, now)
	newEventLog(o, st.Name, id).emitAt(now, "run-start", runStartFields(st.runByID(id)))
	if err := saveState(o, st); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// cmdRunEnd closes a script driver's run record.
func cmdRunEnd(o *opts, id string) error {
	_, st, err := oneRepo(o)
	if err != nil {
		return err
	}
	r := st.runByID(id)
	if r == nil {
		return fmt.Errorf("no run %q in %s", id, statePath(o, st.Name))
	}
	if r.Ended == "" {
		st.endRun(id, newEventLog(o, st.Name, id), time.Now())
	}
	if err := saveState(o, st); err != nil {
		return err
	}
	r = st.runByID(id)
	fmt.Printf("run %s (%s): %d item(s), %.0fs wall, max concurrency observed %d\n", r.ID, r.Driver, r.Items, r.WallSec, r.MaxConcurrencyObserved)
	return nil
}

// closeSessionRuns ends the open in-session runs (gocov verify closes them).
func closeSessionRuns(o *opts, st *RepoState) {
	now := time.Now()
	for _, r := range st.Runs {
		if r.Driver == driverSession && r.Ended == "" {
			st.endRun(r.ID, newEventLog(o, st.Name, r.ID), now)
		}
	}
}

// ------------------------------------------------------------- reporting ----

func durStr(sec float64) string {
	return (time.Duration(sec*10) * time.Second / 10).String()
}

func clock(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "-"
	}
	return t.Format("15:04:05")
}

// writeRunSection adds FINAL_REPORT.md's "Run" section: the latest run in
// detail with its item timeline, and one line per earlier run.
func writeRunSection(sb *strings.Builder, st *RepoState) {
	if len(st.Runs) == 0 {
		return
	}
	r := st.Runs[len(st.Runs)-1]
	if r.Ended == "" {
		st.measureRun(&r, time.Now())
	}
	ended := r.Ended
	if ended == "" {
		ended = "not ended (still running, or killed - see events.jsonl)"
	}
	fmt.Fprintf(sb, "## Run\n\nLatest run `%s`: driver **%s**, skill %s, started %s, ended %s.\n\n", r.ID, r.Driver, r.SkillVersion, r.Started, ended)
	sb.WriteString("| Parallel | Jobs | Waves | Items | Wall time | Sum of item times | Speed-up | Max concurrency observed |\n|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(sb, "| %d | %d | %d | %d | %s | %s | %.2fx | %d |\n\n", r.Parallel, r.Jobs, r.Waves, r.Items, durStr(r.WallSec), durStr(r.ItemSecSum), r.Speedup, r.MaxConcurrencyObserved)
	sb.WriteString("Speed-up = sum of item times / wall time (1.0 = no overlap; measured, not the setting). Timeline: `events.jsonl`.\n\n")
	var items []Item
	for _, it := range st.Items {
		if it.Run == r.ID && it.StartedAt != "" {
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].StartedAt < items[j].StartedAt })
	if len(items) > 0 {
		sb.WriteString("| Wave | Worker | Item | Start | End | Status | Cost | Merge |\n|---:|---:|---|---|---|---|---:|---|\n")
		for _, it := range items {
			sha := it.MergeSHA
			if len(sha) > 10 {
				sha = sha[:10]
			}
			fmt.Fprintf(sb, "| %d | w%d | `%s` | %s | %s | %s | $%.2f | %s |\n", it.Wave, it.Worker, it.ID, clock(it.StartedAt), clock(it.FinishedAt), it.Status, it.CostUSD, sha)
		}
		sb.WriteString("\n")
	}
	if len(st.Runs) > 1 {
		sb.WriteString("Earlier runs:\n\n")
		for _, p := range st.Runs[:len(st.Runs)-1] {
			fmt.Fprintf(sb, "- `%s` %s, parallel %d: %d item(s), wall %s, max concurrency %d, ended %s\n", p.ID, p.Driver, p.Parallel, p.Items, durStr(p.WallSec), p.MaxConcurrencyObserved, dashIfEmpty(p.Ended))
		}
		sb.WriteString("\n")
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
