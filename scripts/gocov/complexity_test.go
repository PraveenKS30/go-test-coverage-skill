package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Expected values come from SonarSource's cognitive-complexity white paper
// examples (translated to Go) and the standard cyclomatic definition.
func TestComplexityKnownExamples(t *testing.T) {
	src := `package x

func sumOfPrimes(max int) int {
	total := 0
outer:
	for i := 1; i <= max; i++ { // +1
		for j := 2; j < i; j++ { // +2 (nesting 1)
			if i%j == 0 { // +3 (nesting 2)
				continue outer // +1 (labelled jump)
			}
		}
		total += i
	}
	return total
}

func words(n int) string {
	switch n { // +1 regardless of the number of cases
	case 1:
		return "one"
	case 2:
		return "two"
	case 3:
		return "three"
	default:
		return "lots"
	}
}

func chain(a, b, c, d bool) bool {
	if a && b && c || d { // +1 (if) +1 (&& run) +1 (|| run)
		return true
	}
	return false
}

func elseIf(n int) int {
	if n > 0 { // +1
		return 1
	} else if n < 0 { // +1
		return -1
	} else { // +1
		return 0
	}
}

func closure(xs []int) int {
	n := 0
	f := func(x int) { // closure: nesting +1, no cost itself
		if x > 0 { // +1 +1 (nesting 1)
			n++
		}
	}
	for _, x := range xs { // +1
		f(x)
	}
	return n
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "x.go")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ cyc, cog int }{
		"sumOfPrimes": {4, 7},
		"words":       {4, 1}, // 3 cases + 1
		"chain":       {5, 3}, // if + && + && + || + 1
		"elseIf":      {3, 3},
		"closure":     {3, 3}, // if in closure = 1+1 nesting, for = 1
	}
	got := map[string]FuncComplexity{}
	for _, f := range fileComplexity(file, "x.go") {
		got[f.Name] = f
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: not measured", name)
			continue
		}
		if g.Cyclomatic != w.cyc || g.Cognitive != w.cog {
			t.Errorf("%s: cyclomatic %d cognitive %d, want %d and %d", name, g.Cyclomatic, g.Cognitive, w.cyc, w.cog)
		}
	}
}

func TestComplexityOverFiltersAndSorts(t *testing.T) {
	fs := []FuncComplexity{
		{Name: "ok", Cyclomatic: 15, Cognitive: 15},
		{Name: "cyc", Cyclomatic: 20, Cognitive: 3},
		{Name: "cog", Cyclomatic: 3, Cognitive: 30},
	}
	if got := complexityOver(fs, 15, "cyclomatic"); len(got) != 1 || got[0].Name != "cyc" {
		t.Errorf("cyclomatic over = %v", got)
	}
	if got := complexityOver(fs, 15, "cognitive"); len(got) != 1 || got[0].Name != "cog" {
		t.Errorf("cognitive over = %v", got)
	}
	got := complexityOver(fs, 15, "both")
	if len(got) != 2 || got[0].Name != "cog" || got[1].Name != "cyc" {
		t.Errorf("both over = %v, want [cog cyc] (worst first, limit is inclusive)", got)
	}
}
