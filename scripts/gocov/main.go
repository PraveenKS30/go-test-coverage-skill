// gocov - standalone Go coverage engine for the go-test-coverage skill.
//
// Everything that does not need judgment (measuring, ranking, planning, budget
// tracking, progress, goal verification) lives here so it costs zero tokens and
// behaves identically on Windows, macOS and Linux. The LLM is only used (via
// `claude -p`, or interactively in a session) to AUTHOR tests.
//
//	gocov report -config c.json     baseline + COVERAGE_REPORT.md (writes no tests)
//	gocov plan   -config c.json     ranked work items + cost estimate (PLAN.md)
//	gocov run    -config c.json     execute plan via `claude -p` with budget caps
//	gocov status -config c.json     progress table
//	gocov verify -config c.json     fresh full run, mark achieved / not achieved
//	gocov mark   -repo R -item ID -status done|blocked   (interactive bookkeeping)
//	gocov begin / finish / next-call / queue / goal      (building blocks for fan-out scripts)
//
// Single repo without a config file: gocov report -path DIR -goal 80 -scope a,b
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RepoCfg struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Goal        float64  `json:"goal"`       // target coverage %, e.g. 60 or 80
	GoalBasis   string   `json:"goal_basis"` // "adjusted" (default: generated+vendor excluded) | "raw"
	Paths       []string `json:"paths"`      // scope: folders/files/globs relative to repo root
	TestFlags   []string `json:"test_flags"` // e.g. ["-tags=unit"]
	Exclude     []string `json:"exclude"`    // extra globs of MACHINE-GENERATED files only
	Branch      string   `json:"branch"`     // work branch for generated tests (git repos)
	AllowNonGit bool     `json:"allow_non_git"`
}

type Config struct {
	OutDir string `json:"out_dir"`
	Jobs   int    `json:"jobs"` // repos processed in parallel
	Budget struct {
		MaxUSDTotal    float64 `json:"max_usd_total"`
		MaxUSDPerRepo  float64 `json:"max_usd_per_repo"`
		MaxUSDPerItem  float64 `json:"max_usd_per_item"`
		ItemTimeoutMin int     `json:"item_timeout_min"`
		TimeLimitMin   int     `json:"time_limit_min"`
	} `json:"budget"`
	Models struct {
		Default  string `json:"default"`
		Escalate string `json:"escalate"`
	} `json:"models"`
	MaxComplexity    int       `json:"max_complexity"`      // per-function limit, default 15; -1 disables
	ComplexityMetric string    `json:"complexity_metric"`   // cyclomatic | cognitive | both (default)
	MinUncovered     int       `json:"min_uncovered_stmts"` // skip packages with fewer uncovered stmts
	MaxAttempts      int       `json:"max_attempts"`
	GoalMargin       float64   `json:"goal_margin_pct"` // plan to goal+margin to absorb estimate error
	EstUSDPerItem    float64   `json:"est_usd_per_item"`
	EstMinPerItem    float64   `json:"est_min_per_item"`
	Race             bool      `json:"race"` // run -race on touched packages (needs cgo)
	AllowDeps        bool      `json:"allow_deps"`
	Repos            []RepoCfg `json:"repos"`
}

func (c *Config) defaults() {
	if c.OutDir == "" {
		c.OutDir = "gocov-out"
	}
	if c.Jobs <= 0 {
		c.Jobs = 2
	}
	if c.Budget.MaxUSDTotal == 0 {
		c.Budget.MaxUSDTotal = 20
	}
	if c.Budget.MaxUSDPerRepo == 0 {
		c.Budget.MaxUSDPerRepo = 10
	}
	if c.Budget.MaxUSDPerItem == 0 {
		c.Budget.MaxUSDPerItem = 1.5
	}
	if c.Budget.ItemTimeoutMin == 0 {
		c.Budget.ItemTimeoutMin = 15
	}
	if c.Models.Default == "" {
		c.Models.Default = "sonnet"
	}
	if c.Models.Escalate == "" {
		c.Models.Escalate = "opus"
	}
	if c.MaxComplexity == 0 {
		c.MaxComplexity = 15
	}
	if c.ComplexityMetric == "" {
		c.ComplexityMetric = "both"
	}
	if c.MinUncovered == 0 {
		c.MinUncovered = 15
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 2
	}
	if c.GoalMargin == 0 {
		c.GoalMargin = 3
	}
	if c.EstUSDPerItem == 0 {
		c.EstUSDPerItem = 0.6
	}
	if c.EstMinPerItem == 0 {
		c.EstMinPerItem = 4
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Goal == 0 {
			r.Goal = 60
		}
		if r.GoalBasis == "" {
			r.GoalBasis = "adjusted"
		}
		if r.Name == "" {
			r.Name = filepath.Base(filepath.Clean(r.Path))
		}
	}
}

type opts struct {
	cfg    *Config
	only   string
	pilot  int
	dryRun bool
	commit bool
	force  bool
	quiet  bool
}

