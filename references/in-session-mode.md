# In-session mode (default inside Claude Desktop / CLI)

The session you are already in is authenticated, so it writes the tests itself. `gocov begin` / `gocov finish` put the same guard rails around it that headless mode has. **Never ask the user to log in for this mode.**

## Loop for one package (take Item IDs from `PLAN.md`, top first)

1. `gocov begin -config cfg.json -repo R -item ID` - checks out the work branch, measures the package, snapshots the tree, prints the focused prompt (package path, uncovered line ranges, rules, complexity limit).
2. **Read first**: the source files named in the prompt, the package's existing `_test.go` (including build-tagged ones), and what the code depends on (services, stores, models) so assertions are real. Before the first test in a repo also read `junk-patterns.md`. Match the existing style: black-box (`package x_test`) vs white-box, existing helpers; testify only if already a dependency. If `go.mod` says go < 1.22, add `tc := tc` before `t.Parallel()` in table loops.
3. **Write table-driven behavioral tests** for untested exported funcs and branchy unexported ones. Prefer driving real collaborators (router + real in-memory store, `httptest`) over mocks of types you own; small consumer-side fakes or wrappers for failure injection. Inject clocks; no sleeps, no network, `t.TempDir()`.
4. **Keep functions under the complexity limit** - pattern in `complexity.md`.
5. **Authoring gate**, per test: what behavior does it protect? what regression would it catch? why is it not already covered? does it need a test-only production seam (then skip it and report)?
6. **Mutation check**, per new test function that guards branching logic: break the branch it protects (e.g. flip a status code, drop a condition, remove a route), run the package tests and confirm they FAIL, restore exactly (`git checkout -- file`), confirm green. `git status` must show only test files at the end. A test that survives a plausible mutant is weak - strengthen it or drop it.
7. `gocov finish -config cfg.json -repo R -item ID`. The script reverts any non-test edits, requires green tests, rejects new over-limit functions, runs `-count=3` for flakiness, and requires real new coverage. On success the item is `done`; otherwise it prints feedback - fix and re-run. After 2 failed attempts: `gocov finish -abandon` (reverts, marks `blocked`). Pass `-cost USD` if the spend is known.
8. Report progress to the user after each package (package before -> after, repo % vs goal, items left).
9. When the queue is done, run the adversarial review (`review-step.md`) before the final verify.

## Rules

- Never edit production code, `go.mod` or `go.sum` unless the user approved a specific refactor in the plan. If a branch needs a seam, skip it and list it in the handoff.
- If you spot a bug in production code while reading (for example wrong ordering of validate-then-reserve), do not "fix" it and do not write a test that locks the buggy behavior in. Verify it with a throwaway test you delete afterwards, then report it.
- No USD meter in this mode: the cost lever is the number of items. Run the first 3, show progress, then continue.
- The guard rails need git. If the folder is not a repo, offer `git init` plus an initial commit; without git `finish` cannot revert forbidden edits, so get explicit OK before proceeding.
- Stop as soon as the goal is reached; do not write bonus tests for remaining queued packages.

## Many packages: do NOT share one working tree between writers

`begin` snapshots the working tree and `finish` / `-abandon` compare against and revert to that snapshot. Two agents writing in the same tree would see each other's new files as their own: one could be blamed for the other's over-limit test, and `-abandon` could delete the other's work. So:

- **Default: one package at a time**, in plan order. Simple, and the goal is usually reached after a few packages anyway.
- **Parallel across repos** is safe: different repos have separate trees and separate `state.json` files.
- **Parallel within one repo** only with isolation, as Claude Code's own fan-out guidance does (each subagent in its own git worktree; the built-in `/batch` command works this way). Give each subagent its own worktree **and** its own `-out` directory, have it run `begin` -> write -> mutation check -> `finish` there, then merge the worktree branches and record the results in the main state with `gocov mark -status done`. Finish with one `gocov verify` on the merged tree. Expect to resolve overlaps in shared helper files.
- Keep `finish` calls for one repo sequential (they all update that repo's `state.json`).
