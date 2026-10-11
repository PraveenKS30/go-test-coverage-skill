# Adversarial review step

Coverage and a mutation check prove the tests run the code and caught the mutants *you* thought of. They do not prove the tests are good. A reviewer in a **fresh context** - it sees the tests and the criteria, not the reasoning that produced them - tries to find tests that would let a real regression through. This is the "second opinion" pattern from Claude Code's best-practices guide, applied to the generated tests.

Run it after all queued packages are `done` and before `gocov verify` is reported as the end of the job. It is recommended for every run and is cheap compared with the writing.

## Procedure

1. `gocov review -config cfg.json -repo R` writes `gocov-out/R/review/PROMPT.md`: the done packages, their test files, the criteria, the exact output format, and the "read-only" rule. It does not call a model.
2. Run the reviewer:
   - **In a session (normal case):** dispatch a subagent with a fresh context (Agent tool, general-purpose) and a one-line instruction: read `PROMPT.md`, follow it exactly, read-only, reply only with the review. Save its reply to `gocov-out/R/REVIEW.md`.
   - **Headless / CI:** `gocov review -config cfg.json -repo R -headless` runs one `claude -p` reviewer (strongest configured model, read-only tools, `dontAsk`, capped at 2x the per-item budget) and writes `REVIEW.md` itself. Needs the same auth as headless mode.
3. Triage `REVIEW.md` (the first line is `Verdict: PASS` or `Verdict: CHANGES NEEDED`; findings are `### BLOCKING - <KIND> - file:line` or `### OPTIONAL - ...`):
   - **BLOCKING**: verify the claim yourself before acting (reviewers can be wrong - run the suggested mutation if it is cheap). Fix real ones in the test files, re-run the mutation check, re-run `gocov finish` for that package. Delete `REVIEW.md` and re-run the review if you changed a lot; one clean pass is enough.
   - **OPTIONAL**: take only the cheap ones. Do not chase every item.
   - **LOCKS-IN-A-BUG** findings are production defects: report them to the user, do not change production code, do not weaken the test to hide them.
4. `gocov verify` prints a warning while `REVIEW.md` has unresolved BLOCKING findings, and `FINAL_REPORT.md` carries an "Adversarial review:" line (verdict, counts, or "not run"). Do not call the job done while blocking findings are open.

## Why the prompt is strict about over-reporting

A reviewer told to find gaps will usually find some, even when the work is sound, and chasing every finding leads to over-engineering: extra abstraction, defensive code, tests for cases that cannot happen. So the prompt limits findings to correctness and the stated requirements, forbids style comments, asks for a concrete mutation or regression for every WEAK/UNPROTECTED claim, and says to report nothing rather than pad. Keep that wording if you edit the template (`buildReviewPrompt` in `scripts/gocov/review.go`).

## What the reviewer checks

WEAK (would still pass if the behavior broke - name the surviving mutation), JUNK (see `junk-patterns.md`), UNPROTECTED (a clear contract behavior no test asserts), NONDETERMINISTIC, LOCKS-IN-A-BUG, SCOPE (anything outside `_test.go`/`testdata`), COMPLEXITY (function over the limit).

## Limits

- The reviewer reads and runs tests but cannot edit, so it cannot run mutants itself; it reasons about them from the assertions. That is why you verify blocking claims before acting.
- It judges only packages with `done` items and recorded test files.
- It is one independent opinion, not a proof.
