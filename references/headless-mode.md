# Headless mode (unattended, CI, very large batches)

One fresh `claude -p` per package, driven by a script - the "fan out across files" pattern from Claude Code's best-practices guide. **A script, not a model, runs the loop and the commands.** Two equivalent drivers:

- `scripts/fanout.sh` / `scripts/fanout.ps1` - short, readable loops you can edit (recommended).
- `gocov run` - the same loop built into the Go engine, with extras (parallel repos, escalation heuristics, per-repo/total/time budgets).

Both start one fresh `claude -p` per package. Use it when nobody is watching or the batch is too large for one conversation. Inside Claude Desktop / CLI prefer `in-session-mode.md`.

## Authentication - read this first

A child `claude -p` process **cannot borrow** the login of the Claude Desktop app or of the session that launched it (the host refreshes that login over a private channel). It needs one of:

1. The standalone CLI logged in: `claude auth status` shows `loggedIn: true` (one-time `claude` then `/login`).
2. `ANTHROPIC_API_KEY` (a Console API key) - the right choice for CI.

`gocov run` starts with a ~$0.05 probe. If auth fails it stops in seconds with these options (status `paused-error`, attempts not consumed). **If you are inside a session when this happens, switch to in-session mode rather than asking the user to log in.**

## Fan-out scripts (the article pattern)

Prerequisite: `gocov report` and `gocov plan` done and the plan approved (the scripts never write tests before that).

```bash
# bash / Git Bash / WSL / macOS / Linux
scripts/fanout.sh -c cfg.json -r REPO -d /path/to/repo -n 3        # pilot: first 3 items
scripts/fanout.sh -c cfg.json -r REPO -d /path/to/repo -t 10 -v    # everything, $10 cap, headless review at the end
scripts/fanout.sh -c cfg.json -r REPO -d /path/to/repo --dry-run   # print the claude command + prompt only
```

```powershell
# Windows PowerShell
scripts\fanout.ps1 -Config C:\work\cfg.json -Repo REPO -Dir C:\work\repo -Pilot 3
scripts\fanout.ps1 -Config C:\work\cfg.json -Repo REPO -Dir C:\work\repo -TotalUsd 10 -Review
```

Options: model (attempt 1), escalation model (later attempts), max attempts (2), per-call USD cap (1.50), total USD cap (10), pilot N, review, dry-run. The loop is, per queued item:

1. `gocov goal` - stop when the running estimate already reaches the goal.
2. `gocov begin -q` - snapshot the tree, print only the prompt file path.
3. `claude -p --output-format json --permission-mode dontAsk --max-budget-usd N --no-session-persistence --allowedTools "<pre-approved list>" < prompt` run inside the repo.
4. `gocov finish -cost <spent>` - exit 0 = accepted; **exit 3 = rejected, feedback is in `work/<id>.feedback.md`** and is appended to the next attempt's prompt (with the escalation model); after the last attempt `gocov finish -abandon` reverts and blocks the item.
5. After the loop: `gocov verify` (fresh full run), optional `gocov review -headless`.

The scripts first run `gocov preflight`, so a missing login fails in seconds. If `claude -p` returns an error without spending anything (auth/API), the script exits and the item stays queued. Helper commands for your own scripts: `gocov queue` (queued IDs), `gocov goal` (exit 0 reached / 10 not), `gocov preflight`, `gocov begin -q`, `gocov finish` (exit 3 = not accepted).

## `gocov run` commands

```bash
gocov run -config cfg.json -pilot 3      # first 3 items per repo, then prints real $/item and stmts gained per $1
gocov run -config cfg.json -dry-run      # print the exact claude command and prompt, run nothing
gocov run -config cfg.json               # everything queued; resumable, safe to interrupt
gocov run -config cfg.json -repo NAME    # one repo of the config
gocov run -config cfg.json -commit       # one local commit per successful item (never pushes)
```

Always pilot first, copy the observed `$ per item` into `est_usd_per_item`, re-run `gocov plan`, then approve the full run.

## What the script enforces (not the model)

- Permissions: the documented unattended pattern - `--allowedTools` pre-approves only `Read, Edit, Write, Glob, Grep` and the `go test|vet|build|list`, `gofmt`, `git diff|status` shell commands, and `--permission-mode dontAsk` denies everything else instead of prompting. The script, not the prompt, is the safety boundary.
- One package per call; the prompt is only the package path, uncovered line ranges and the rules.
- Model: `models.default` first; `models.escalate` on retry, for cgo/exec/syscall code, or packages with more than 300 uncovered statements.
- Budgets: `--max-budget-usd` per call (the smaller of per-item cap, repo remaining, run remaining) plus `max_usd_per_repo`, `max_usd_total`, `time_limit_min`, `item_timeout_min`. At a cap the repo pauses as `paused-budget`; raise the cap and re-run to resume. This `claude` version has no `--max-turns`; the USD cap and timeout bound each call.
- Only `_test.go` and `testdata/` edits survive. Production edits and `go.mod`/`go.sum` (unless `allow_deps`) are reverted automatically; failed items are cleaned out of the tree.
- Acceptance per item: tests green, no new over-limit function (`complexity.md`), `-count=3` flake check, at least one newly covered statement. Max `max_attempts` (default 2) then `blocked(reason)`.
- Early stop: when the running estimate reaches the goal it runs a fresh full verify; if the verified number is below goal it carries on.
- Repos run in parallel (`jobs`), each on its own branch (`branch`); a dirty working tree is refused so generated work stays separable.

## Cost controls checklist

Deterministic work is script-only; estimate shown at `plan`; pilot before bulk; packages under `min_uncovered_stmts` are never queued; one small prompt per package; bounded retries; hard caps at item/repo/run/time level; every result checkpointed in `state.json`.

## Limits worth knowing

- In-flight coverage is an estimate until `gocov verify` (or the early-stop verify) replaces it with a fresh full run.
- `-race` needs cgo; set `race: true` only where a C toolchain exists.
