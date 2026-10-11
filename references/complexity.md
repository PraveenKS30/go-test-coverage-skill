# Complexity limits (usually 15 per function)

Teams commonly cap per-function complexity in a SonarQube gate or a golangci-lint rule (`gocyclo`, `gocognit`, `cyclop`). The cap **applies to production code and, very often, to test files too**. Ask at intake whether tests are included; this skill enforces the limit on every test function and helper it writes.

## What gocov measures

| Metric | Definition | Tool it mirrors |
|---|---|---|
| cyclomatic | 1 + `if`, `for`, `range`, each non-default `case`, `select` case, each `&&` / `||` | gocyclo, cyclop |
| cognitive | +1 per `if`/`for`/`range`/`switch`/`select` plus the current nesting depth; `else`/`else if` +1 flat; a run of the same boolean operator +1; labelled `break`/`continue`/`goto` +1; closures add nesting but cost nothing | SonarQube S3776, gocognit |

`complexity_metric: both` (default) means a function is over the limit if either number is. Cognitive is far stricter for tests because `for` -> `t.Run(func…)` -> `if` nests: an `if` inside a table loop's subtest closure costs 3, not 1. A real example from a pilot: a test with cyclomatic 13 had cognitive 43.

gocov's numbers are faithful approximations validated against SonarSource's published examples, not byte-exact clones. If the repo's real linter says otherwise, the linter wins: fix, and tell the user the figures differed. `gocov report` prints any threshold it finds in `.golangci.yml`/`.yaml`/`.toml` or `sonar-project.properties` - set `max_complexity` to match.

## Where it is enforced

- `gocov report` / `gocov complexity`: lists production functions and existing tests over the limit. **Reported, never auto-changed** - fixing production code is a refactor and needs the user's approval.
- `gocov finish` / `gocov run`: reject any test function that is new or became over-limit in this item, with feedback to split it. Functions that were already over the limit before the item started are not blamed on it.
- Config: `max_complexity` (default 15, `-1` disables), `complexity_metric`.

## How to write tests that stay flat

1. One small top-level `Test...` per scenario group instead of one `Test...` with many `t.Run` blocks. Split by behavior (`TestCancelRefusesPaidOrder`, `TestCancelRestoresStock`), not by arbitrary size.
2. Table-driven loop whose body is a single call; the `if` checks live in a helper at nesting level 0:

```go
type pricingCase struct {
	name      string
	qty       int
	wantTotal int64
}

func TestPlaceOrderPricing(t *testing.T) {
	tests := []pricingCase{
		{"plain", 2, 2000},
		{"bulk tier 10", 10, 9500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { checkPricing(t, tc) }) // loop + closure, no ifs here
	}
}

func checkPricing(t *testing.T, tc pricingCase) {
	t.Helper()
	// setup ...
	if got != tc.wantTotal { // nesting 0 -> costs 1
		t.Errorf("total = %d, want %d", got, tc.wantTotal)
	}
}
```

3. Move repeated setup (`newServer`, `seedProduct`) and repeated assertion blocks into helpers; helpers are measured separately.
4. A table field replaces an `if tc.name == …` branch inside the loop (for example `untouched bool`, `wantErr string`).
5. Never delete scenarios to get under the limit; split them.

## Production code already over the limit

Report it with file:line and both numbers. Offer a refactor only as a separate, approved step (extract functions, replace nested conditionals with early returns / table lookups), write characterization tests first, and keep that out of the coverage run.
