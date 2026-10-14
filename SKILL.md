---
name: go-test-coverage
description: Measures, plans and raises Go unit-test coverage to a goal (e.g. 60% or 80%) across one or many repos, folders or files, while keeping every function it writes under a complexity limit (default 15). Always produces a coverage report first, ranks packages by uncovered statements, gets the plan approved, then writes real behavior-verifying table-driven tests (never padding stubs), reports progress after every package, and marks each repo achieved only after a fresh full run verifies the goal. Works inside Claude Desktop/CLI with no extra login, or headless with `claude -p` and cost caps for large batches. Use when the user says "improve/increase test coverage", "get coverage to 60/80%", "write unit tests for this Go package/repo(s)", "coverage report for Go repos", or "keep complexity under 15" for Go code.
metadata:
  version: 2.5.0
---

# Go Test Coverage

Raise Go unit-test coverage to a **stated goal** across **one or more repos**, optionally limited to **folders/files**, without padding the number. Fixed order: **report -> rank and plan -> approval -> write tests -> track progress -> verify -> close.** Never write tests before the report and the approved plan exist.

A test that exists only to move a percentage is worse than no test. If a goal cannot be reached without junk, say so instead of writing the junk.

## Tooling

`gocov` is a stdlib-only Go program in `scripts/gocov/` that does all the deterministic work (measuring, ranking, planning, budgets, guard rails, progress, goal verification, complexity checks) at zero token cost. You write the tests; it measures and enforces. Build once:

```bash
cd <skill-dir>/scripts/gocov && go build -o ../bin/gocov .      # add .exe on Windows
```

Commands, config keys, outputs and statuses: `references/gocov-reference.md`. Config template: `references/batch-config-example.json`. Single repo without a config: `gocov <cmd> -path DIR -goal 80 -scope internal/foo,pkg/x.go`.

## Step 0 - Intake (ask only what you cannot derive)

1. **Repos** (paths) and **scope** (whole repo, or folders/files/globs).
2. **Goal per repo** (default 60) and metric: `adjusted` (default; generated + vendor code removed) or `raw`. This skill measures local `go test` coverage; SonarQube-specific diagnosis (new-code gate conditions, report path mapping) is out of scope - say so if the user's real problem is a Sonar gate.
3. **Complexity limit**: "Is there a per-function limit (commonly 15), and does it include test files?" Default 15, enforced on every test function this skill writes. Detail: `references/complexity.md`.
4. **Mode - by where you are running, never by asking the user to log in:**
   - Inside Claude Desktop or the Claude CLI (normal case): **in-session mode** - `references/in-session-mode.md`.
   - Unattended / CI / very large batches: **headless** - a plain script loops over `claude -p`, one fresh call per package, as in Claude Code's "fan out across files" guidance: `scripts/fanout.sh` or `scripts/fanout.ps1` (or the built-in `gocov run`). The *script* runs the commands, not a model. See `references/headless-mode.md`. It needs the standalone CLI logged in or `ANTHROPIC_API_KEY`; a child `claude -p` cannot reuse the Desktop/session login. If its preflight fails while you are in a session, switch to in-session mode.
5. **Budget** (headless): total / per-repo / per-item USD caps.
6. Write the config (or use `-path`). Every repo should be a git repo (work happens on a branch, edits are revertable); if not, offer `git init` + initial commit and get an explicit OK.

## Step 1 - Report first (no tests yet)

`gocov report -config cfg.json`, then show the user from `COVERAGE_REPORT.md` / `SUMMARY.md`: raw vs adjusted vs scoped %, gap to goal, top files/packages by **uncovered statements** (not percent), zero-coverage files, generated / `main` code, build-tagged tests that are skipped, complexity offenders, and any **pre-existing failing or unbuildable module** (report as pre-existing, never blame new tests). The baseline always uses `-coverpkg=./...`.

## Step 2 - Rank and plan (single approval gate)

`gocov plan -config cfg.json`, then present `PLAN.md` per repo: queued vs reserve packages, projected % (an estimate, verified later), cost/time estimate, and the warning if the goal looks unreachable. If unreachable offer: lower the goal, widen the scope, or approve specific production refactors. **Get the user's approval of the plan and budget once; do not start Step 3 without it.**

