# gocov reference: commands, outputs, config

`gocov` is the stdlib-only Go program in `scripts/gocov/`. It does everything that needs no judgment (measure, de-duplicate profiles, rank, plan, enforce budgets and guard rails, track progress, verify goals) so it costs zero tokens and behaves the same on every OS. The model's only job is to author tests.

Build once: `cd <skill-dir>/scripts/gocov && go build -o ../bin/gocov .` (add `.exe` on Windows). Run its own tests with `go test ./...` in that directory.

## Commands

| Command | What it does | Writes tests? |
|---|---|---|
| `gocov report` | Finds every `go.mod`, runs a full `-coverpkg=./...` baseline per repo, writes `COVERAGE_REPORT.md`, `state.json`, `SUMMARY.md`. Refuses to overwrite a repo that already has progress unless `-force`. | no |
| `gocov plan` | Groups uncovered code by package, ranks by value score, queues only what the goal needs, writes `PLAN.md` (cost/time estimate, "goal likely unreachable" warning). Safe to re-run after progress (e.g. after a pilot): `done`/`blocked` items are kept, only the rest is re-ranked. | no |
| `gocov begin -repo R -item ID [-q]` | Checks out the work branch, measures the package, snapshots the tree, writes and prints the task prompt (`-q`: print only the prompt file path). Refuses while any non-test file differs from HEAD (e.g. an unrestored mutation). **Exit 3 = the package does not build/test**: the item is marked blocked. With `-dry-run` it only writes `work/<id>.dry-run.prompt.md`: no state save, branch checkout or snapshot. | no |
| `gocov next-call -repo R -item ID -attempt K [-spent USD] [-elapsed SEC]` | For fan-out scripts: prints `MODEL CAP_USD TIMEOUT_SEC WORKDIR` for the next `claude -p` call by the same rules as `gocov run` (`-spent`/`-elapsed` are the script's run totals). **Exit 4 = budget or time limit reached** (repo marked `paused-budget`, except with `-dry-run`), **5 = `max_attempts` used**. | no |
| `gocov finish -repo R -item ID [-abandon] [-cost USD] [-model M] [-cap USD]` | Verifies what was written (`-model`/`-cap` only go into the attempt log); accepts, returns feedback, or with `-abandon` reverts and blocks the item. **Exit code 0 = accepted, 3 = not accepted** (feedback printed and saved to `work/<id>.feedback.md`). | no |
| `gocov queue -repo R` | Prints queued item IDs, one per line - for shell loops. | no |
| `gocov goal -repo R` | Prints the running estimate; exit 0 if it has reached the goal, 10 if not. | no |
| `gocov preflight` | Checks that `claude -p` can authenticate (one capped ~$0.05 call); exit 0/1. | no |
| `gocov run [-pilot N] [-dry-run] [-commit] [-parallel N]` | Headless executor using `claude -p`. `-parallel N` works up to N packages of a repo at once, each in its own git worktree (one cap across all repos; `-commit` implied). See `headless-mode.md`. | yes |
| `gocov run-start -repo R -driver NAME` / `gocov run-end -repo R -run ID` | For scripts: open a run record and print its ID (export it as `GOCOV_RUN_ID` so `begin`/`finish` record items under it), then close it with its measured times and concurrency. | no |
| `gocov review -repo R [-headless]` | Adversarial review: writes `review/PROMPT.md` for a fresh-context reviewer; with `-headless` runs one read-only `claude -p` reviewer and writes `REVIEW.md`. See `review-step.md`. | no |
| `gocov complexity -path DIR [-limit N]` | Lists functions over the complexity limit in production code and tests. | no |
| `gocov status` | Progress table across repos, the latest run's measured concurrency, and items in progress with their worker slot. | no |
| `gocov verify` | Fresh full run with the baseline's flags; sets the final status; writes `FINAL_REPORT.md`; closes an open in-session run record. | no |
| `gocov mark -repo R -item ID -status done\|blocked\|queued [-reason T]` | Manual bookkeeping. | no |

Common flags: `-config FILE` (multi-repo) or `-path DIR [-goal N] [-scope a,b]` (single repo), `-out DIR`, `-repo NAME` (one repo of a config).

## Outputs (`out_dir`, default `./gocov-out/<repo>/`)

`COVERAGE_REPORT.md` (baseline), `PLAN.md`, `REVIEW.md` (reviewer output), `FINAL_REPORT.md` (with a "Run" section: parallel N, waves, wall time, sum of item times, speed-up, max concurrency observed, per-item timeline), `state.json` (everything, resumable), `events.jsonl` (run timeline), `logs/` (go test and claude output, per item), `work/` (per-item prompt and tree snapshot), `wt/` (per-item git worktrees while a `-parallel` run works on them; removed after each item). `SUMMARY.md` in `out_dir` covers all repos. Keep `out_dir` out of the repo being tested.

### Run tracking

`state.json` `runs[]` - one record per driver invocation: `id`, `started`, `ended` (empty while running or if killed), `driver` (`gocov run` | `fanout.sh` | `fanout.ps1` | `in-session begin-finish`), `parallel`, `jobs`, `models`, `budget`, `skill_version`, `status`, and measured from the items' start/end times: `items`, `waves`, `wall_sec`, `item_sec_sum`, `speedup` (= item_sec_sum / wall_sec), `max_concurrency_observed`.

Per item: `run`, `started_at`, `finished_at`, `wave`, `worker` (slot), `worktree`, `branch`, `merge_sha`, and `attempt_log[]` (`n`, `run`, `worker`, `model`, `cap_usd`, `cost_usd`, `started`, `ended`, `error`). A `-parallel` run numbers waves of up to N items; a one-at-a-time driver makes each item a wave of one on worker 1.

`events.jsonl` - append-only, one JSON object per line, never rewritten (a killed run leaves a parseable trace). Every event has `t` (RFC 3339), `run` and `ev`:

| `ev` | Other fields |
|---|---|
| `run-start` | `driver`, `parallel`, `jobs`, `models`, `budget`, `skill_version` |
| `wave-start` | `wave`, `items` (IDs) |
| `item-start` | `item`, `worker`, `wave`, `worktree`, `branch` |
| `attempt` | `item`, `worker`, `attempt`, `model`, `cap_usd`, `cost_usd`, `started`, `error` (written when the call ends) |
| `item-end` | `item`, `worker`, `status` (`done`, `blocked`, `queued` again as `requeued`, or `paused`), `gained`, `cost_usd`, `reason` |
| `merge` | `item`, `sha` (its commit on the work branch) |
| `merge-conflict` | `item`, `error` |
| `checkpoint` | `wave`, `verified_pct`, `goal` |
| `run-end` | `status`, `item_count`, `waves`, `wall_sec`, `item_sec_sum`, `speedup`, `max_concurrency_observed` |

## Item statuses

`queued` (will run; while it runs it has `started_at` and no `finished_at`) - `reserve` (ranked, not needed for the goal) - `done` - `blocked(reason)`.
Repo statuses: `baselined` - `planned` - `in-progress` - `achieved` | `needs-replan` | `not-achieved` | `failing-tests` | `paused-budget` | `paused-error` (its reason says "paused: rate or usage limit" when claude was limited mid-run).
A verify below the goal with items still `queued` (after `-pilot`, a budget pause, or a script stopped early) does not judge the run: the repo stays `in-progress` (or `planned` / `paused-*`) with "N queued item(s) not run yet" - re-run to continue. `needs-replan` = queue empty, `reserve` packages untried; `not-achieved` = nothing left to try.
`achieved` needs the goal met **and** no package failing that passed at baseline; packages already failing at baseline are named as pre-existing. `failing-tests` = goal may be met, but a package that was green at baseline now fails (listed in `FINAL_REPORT.md`).

## Config keys

Top level: `out_dir`, `jobs` (repos in parallel), `parallel` (packages of one repo in parallel in `gocov run`, each in its own worktree; default 1; one cap across all repos; not allowed with `allow_deps`), `min_uncovered_stmts` (skip smaller packages, default 15), `max_attempts` (default 2), `goal_margin_pct` (plan to goal+margin, default 3), `est_usd_per_item`, `est_min_per_item` (estimate inputs; calibrate with `-pilot`), `race` (run `-race` on touched packages; needs cgo), `allow_deps` (let an author touch go.mod/go.sum - off by default), `max_complexity` (default 15, `-1` off), `complexity_metric` (`cyclomatic`|`cognitive`|`both`).
`budget`: `max_usd_total`, `max_usd_per_repo`, `max_usd_per_item`, `item_timeout_min` (fractions allowed, e.g. `0.5` = 30 s), `time_limit_min`. `models`: `default`, `escalate`.
Per repo: `name`, `path`, `goal`, `goal_basis` (`adjusted` default | `raw`), `paths` (scope folders/files/globs), `test_flags` (e.g. `["-tags=unit"]`), `exclude` (globs of MACHINE-GENERATED files only), `branch` (work branch), `allow_non_git`.

Working example: `batch-config-example.json`.

## Metrics, precisely

- Always `-coverpkg=./...` for the baseline and the final verify; if it will not build (cgo, tags) gocov falls back and says so in the report.
- Profile blocks are de-duplicated and OR-ed across test binaries; totals are statement-weighted, never an average of percentages.
- `adjusted` removes generated and vendor code from the goal metric. `main` composition roots stay in it.
- An item's gain is counted per coverage block: only blocks still uncovered in the goal metric that the package's tests now hit. Statements other packages' tests already cover (visible through `-coverpkg`) earn nothing, and the task prompt lists only the lines still uncovered. The running estimate can therefore only be low, never high - the final verify also credits cross-package coverage by the new tests.
