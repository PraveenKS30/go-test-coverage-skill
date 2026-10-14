# go-test-coverage

A [Claude Code skill](https://code.claude.com/docs/en/skills) that raises Go unit-test coverage to a goal (for example 60% or 80%) across one or many repos, folders or files - with real, behavior-verifying tests, not padding - while keeping every function it writes under a complexity limit (default 15).

## What it does

1. **Report first** - measures coverage per repo (`-coverpkg=./...`), ranks files and packages by uncovered statements. No tests are written yet.
2. **Plan** - queues only the packages the goal needs, with a cost estimate and a warning if the goal looks unreachable. One approval gate.
3. **Write tests** - table-driven, deterministic, mutation-checked; only `_test.go` files change. Either inside a Claude Code session (no extra login) or headless with a script that loops over `claude -p`, one fresh call per package.
4. **Guard rails** (enforced by a script, not the model) - non-test edits are reverted, tests must be green, stable and order-independent (`-count=3 -shuffle=on`) and `go vet` clean, new functions over the complexity limit are rejected, and each package must show real new coverage.
5. **Adversarial review** - a fresh-context, read-only reviewer looks for tests that would not catch a real regression.
6. **Verify and close** - a repo is marked `achieved` only after a fresh full run meets the goal.

Progress is reported after every package, and state is saved so runs can be resumed.

## Install

```bash
# personal skill
git clone https://github.com/PraveenKS30/go-test-coverage-skill ~/.claude/skills/go-test-coverage

# build the engine once (needs Go)
cd ~/.claude/skills/go-test-coverage/scripts/gocov && go build -o ../bin/gocov .   # gocov.exe on Windows
```

Then ask Claude Code something like *"get coverage of ./myrepo to 80%"*.

## Headless / unattended runs

A child `claude -p` cannot reuse the Claude Desktop or session login. It needs the standalone CLI logged in (`claude auth status`) or `ANTHROPIC_API_KEY`.

```bash
scripts/fanout.sh  -c cfg.json -r REPO -n 3     # pilot: first 3 packages, prints real cost
```
```powershell
scripts\fanout.ps1 -Config cfg.json -Repo REPO -Pilot 3
```

The repo path, models, spend caps, attempts and timeouts all come from the config (`references/batch-config-example.json`), the same rules `gocov run` applies.

Run `gocov report` and `gocov plan` first (see `SKILL.md`). Config template: `references/batch-config-example.json`.

## Layout

| Path | Purpose |
|---|---|
| `SKILL.md` | The workflow Claude follows |
| `references/` | Detail loaded on demand: in-session and headless modes, complexity limits, review step, command/config reference, junk-test patterns |
| `scripts/gocov/` | Stdlib-only Go engine: measuring, ranking, planning, guard rails, progress, goal verification, complexity, review packet |
| `scripts/fanout.sh`, `scripts/fanout.ps1` | Article-style loop over `claude -p` |

## Status and caveats

- Developed and exercised on small sample Go repos. The headless pilot ran against a real model on one sample repo (3 packages, about $1.14, goal verified); it is not yet proven on large codebases.
- Complexity numbers (cyclomatic and cognitive) are faithful approximations validated against SonarSource's published examples, not byte-exact copies of any linter. If your repo's linter disagrees, the linter wins.
- SonarQube-specific diagnosis (new-code gate conditions, report path mapping) is out of scope.
- No license has been chosen yet.
