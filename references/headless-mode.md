# Headless mode (unattended, CI, very large batches)

One fresh `claude -p` per package, driven by a script - the "fan out across files" pattern from Claude Code's best-practices guide. **A script, not a model, runs the loop and the commands.** Two equivalent drivers:

- `scripts/fanout.sh` / `scripts/fanout.ps1` - short, readable loops you can edit (recommended).
- `gocov run` - the same loop built into the Go engine; it adds parallel repos (`jobs`), parallel packages within a repo (`-parallel N`, each in its own git worktree) and `-commit`.

Both start one fresh `claude -p` per package and apply the same rules from the same config: models and escalation, per-call/repo/run caps, `time_limit_min`, `item_timeout_min`, `max_attempts`, blocking packages that do not build, and verifying before stopping early. The scripts get them from `gocov next-call`, so the rules live in one place. Use it when nobody is watching or the batch is too large for one conversation. Inside Claude Desktop / CLI prefer `in-session-mode.md`.

## Authentication - read this first

A child `claude -p` process **cannot borrow** the login of the Claude Desktop app or of the session that launched it (the host refreshes that login over a private channel). It needs one of:

1. The standalone CLI logged in: `claude auth status` shows `loggedIn: true` (one-time `claude` then `/login`).
2. `ANTHROPIC_API_KEY` (a Console API key) - the right choice for CI.

`gocov run` starts with a ~$0.05 probe. If auth fails it stops in seconds with these options (status `paused-error`, attempts not consumed). **If you are inside a session when this happens, switch to in-session mode rather than asking the user to log in.**

Once the probe has passed, a call that fails without spending anything is not a login problem: the repo pauses as `paused-error` with the reason "paused: rate or usage limit", the item stays queued, and a re-run later resumes. A rate-limit error (HTTP 429, "rate limit", "overloaded") makes every worker back off together (30 s, doubling, up to 3 times) and retries the same attempt without using it up; still limited after that, the repo pauses the same way.

## Fan-out scripts (the article pattern)

Prerequisite: `gocov report` and `gocov plan` done and the plan approved (the scripts never write tests before that).

```bash
# bash / Git Bash / WSL / macOS / Linux
scripts/fanout.sh -c cfg.json -r REPO -n 3        # pilot: first 3 items
scripts/fanout.sh -c cfg.json -r REPO -v          # everything queued, headless review at the end
scripts/fanout.sh -c cfg.json -r REPO --dry-run   # print the claude command + prompt only
```

```powershell
# Windows PowerShell
scripts\fanout.ps1 -Config C:\work\cfg.json -Repo REPO -Pilot 3
scripts\fanout.ps1 -Config C:\work\cfg.json -Repo REPO -Review
```