Never targeted: vendor, generated code (`*.pb.go`, `*_gen.go`, `mock_*.go`, `// Code generated ... DO NOT EDIT`), `package main` roots (still counted in the goal). Exclusion is legitimate only for machine-generated code - never for hard-to-test hand-written logic.

## Step 3 - Write tests

Follow the mode's reference file. Whatever the mode, every test must pass these:

- **Behavioral**: table-driven, asserts real outputs / state / errors. Read `references/junk-patterns.md` before the first test in a repo. No assertion-free, call-only, self-fulfilling-mock, trivial getter/setter, or permanently skipped tests.
- **Deterministic**: no sleeps, wall-clock, network or shared global state.
- **Proven**: a mutation check - break the branch the test claims to protect, see it fail, restore exactly.
- **Within the complexity limit** (flat tests: `references/complexity.md`).
- **Only `_test.go` / `testdata/` files change.** Production code, `go.mod` and `go.sum` are untouched unless the user approved a specific refactor.
- If you notice a production bug, verify it with a throwaway test, report it, and do not encode the buggy behavior in a test.

`gocov finish` (in-session) and `gocov run` (headless) enforce: non-test edits reverted, green tests, no new over-limit function, `-count=3` flake check, real new coverage.

## Step 4 - Report progress

After every package: package before -> after, repo % vs goal, points left, items done/queued/blocked, spend. `gocov status` for the cross-repo table. Combined figures are statement-weighted, **never an average of percentages**. In-flight numbers are estimates until a fresh full run.

## Step 5 - Adversarial review

Before closing, have a reviewer with a **fresh context** try to break confidence in the new tests: `gocov review` prepares the packet; a read-only subagent (or `gocov review -headless`) judges it and writes `REVIEW.md`. Fix every verified BLOCKING finding (weak tests, junk, nondeterminism, anything outside `_test.go`), take only cheap OPTIONAL ones, and report LOCKS-IN-A-BUG findings as production defects. Do not call the job done while blocking findings are open. Procedure, output format and the over-reporting guardrails: `references/review-step.md`.

## Step 6 - Verify and close

Completion is **not** "my tests pass". Run `gocov verify -config cfg.json` (fresh full run, same flags as the baseline):

- `achieved` - verified goal metric >= goal and no package fails that passed at baseline. Only this may be reported as done; name any pre-existing failures it lists.
- `failing-tests` - a package that was green at baseline now fails: fix it (never by weakening tests) and verify again, whatever the coverage.
- `needs-replan` - not met but reserve packages exist: show the gap, offer to promote them (scope/budget), re-plan, get approval.
- `not-achieved` - nothing worthwhile left: say what blocks it (generated / `main` / hard I/O code, named refactors that would unlock it). Do not force weak tests for the last points.

Then `go vet ./...` and `golangci-lint run` if already installed (never install it unasked). If the repo's real linter disagrees with gocov's complexity numbers, the linter wins.

## Step 7 - Handoff

Per repo and combined (`FINAL_REPORT.md`): status; goal metric before -> after (raw and adjusted; whether `-coverpkg` was used); per package what behavior is now protected and that it passed the mutation check; the adversarial review verdict and how each blocking finding was resolved; coverage that is only indirect (package has no tests of its own); blocked / skipped packages and why; production bugs and over-limit production functions found; pre-existing failures; spend; the branch holding the work. Do not commit, push or open PRs unless asked.

## Reference map

| Read when | File |
|---|---|
| Looking up a command, config key, status | `references/gocov-reference.md` |
| Writing tests in this session | `references/in-session-mode.md` |
| Running unattended / CI, auth, cost caps | `references/headless-mode.md` |
| A complexity limit applies | `references/complexity.md` |
| Reviewing the new tests before closing | `references/review-step.md` |
| Authoring tests (always, first time in a repo) | `references/junk-patterns.md` |

## Extending later

Other languages (swap the measure step), mutation tools such as go-mutesting in place of the manual check, new-code-only goals (diff against a base branch), CI wiring, or a separate approved-refactor skill for production complexity.
