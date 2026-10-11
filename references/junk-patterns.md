# Junk patterns to reject

Adapted from the openclaw `test-audit` skill's junk-pattern checklist
(https://github.com/openclaw/openclaw/blob/main/.agents/skills/test-audit/SKILL.md),
Go-flavored. If a candidate test matches any of these, don't keep it — rewrite
it to actually verify behavior, or drop the target and move on.

## The mutation check — run this, don't just reason about it

Every pattern below is a *description* of a bad test. The authoring gate in
SKILL.md asks you to judge your own test against them, and self-judgement is
exactly where a test that "looks like it asserts something" slips through. So
verify it mechanically instead:

1. Break the behavior the new test claims to protect — invert a condition,
   return a zero value, drop a branch, skip the transformation. Make the
   smallest edit that changes behavior while still compiling.
2. Run only the new test: `go test -run 'TestName/subtest_name' ./pkg/...`
3. **It must fail.** If it still passes, the test does not protect that
   behavior, regardless of how it reads or how much coverage it added.
4. Revert the mutation (`git diff` to confirm the production file is clean)
   and re-run to confirm green.

A test that passes against deliberately broken code is junk by definition —
it is an assertion-free probe wearing an assertion. Delete or rewrite it.

Do this at minimum once per new test *function* (not per table row), and
always for tests of branching logic. Mutate the specific branch a case claims
to cover, not just any line in the function: a test can catch a broken happy
path while proving nothing about the error path it was written for.

**Never leave a mutation in the working tree.** Verify with `git status` /
`git diff` before moving to the next package.

## Assertion-free "coverage probes"

```go
// BAD: calls the function, asserts nothing about its behavior.
func TestProcess(t *testing.T) {
    Process(someInput)
}
```
This executes the line for coverage purposes but verifies nothing. It will
never fail even if `Process` is completely broken.

## Self-fulfilling mock assertions

```go
// BAD: configures the mock to return X, then asserts the result is X.
mockRepo.On("Get", id).Return(fakeUser, nil)
got, _ := svc.GetUser(id)
assert.Equal(t, fakeUser, got) // only proves the mock works, not svc.GetUser
```
If the assertion only reflects back what the mock was told to return, with no
independent logic of `svc.GetUser` in between, the test proves nothing about
`svc.GetUser`'s own behavior (e.g. does it transform, validate, or cache?).

## Trivial getter/setter tests

```go
// BAD: no behavior to break.
func TestGetName(t *testing.T) {
    u := User{Name: "a"}
    assert.Equal(t, "a", u.GetName())
}
```
Skip these unless the getter contains actual logic (defaulting, computed
value, lazy init).

## Tests of generated code

Never write tests targeting `.pb.go`, `_gen.go`, `mock_*.go`, or anything
under a `// Code generated ... DO NOT EDIT` header. If coverage on generated
code is what's blocking the target, exclude it from the coverage metric (config `exclude`
globs, or the repo's own coverage-tool exclusions) with the user's permission, not by
testing generated code.

## Un-injected `time.Now()` dependence

```go
// BAD: flaky, and re-running it a year from now may behave differently.
func TestIsExpired(t *testing.T) {
    e := Entry{CreatedAt: time.Now().Add(-1 * time.Hour)}
    assert.True(t, e.IsExpired())
}
```
Inject a fixed clock/time instead (a `Clock` interface or a `now func() time.Time` field set by the test).

## Permanent `t.Skip` placeholders

```go
// BAD: left in indefinitely, pads the file without pading coverage.
func TestEdgeCase(t *testing.T) {
    t.Skip("TODO: implement")
}
```
Either implement it now or don't create the stub at all.

## Cosmetic-only table-driven cases

```go
// BAD: three cases, one behavior.
{name: "input foo", input: "foo", want: len("foo")},
{name: "input bar", input: "bar", want: len("bar")},
{name: "input baz", input: "baz", want: len("baz")},
```
If every case exercises the identical code path with only the literal value
changed, collapse to one representative case (plus real edge cases: empty
input, max length, unicode, etc. — each chosen because it exercises a
*different* branch).

## Test-only production seams that hide real design debt

```go
// BAD: exporting an unexported field purely so a test can poke it.
type Service struct {
    Cache map[string]string // was `cache`, exported only for the test
}
```
If reaching a branch requires this kind of hack, that's a signal the
production code needs a real interface/DI seam, not a backdoor. Flag it in
the handoff report as a design suggestion instead of making the hack.
