# gocov reference: commands, outputs, config

`gocov` is the stdlib-only Go program in `scripts/gocov/`. It does everything that needs no judgment (measure, de-duplicate profiles, rank, plan, enforce budgets and guard rails, track progress, verify goals) so it costs zero tokens and behaves the same on every OS. The model's only job is to author tests.

Build once: `cd <skill-dir>/scripts/gocov && go build -o ../bin/gocov .` (add `.exe` on Windows). Run its own tests with `go test ./...` in that directory.

## Commands

| Command | What it does | Writes tests? |
|---|---|---|
| `gocov report` | Finds every `go.mod`, runs a full `-coverpkg=./...` baseline per repo, writes `COVERAGE_REPORT.md`, `state.json`, `SUMMARY.md`. Refuses to overwrite a repo that already has progress unless `-force`. | no |
| `gocov plan` | Groups uncovered code by package, ranks by value score, queues only what the goal needs, writes `PLAN.md` (cost/time estimate, "goal likely unreachable" warning). Safe to re-run after progress (e.g. after a pilot): `done`/`blocked` items are kept, only the rest is re-ranked. | no |
| `gocov begin -repo R -item ID [-q]` | Checks out the work branch, measures the package, snapshots the tree, writes and prints the task prompt (`-q`: print only the prompt file path). Refuses while any non-test file differs from HEAD (e.g. an unrestored mutation). | no |
| `gocov finish -repo R -item ID [-abandon] [-cost USD]` | Verifies what was written; accepts, returns feedback, or with `-abandon` reverts and blocks the item. **Exit code 0 = accepted, 3 = not accepted** (feedback printed and saved to `work/<id>.feedback.md`). | no |
| `gocov queue -repo R` | Prints queued item IDs, one per line - for shell loops. | no |
| `gocov goal -repo R` | Prints the running estimate; exit 0 if it has reached the goal, 10 if not. | no |
| `gocov preflight` | Checks that `claude -p` can authenticate (one capped ~$0.05 call); exit 0/1. | no |
| `gocov run [-pilot N] [-dry-run] [-commit]` | Headless executor using `claude -p`. See `headless-mode.md`. | yes |
| `gocov review -repo R [-headless]` | Adversarial review: writes `review/PROMPT.md` for a fresh-context reviewer; with `-headless` runs one read-only `claude -p` reviewer and writes `REVIEW.md`. See `review-step.md`. | no |
| `gocov complexity -path DIR [-limit N]` | Lists functions over the complexity limit in production code and tests. | no |
| `gocov status` | Progress table across repos. | no |
| `gocov verify` | Fresh full run with the baseline's flags; sets the final status; writes `FINAL_REPORT.md`. | no |
| `gocov mark -repo R -item ID -status done\|blocked\|queued [-reason T]` | Manual bookkeeping. | no |

Common flags: `-config FILE` (multi-repo) or `-path DIR [-goal N] [-scope a,b]` (single repo), `-out DIR`, `-repo NAME` (one repo of a config).

## Outputs (`out_dir`, default `./gocov-out/<repo>/`)

`COVERAGE_REPORT.md` (baseline), `PLAN.md`, `REVIEW.md` (reviewer output), `FINAL_REPORT.md`, `state.json` (everything, resumable), `logs/` (go test and claude output), `work/` (per-item prompt and tree snapshot). `SUMMARY.md` in `out_dir` covers all repos. Keep `out_dir` out of the repo being tested.

## Item statuses

`queued` (will run) - `reserve` (ranked, not needed for the goal) - `done` - `blocked(reason)`.
Repo statuses: `baselined` - `planned` - `in-progress` - `achieved` | `needs-replan` | `not-achieved` | `failing-tests` | `paused-budget` | `paused-error`.
`achieved` needs the goal met **and** no package failing that passed at baseline; packages already failing at baseline are named as pre-existing. `failing-tests` = goal may be met, but a package that was green at baseline now fails (listed in `FINAL_REPORT.md`).

## Config keys

Top level: `out_dir`, `jobs` (repos in parallel), `min_uncovered_stmts` (skip smaller packages, default 15), `max_attempts` (default 2), `goal_margin_pct` (plan to goal+margin, default 3), `est_usd_per_item`, `est_min_per_item` (estimate inputs; calibrate with `-pilot`), `race` (run `-race` on touched packages; needs cgo), `allow_deps` (let an author touch go.mod/go.sum - off by default), `max_complexity` (default 15, `-1` off), `complexity_metric` (`cyclomatic`|`cognitive`|`both`).
`budget`: `max_usd_total`, `max_usd_per_repo`, `max_usd_per_item`, `item_timeout_min`, `time_limit_min`. `models`: `default`, `escalate`.
Per repo: `name`, `path`, `goal`, `goal_basis` (`adjusted` default | `raw`), `paths` (scope folders/files/globs), `test_flags` (e.g. `["-tags=unit"]`), `exclude` (globs of MACHINE-GENERATED files only), `branch` (work branch), `allow_non_git`.

Working example: `batch-config-example.json`.

## Metrics, precisely

- Always `-coverpkg=./...` for the baseline and the final verify; if it will not build (cgo, tags) gocov falls back and says so in the report.
- Profile blocks are de-duplicated and OR-ed across test binaries; totals are statement-weighted, never an average of percentages.
- `adjusted` removes generated and vendor code from the goal metric. `main` composition roots stay in it.
- An item's gain is counted per coverage block: only blocks still uncovered in the goal metric that the package's tests now hit. Statements other packages' tests already cover (visible through `-coverpkg`) earn nothing, and the task prompt lists only the lines still uncovered. The running estimate can therefore only be low, never high - the final verify also credits cross-package coverage by the new tests.
