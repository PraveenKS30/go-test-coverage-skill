# Headless mode (unattended, CI, very large batches)

One fresh `claude -p` per package, driven by a script - the "fan out across files" pattern from Claude Code's best-practices guide. **A script, not a model, runs the loop and the commands.** Two equivalent drivers:

- `scripts/fanout.sh` / `scripts/fanout.ps1` - short, readable loops you can edit (recommended).
- `gocov run` - the same loop built into the Go engine; it adds parallel repos (`jobs`) and `-commit`.

Both start one fresh `claude -p` per package and apply the same rules from the same config: models and escalation, per-call/repo/run caps, `time_limit_min`, `item_timeout_min`, `max_attempts`, blocking packages that do not build, and verifying before stopping early. The scripts get them from `gocov next-call`, so the rules live in one place. Use it when nobody is watching or the batch is too large for one conversation. Inside Claude Desktop / CLI prefer `in-session-mode.md`.

## Authentication - read this first

A child `claude -p` process **cannot borrow** the login of the Claude Desktop app or of the session that launched it (the host refreshes that login over a private channel). It needs one of:

1. The standalone CLI logged in: `claude auth status` shows `loggedIn: true` (one-time `claude` then `/login`).
2. `ANTHROPIC_API_KEY` (a Console API key) - the right choice for CI.

`gocov run` starts with a ~$0.05 probe. If auth fails it stops in seconds with these options (status `paused-error`, attempts not consumed). **If you are inside a session when this happens, switch to in-session mode rather than asking the user to log in.**

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

1. `gocov goal` - when the running estimate reaches the goal, `gocov verify`; stop if the verified figure meets it, otherwise carry on (as `gocov run` does).
2. `gocov next-call -item ID -attempt K -spent USD -elapsed SEC` - prints `MODEL CAP_USD TIMEOUT_SEC WORKDIR`: `models.default`, or `models.escalate` on a retry, for cgo/exec/syscall code, or above 300 uncovered statements; the cap is the smallest of the per-item cap and what is left of the repo and run budgets. **Exit 4 = budget or time limit reached** (the repo becomes `paused-budget`, remaining items stay queued, the script stops); **exit 5 = `max_attempts` used** (the script abandons the item).
3. `gocov begin -q` - snapshot the tree, print only the prompt file path. **Exit 3 = the package does not build/test**: gocov marks it blocked and the script moves on.
4. `claude -p --output-format json --model MODEL --permission-mode dontAsk --max-budget-usd CAP --no-session-persistence --allowedTools "<pre-approved list>" < prompt`, run in the item's module dir and killed after TIMEOUT_SEC (`item_timeout_min`) so a hung call cannot block the run. A killed call reports no cost, so the script charges its whole cap and says so.
5. `gocov finish -cost <spent>` - exit 0 = accepted; **exit 3 = rejected, feedback is in `work/<id>.feedback.md`** and is appended to the next attempt's prompt; when next-call says the attempts are used, `gocov finish -abandon` reverts and blocks the item. If the budget runs out mid-item the item is abandoned and the run stops, as in `gocov run`.
6. After the loop: `gocov verify` (fresh full run; skipped if step 1 already verified the goal), optional `gocov review -headless`.

The scripts first run `gocov preflight`, so a missing login fails in seconds. If `claude -p` returns an error without spending anything (auth/API), the script exits and the item stays queued. Helper commands for your own scripts: `gocov queue` (queued IDs), `gocov goal` (exit 0 reached / 10 not), `gocov next-call` (model, cap, timeout, work dir; exit 4 stop / 5 attempts used), `gocov preflight`, `gocov begin -q` (exit 3 = blocked), `gocov finish` (exit 3 = not accepted).

## `gocov run` commands

```bash
gocov run -config cfg.json -pilot 3      # first 3 items per repo, then prints real $/item and stmts gained per $1
gocov run -config cfg.json -dry-run      # print the exact claude command and prompt, run nothing
gocov run -config cfg.json               # everything queued; resumable, safe to interrupt
gocov run -config cfg.json -repo NAME    # one repo of the config
gocov run -config cfg.json -commit       # one local commit per successful item, holding only its test files (never pushes)
```

Always pilot first, copy the observed `$ per item` into `est_usd_per_item`, re-run `gocov plan` (the pilot's done/blocked items are kept), then approve the full run.

## What the script enforces (not the model)

- Permissions: the documented unattended pattern - `--allowedTools` pre-approves only `Read, Edit, Write, Glob, Grep` and the `go test|vet|build|list`, `gofmt`, `git diff|status` shell commands, and `--permission-mode dontAsk` denies everything else instead of prompting. The script, not the prompt, is the safety boundary.
- One package per call; the prompt is only the working directory (the module root, where `claude -p` runs), the package path relative to it, uncovered line ranges and the rules.
- Model: `models.default` first; `models.escalate` on retry, for cgo/exec/syscall code, or packages with more than 300 uncovered statements.
- Budgets: `--max-budget-usd` per call (the smallest of per-item cap, repo remaining, run remaining, in whole cents) plus `max_usd_per_repo`, `max_usd_total`, `time_limit_min` (checked before each item), `item_timeout_min` (each call is killed after it). With `jobs` > 1 each call reserves its cap from the run budget before it starts and settles to its real cost after, so parallel repos are never granted the same dollars. A call killed at `item_timeout_min` reports no cost; it is charged its whole cap (the most it may have spent) and the charge is logged. At a cap the repo pauses as `paused-budget`; raise the cap and re-run to resume. "Run" means one `gocov run` or one script invocation; repo spend is kept in `state.json` across runs. This `claude` version has no `--max-turns`; the USD cap and timeout bound each call.
- Only `_test.go` and `testdata/` edits survive. Production edits and `go.mod`/`go.sum` (unless `allow_deps`) are reverted automatically; failed items are cleaned out of the tree.
- Acceptance per item: tests green, `-count=3 -shuffle=on` flake and order check, `go vet` clean in the new test files (findings in production code or older tests are not the author's and do not block), no new over-limit function (`complexity.md`), at least one newly covered statement. Max `max_attempts` (default 2) then `blocked(reason)`.
- Early stop: when the running estimate reaches the goal it runs a fresh full verify; if the verified number is below goal it carries on.
- Repos run in parallel (`jobs`), each on its own branch (`branch`); a dirty working tree is refused so generated work stays separable.

## Cost controls checklist

Deterministic work is script-only; estimate shown at `plan`; pilot before bulk; packages under `min_uncovered_stmts` are never queued; one small prompt per package; bounded retries; hard caps at item/repo/run/time level; every result checkpointed in `state.json`.

## Limits worth knowing

- A call killed at the timeout reports no cost, so it is charged its whole cap. That can overstate the spend of a call that was killed early in a cheap turn; it never understates it.
- With `-commit`, a commit that fails (a hook, no git identity) leaves the accepted tests uncommitted and is recorded as a warning in `state.json` and the final report.
- In-flight coverage is an estimate until `gocov verify` (or the early-stop verify) replaces it with a fresh full run.
- `-race` needs cgo; set `race: true` only where a C toolchain exists.