func (o *opts) repos() []RepoCfg {
	var out []RepoCfg
	for _, r := range o.cfg.Repos {
		if o.only == "" || o.only == r.Name {
			out = append(out, r)
		}
	}
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: gocov <report|plan|run|begin|finish|next-call|queue|goal|preflight|review|complexity|status|verify|mark> [flags]
  -config FILE   JSON config (repos, goals, budgets). See references/batch-config-example.json
  -path DIR      single repo without a config file (use with -goal, -scope)
  -goal N        target coverage % for -path mode (default 60)
  -scope a,b     folders/files/globs (relative to repo root) for -path mode
  -out DIR       output dir (default gocov-out)
  -repo NAME     only this repo from the config
  -pilot N       (run) only the first N queued items per repo; prints cost per item
  -dry-run       (run) print the claude command + prompt for the first item, run nothing;
                 (begin, next-call) print as usual but save no state, check out no branch, snapshot nothing
  -commit        (run) git-commit each successful item
  -force         (report) overwrite state that already has progress
  queue -repo R                          print queued item IDs, one per line (for scripts)
  next-call -repo R -item ID -attempt K [-spent USD] [-elapsed SEC]
                                         (scripts) print "MODEL CAP_USD TIMEOUT_SEC WORKDIR" for the next claude call,
                                         from the config's models and budgets; exit 4 = budget/time limit reached, 5 = attempts used
  goal  -repo R                          exit 0 if the running estimate has reached the goal, 10 if not
  preflight                              check that claude -p can authenticate (exit 0/1)
  review -repo R [-headless]             adversarial review of the new tests: writes a packet for a fresh-context reviewer (or runs one claude -p)
  complexity -path DIR [-limit N]        list functions over the complexity limit (code and tests)
  begin  -repo R -item ID [-q]            (-q prints only the prompt file path; exit 3 = package does not build/test, item blocked)
  begin  -repo R -item ID                 (in-session mode) snapshot + print the task prompt
  finish -repo R -item ID [-abandon] [-cost USD]   verify the tests the session wrote
  mark -repo R -item ID -status done|blocked|queued [-reason TEXT]   (interactive mode bookkeeping)`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "")
	path := fs.String("path", "", "")
	goal := fs.Float64("goal", 60, "")
	scope := fs.String("scope", "", "")
	out := fs.String("out", "", "")
	only := fs.String("repo", "", "")
	pilot := fs.Int("pilot", 0, "")
	dry := fs.Bool("dry-run", false, "")
	commit := fs.Bool("commit", false, "")
	force := fs.Bool("force", false, "")
	item := fs.String("item", "", "")
	mstatus := fs.String("status", "", "")
	reason := fs.String("reason", "", "")
	abandon := fs.Bool("abandon", false, "")
	costF := fs.Float64("cost", 0, "")
	limitF := fs.Int("limit", 0, "")
	headlessF := fs.Bool("headless", false, "")
	quietF := fs.Bool("q", false, "")
	attemptF := fs.Int("attempt", 0, "")
	spentF := fs.Float64("spent", 0, "")
	elapsedF := fs.Int("elapsed", 0, "")
	fs.Usage = usage
	_ = fs.Parse(os.Args[2:])

	cfg := &Config{}
	switch {
	case *cfgPath != "":
		data, err := os.ReadFile(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			fatal(fmt.Errorf("parse %s: %w", *cfgPath, err))
		}
	case *path != "":
		r := RepoCfg{Path: *path, Goal: *goal}
		if *scope != "" {
			r.Paths = strings.Split(*scope, ",")
		}
		cfg.Repos = []RepoCfg{r}
	default:
		usage()
		os.Exit(2)
	}
	cfg.defaults()
	if *out != "" {
		cfg.OutDir = *out
	}
	for i := range cfg.Repos {
		abs, err := filepath.Abs(cfg.Repos[i].Path)
		if err != nil {
			fatal(err)
		}
		cfg.Repos[i].Path = abs
	}
	o := &opts{cfg: cfg, only: *only, pilot: *pilot, dryRun: *dry, commit: *commit, force: *force, quiet: *quietF}
	if len(o.repos()) == 0 {
		fatal(fmt.Errorf("no repos selected"))
	}
	ctx := context.Background()
	var err error
	switch cmd {
	case "report":
		err = cmdReport(ctx, o)
	case "plan":
		err = cmdPlan(o)
	case "run":
		err = cmdRun(ctx, o)
	case "status":
		err = cmdStatus(o)
	case "verify":
		err = cmdVerify(ctx, o)
	case "next-call":
		err = cmdNextCall(o, *item, *attemptF, *spentF, *elapsedF)
	case "queue":
		err = cmdQueue(o)
	case "goal":
		err = cmdGoal(o)
	case "preflight":
		err = authProbe(ctx)
	case "review":
		err = cmdReview(ctx, o, *headlessF)
	case "complexity":
		err = cmdComplexity(o, *limitF)
	case "begin":
		err = cmdBegin(ctx, o, *item)
	case "finish":
		err = cmdFinish(ctx, o, *item, *abandon, *costF)
	case "mark":
		err = cmdMark(o, *item, *mstatus, *reason)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// exitCode lets finish/goal signal a result to calling scripts without being an error.
var exitCode int

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gocov: error:", err)
	os.Exit(1)
}

// ---------------------------------------------------------------- state ----

type Item struct {
	ID            string   `json:"id"`
	Module        string   `json:"module"`
	Pkg           string   `json:"pkg"`
	Files         []string `json:"files"`
	Stmts         int      `json:"stmts"`
	Uncovered     int      `json:"uncovered"`
	Score         float64  `json:"score"`
	Hard          bool     `json:"hard"`
	ProjectedGain int      `json:"projected_gain"`
	Status        string   `json:"status"` // queued | reserve | done | blocked
	Reason        string   `json:"reason,omitempty"`
	Attempts      int      `json:"attempts"`
	CostUSD       float64  `json:"cost_usd"`
	PkgBefore     float64  `json:"pkg_before_pct"`
	PreOver       []string `json:"pre_over_complexity,omitempty"`
	PreCovered    int      `json:"pre_covered"`
	PkgAfter      float64  `json:"pkg_after_pct"`
	Gained        int      `json:"stmts_gained"`
	TestFiles     []string `json:"test_files,omitempty"`
}

type HistoryEntry struct {
	Time  string  `json:"time"`
	Event string  `json:"event"`
	Pct   float64 `json:"goal_pct"`
}

type RepoState struct {
	Name          string              `json:"name"`
	Path          string              `json:"path"`
	Goal          float64             `json:"goal"`
	GoalBasis     string              `json:"goal_basis"`
	Scope         []string            `json:"scope"`
	Modules       []Module            `json:"modules"`
	Status        string              `json:"status"` // baselined | planned | in-progress | achieved | not-achieved | needs-replan | paused-budget | blocked
	Reason        string              `json:"reason,omitempty"`
	Warnings      []string            `json:"warnings,omitempty"`
	BaseRaw       Totals              `json:"baseline_raw"`
	BaseAdj       Totals              `json:"baseline_adjusted"`
	BaseGoal      Totals              `json:"baseline_goal"`
	Rows          []FileCov           `json:"rows"`
	UncovBlocks   map[string][]UBlock `json:"uncovered_blocks"`           // nil only in state written before v2.5; goal-relevant uncovered blocks per file; the gain of an item is the stmts of these it newly hits
	BaseFailing   []string            `json:"baseline_failing,omitempty"` // packages failing at baseline (pre-existing)
	Failing       []string            `json:"failing,omitempty"`          // packages failing in the latest full run
	Items         []Item              `json:"items"`
	SpendUSD      float64             `json:"spend_usd"`
	Verified      bool                `json:"verified"` // last Current figure came from a fresh full run
	History       []HistoryEntry      `json:"history"`
	ComplexProd   []FuncComplexity    `json:"complex_prod,omitempty"`
	ComplexTests  []FuncComplexity    `json:"complex_tests,omitempty"`
	LintHints     []string            `json:"lint_hints,omitempty"`
	BuildTagTests int                 `json:"build_tagged_test_files"`
	ProjectedPct  float64             `json:"plan_projected_pct,omitempty"`
}

// goalTotals is the figure the goal is judged against.
func (s *RepoState) goalTotals() Totals {
	return sumRows(s.Rows, s.Scope, s.GoalBasis != "raw")
}

// absorb adds one module's full-run measurement to the state.
func (s *RepoState) absorb(m *Measurement) {
	s.Rows = append(s.Rows, m.Rows...)
	if s.UncovBlocks == nil {
		s.UncovBlocks = map[string][]UBlock{}
	}
	for f, bs := range m.UncovB {
		s.UncovBlocks[f] = bs
	}
	s.Failing = append(s.Failing, m.Failing...)
}

func (s *RepoState) log(event string) {
	s.History = append(s.History, HistoryEntry{Time: time.Now().Format(time.RFC3339), Event: event, Pct: s.goalTotals().Pct()})
}

func repoDir(o *opts, name string) string { return filepath.Join(o.cfg.OutDir, name) }

func statePath(o *opts, name string) string { return filepath.Join(repoDir(o, name), "state.json") }

func loadState(o *opts, name string) (*RepoState, error) {
	data, err := os.ReadFile(statePath(o, name))
	if err != nil {
		return nil, fmt.Errorf("no state for repo %q (run `gocov report` first): %w", name, err)
	}
	s := &RepoState{}
	return s, json.Unmarshal(data, s)
}

func saveState(o *opts, s *RepoState) error {
	if err := os.MkdirAll(repoDir(o, s.Name), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := statePath(o, s.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(o, s.Name))
}