Options: pilot N, review, dry-run (prints the first item's claude command and prompt; it saves no state, checks out no branch and leaves the tree alone, so the real run still starts from a clean tree on the work branch). Everything else comes from the config (`models`, `budget`, `max_attempts`); to spend less, lower `budget.max_usd_total` there. The loop is, per queued item:

0. `gocov run-start -driver fanout.sh` (or `fanout.ps1`) - opens a run record and exports its ID as `GOCOV_RUN_ID`, so `begin`/`finish` record each item under it; `gocov run-end` closes it however the script exits.
1. `gocov goal` - when the running estimate reaches the goal, `gocov verify`; stop if the verified figure meets it, otherwise carry on (as `gocov run` does).
2. `gocov next-call -item ID -attempt K -spent USD -elapsed SEC` - prints `MODEL CAP_USD TIMEOUT_SEC WORKDIR`: `models.default`, or `models.escalate` on a retry, for cgo/exec/syscall code, or above 300 uncovered statements; the cap is the smallest of the per-item cap and what is left of the repo and run budgets. **Exit 4 = budget or time limit reached** (the repo becomes `paused-budget`, remaining items stay queued, the script stops); **exit 5 = `max_attempts` used** (the script abandons the item).
3. `gocov begin -q` - snapshot the tree, print only the prompt file path. **Exit 3 = the package does not build/test**: gocov marks it blocked and the script moves on.
4. `claude -p --output-format json --model MODEL --permission-mode dontAsk --max-budget-usd CAP --no-session-persistence --allowedTools "<pre-approved list>" < prompt`, run in the item's module dir and killed after TIMEOUT_SEC (`item_timeout_min`) so a hung call cannot block the run. A killed call reports no cost, so the script charges its whole cap and says so.
5. `gocov finish -cost <spent> -model M -cap USD` - exit 0 = accepted; **exit 3 = rejected, feedback is in `work/<id>.feedback.md`** and is appended to the next attempt's prompt; when next-call says the attempts are used, `gocov finish -abandon` reverts and blocks the item. If the budget runs out mid-item the item is abandoned and the run stops, as in `gocov run`.
6. After the loop: `gocov verify` (fresh full run; skipped if step 1 already verified the goal), optional `gocov review -headless`.

The scripts first run `gocov preflight`, so a missing login fails in seconds. If `claude -p` returns an error without spending anything (auth/API), the script exits and the item stays queued. Helper commands for your own scripts: `gocov queue` (queued IDs), `gocov goal` (exit 0 reached / 10 not), `gocov next-call` (model, cap, timeout, work dir; exit 4 stop / 5 attempts used), `gocov preflight`, `gocov begin -q` (exit 3 = blocked), `gocov finish` (exit 3 = not accepted).

## `gocov run` commands

```bash
gocov run -config cfg.json -pilot 3      # first 3 items per repo, then prints real $/item and stmts gained per $1
gocov run -config cfg.json -dry-run      # print the exact claude command and prompt, run nothing
gocov run -config cfg.json               # everything queued; resumable, safe to interrupt
gocov run -config cfg.json -repo NAME    # one repo of the config
gocov run -config cfg.json -commit       # one local commit per successful item, holding only its test files (never pushes)
gocov run -config cfg.json -parallel 4   # up to 4 packages at once, each in its own worktree (see below)
```

Always pilot first, copy the observed `$ per item` into `est_usd_per_item`, re-run `gocov plan` (the pilot's done/blocked items are kept), then approve the full run.

## Parallel packages in one repo (`gocov run -parallel N`)

`parallel` (config key, or `-parallel N`) defaults to **1**: one package at a time in the repo's own tree, exactly as before. With N > 1:

- **Unit of work stays one package.** A package is never split across workers: its test files share one namespace.
- **One cap across all repos.** Worker slots come from a single pool of N shared by every repo of the run, so `jobs` x N never multiplies: `jobs: 2, parallel: 4` means at most 4 claude calls at once, not 8.
- **Dependency-aware waves.** `go list -e -json ./...` runs once per module. Each wave takes up to N queued items in plan order such that none imports another (directly, transitively, or through its tests): their coverage overlaps, so they go in different waves.
- **One git worktree per item**, cut from the tip of the work branch (`<out_dir>/<repo>/wt/<id>`, branch `cov/<id>`). Measuring, the snapshot, the `claude -p` call (its working directory is the worktree), the revert of production edits, the flake/vet/complexity checks and the mutation check all happen there, so one worker's mutation or stray edit can never reach another. Logs stay per item in `logs/`.
- **One owner of the state.** A single coordinator goroutine per repo owns `state.json`; workers report results and never save.
- **Serial merge.** An accepted item's test files are committed in its worktree and cherry-picked onto the work branch one at a time (so `-commit` is implied). If the cherry-pick conflicts the item is queued again and re-runs from the new tip; a second conflict blocks it. The worktree and its branch are removed afterwards.
- **Budget: reserve, then settle.** Each dispatched item's first call reserves its cap from the run and repo budgets before the wave starts; every call settles to its real cost when it ends. The caps of the calls in flight can never add up to more than what is left. A wave gets fewer items when the budget cannot cover all of them.
- **Checkpoint after each wave.** A fresh `-coverpkg` full run on the work branch credits indirect coverage. Once that verified figure reaches the goal no further wave is dispatched (items already in flight always finish first).
- **Go builds are throttled**: `GOFLAGS=-p=<cores/N>` for the workers and their claude calls (unless `GOFLAGS` already sets `-p`).
- **Requirements**: a git repo with a clean working tree (each item lands as a commit, so uncommitted changes would not be in the worktrees); `allow_deps` is refused with N > 1 (concurrent `go.mod` edits would conflict).
- **Choosing N**: around 4. More runs into rate limits and CPU contention, and overshoots the goal by a wave.

## Was it really parallel? What a run writes down

Every driver records its runs the same way, so you can tell from the files how a run went:

- **`state.json` `runs[]`**: one record per invocation - `id`, `started`/`ended`, `driver` (`gocov run`, `fanout.sh`, `fanout.ps1`, or `in-session begin-finish`), `parallel`, `jobs`, `models`, `budget`, `skill_version`, the repo `status` at the end, and what was **measured** from the items' own start/end times: `items`, `waves`, `wall_sec`, `item_sec_sum`, `speedup` (item_sec_sum / wall_sec) and `max_concurrency_observed`. A record with no `ended` is a run that is still going or was killed.
- **Per item**: `run`, `started_at`, `finished_at`, `wave`, `worker` (slot 1..N), `worktree`, `branch`, `merge_sha` (its commit on the work branch) and `attempt_log` (model, cap, cost, start/end, error of every call).
- **`<out_dir>/<repo>/events.jsonl`**: an append-only timeline, one JSON object per line, written once and never rewritten, so a crashed run still leaves a readable trace. Events: `run-start`, `wave-start`, `item-start`, `attempt`, `item-end`, `merge`, `merge-conflict`, `checkpoint`, `run-end` (fields in `gocov-reference.md`).
- **`gocov status`** shows the latest run's driver, parallel setting and measured concurrency, and, while it runs, each item in progress with its worker slot. Console lines carry the slot: `[demo w3] root:pkg/x attempt 1/2 ...`.
- **`FINAL_REPORT.md`** has a "Run" section: parallel N, jobs, waves, wall time, sum of item times, speed-up, max concurrency observed, and a per-item timeline (wave, worker, start, end, status, cost, merge commit).

## What the script enforces (not the model)

- Permissions: the documented unattended pattern - `--allowedTools` pre-approves only `Read, Edit, Write, Glob, Grep` and the `go test|vet|build|list`, `gofmt`, `git diff|status` shell commands, and `--permission-mode dontAsk` denies everything else instead of prompting. The script, not the prompt, is the safety boundary.
- One package per call; the prompt is only the working directory (the module root, where `claude -p` runs), the package path relative to it, uncovered line ranges and the rules.
- Model: `models.default` first; `models.escalate` on retry, for cgo/exec/syscall code, or packages with more than 300 uncovered statements.
- Budgets: `--max-budget-usd` per call (the smallest of per-item cap, repo remaining, run remaining, in whole cents) plus `max_usd_per_repo`, `max_usd_total`, `time_limit_min` (checked before each item), `item_timeout_min` (each call is killed after it). With `jobs` > 1 or `parallel` > 1 each call reserves its cap from the run and repo budgets before it starts and settles to its real cost after, so calls running at the same time are never granted the same dollars. A call killed at `item_timeout_min` reports no cost; it is charged its whole cap (the most it may have spent) and the charge is logged. At a cap the repo pauses as `paused-budget`; raise the cap and re-run to resume. "Run" means one `gocov run` or one script invocation; repo spend is kept in `state.json` across runs. This `claude` version has no `--max-turns`; the USD cap and timeout bound each call.
- Only `_test.go` and `testdata/` edits survive. Production edits and `go.mod`/`go.sum` (unless `allow_deps`) are reverted automatically; failed items are cleaned out of the tree.
- Acceptance per item: tests green, `-count=3 -shuffle=on` flake and order check, `go vet` clean in the new test files (findings in production code or older tests are not the author's and do not block), no new over-limit function (`complexity.md`), at least one newly covered statement. Max `max_attempts` (default 2) then `blocked(reason)`.
- Early stop: when the running estimate reaches the goal it runs a fresh full verify; if the verified number is below goal it carries on.
- Repos run in parallel (`jobs`), each on its own branch (`branch`); a dirty working tree is refused so generated work stays separable. Packages of one repo run in parallel only with `parallel` > 1, each in its own worktree (above).

## Cost controls checklist

Deterministic work is script-only; estimate shown at `plan`; pilot before bulk; packages under `min_uncovered_stmts` are never queued; one small prompt per package; bounded retries; hard caps at item/repo/run/time level; every result checkpointed in `state.json`.

## Limits worth knowing

- A call killed at the timeout reports no cost, so it is charged its whole cap. That can overstate the spend of a call that was killed early in a cheap turn; it never understates it.
- With `-commit`, a commit that fails (a hook, no git identity) leaves the accepted tests uncommitted and is recorded as a warning in `state.json` and the final report.
- In-flight coverage is an estimate until `gocov verify` (or the early-stop verify) replaces it with a fresh full run.
- `-race` needs cgo; set `race: true` only where a C toolchain exists.
