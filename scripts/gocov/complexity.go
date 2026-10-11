package main

// Complexity limits. Many teams cap function complexity (commonly 15) in a
// SonarQube gate or a golangci-lint rule, and the cap usually covers test
// files too. gocov computes two metrics per function so it can match either:
//
//	cyclomatic: 1 + decision points (if/for/range/case/comm/&&/||)   (gocyclo)
//	cognitive : SonarSource-style, nesting makes branches cost more  (gocognit / Sonar S3776)
//
// These are faithful approximations, not byte-exact clones of those tools: a
// real linter on PATH or in the repo's lint config stays the authority.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type FuncComplexity struct {
	File       string `json:"file"`
	Name       string `json:"name"`
	Line       int    `json:"line"`
	Cyclomatic int    `json:"cyclomatic"`
	Cognitive  int    `json:"cognitive"`
	Test       bool   `json:"test"`
}

// Worst returns the value that counts against the limit for the metric
// ("cyclomatic", "cognitive" or "both").
func (f FuncComplexity) Worst(metric string) int {
	switch metric {
	case "cyclomatic":
		return f.Cyclomatic
	case "cognitive":
		return f.Cognitive
	}
	if f.Cyclomatic > f.Cognitive {
		return f.Cyclomatic
	}
	return f.Cognitive
}

func (f FuncComplexity) String() string {
	return fmt.Sprintf("%s:%d %s (cyclomatic %d, cognitive %d)", f.File, f.Line, f.Name, f.Cyclomatic, f.Cognitive)
}

// fileComplexity measures every top-level function / method in one file.
func fileComplexity(abs, rel string) []FuncComplexity {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		return nil
	}
	isTest := strings.HasSuffix(rel, "_test.go")
	var out []FuncComplexity
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		out = append(out, FuncComplexity{
			File: rel, Name: fd.Name.Name, Line: fset.Position(fd.Pos()).Line,
			Cyclomatic: cyclomatic(fd), Cognitive: cognitive(fd.Body),
			Test: isTest,
		})
	}
	return out
}

func cyclomatic(fd *ast.FuncDecl) int {
	c := 1
	ast.Inspect(fd, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CommClause:
			c++
		case *ast.CaseClause:
			if x.List != nil { // default does not add a path
				c++
			}
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				c++
			}
		}
		return true
	})
	return c
}

// cognitive implements the SonarSource cognitive-complexity rules for Go:
// +1 per structure (if/for/range/switch/select), plus the current nesting
// depth; else / else-if cost +1 flat; a run of the same boolean operator costs
// +1 once; closures raise the nesting depth without costing anything.
func cognitive(body *ast.BlockStmt) int {
	total := 0
	var stmts func(list []ast.Stmt, nest int)
	var stmt func(s ast.Stmt, nest int)
	var expr func(e ast.Expr, nest int)

	boolOps := func(e ast.Expr) int {
		n, last := 0, token.ILLEGAL
		var walk func(ast.Expr)
		walk = func(e ast.Expr) {
			switch x := e.(type) {
			case *ast.ParenExpr:
				walk(x.X)
			case *ast.BinaryExpr:
				if x.Op == token.LAND || x.Op == token.LOR {
					walk(x.X)
					if x.Op != last {
						n++
						last = x.Op
					}
					walk(x.Y)
					return
				}
			}
		}
		walk(e)
		return n
	}

	// expr only looks for closures (nesting) and boolean sequences.
	expr = func(e ast.Expr, nest int) {
		if e == nil {
			return
		}
		total += boolOps(e)
		ast.Inspect(e, func(n ast.Node) bool {
			if fl, ok := n.(*ast.FuncLit); ok {
				stmts(fl.Body.List, nest+1)
				return false
			}
			return true
		})
	}

	stmts = func(list []ast.Stmt, nest int) {
		for _, s := range list {
			stmt(s, nest)
		}
	}

	var ifChain func(s *ast.IfStmt, nest int, isElseIf bool)
	ifChain = func(s *ast.IfStmt, nest int, isElseIf bool) {
		if isElseIf {
			total++
		} else {
			total += 1 + nest
		}
		if s.Init != nil {
			stmt(s.Init, nest)
		}
		expr(s.Cond, nest)
		stmts(s.Body.List, nest+1)
		switch e := s.Else.(type) {
		case *ast.IfStmt:
			ifChain(e, nest, true)
		case *ast.BlockStmt:
			total++
			stmts(e.List, nest+1)
		}
	}

	stmt = func(s ast.Stmt, nest int) {
		switch x := s.(type) {
		case *ast.IfStmt:
			ifChain(x, nest, false)
		case *ast.ForStmt:
			total += 1 + nest
			expr(x.Cond, nest)
			stmts(x.Body.List, nest+1)
		case *ast.RangeStmt:
			total += 1 + nest
			expr(x.X, nest)
			stmts(x.Body.List, nest+1)
		case *ast.SwitchStmt:
			total += 1 + nest
			expr(x.Tag, nest)
			for _, c := range x.Body.List {
				cc := c.(*ast.CaseClause)
				for _, e := range cc.List {
					expr(e, nest)
				}
				stmts(cc.Body, nest+1)
			}
		case *ast.TypeSwitchStmt:
			total += 1 + nest
			for _, c := range x.Body.List {
				stmts(c.(*ast.CaseClause).Body, nest+1)
			}
		case *ast.SelectStmt:
			total += 1 + nest
			for _, c := range x.Body.List {
				stmts(c.(*ast.CommClause).Body, nest+1)
			}
		case *ast.BlockStmt:
			stmts(x.List, nest)
		case *ast.LabeledStmt:
			stmt(x.Stmt, nest)
		case *ast.BranchStmt:
			if x.Label != nil || x.Tok == token.GOTO {
				total++
			}
		case *ast.ExprStmt:
			expr(x.X, nest)
		case *ast.AssignStmt:
			for _, e := range x.Rhs {
				expr(e, nest)
			}
		case *ast.ReturnStmt:
			for _, e := range x.Results {
				expr(e, nest)
			}
		case *ast.DeferStmt:
			expr(x.Call, nest)
		case *ast.GoStmt:
			expr(x.Call, nest)
		case *ast.DeclStmt:
			ast.Inspect(x, func(n ast.Node) bool {
				if vs, ok := n.(*ast.ValueSpec); ok {
					for _, e := range vs.Values {
						expr(e, nest)
					}
					return false
				}
				return true
			})
		}
	}
	stmts(body.List, 0)
	return total
}

// complexityOf scans the given repo-relative Go files.
func complexityOf(root string, files []string) []FuncComplexity {
	var out []FuncComplexity
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		out = append(out, fileComplexity(filepath.Join(root, filepath.FromSlash(rel)), rel)...)
	}
	return out
}

// complexityOver returns the functions over the limit, worst first.
func complexityOver(fs []FuncComplexity, limit int, metric string) []FuncComplexity {
	var out []FuncComplexity
	for _, f := range fs {
		if f.Worst(metric) > limit {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worst(metric) > out[j].Worst(metric) })
	return out
}

// scanRepoComplexity walks a repo and returns every function over the limit,
// split into production code and tests. Generated and vendored files are skipped.
func scanRepoComplexity(root string, limit int, metric string) (prod, tests []FuncComplexity) {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if class, _ := classify(root, rel); class == "generated" || class == "vendor" {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	for _, f := range complexityOver(complexityOf(root, files), limit, metric) {
		if f.Test {
			tests = append(tests, f)
		} else {
			prod = append(prod, f)
		}
	}
	return prod, tests
}

var lintLimitRe = regexp.MustCompile(`(?i)(min-complexity|max-complexity|threshold)\s*:\s*\d+`)

// lintComplexityHints reports complexity settings found in golangci / sonar
// config so the user sees when the repo's own limit differs from ours.
func lintComplexityHints(root string) []string {
	var hints []string
	for _, name := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json", "sonar-project.properties"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		low := strings.ToLower(string(data))
		if !(strings.Contains(low, "gocyclo") || strings.Contains(low, "gocognit") || strings.Contains(low, "cyclop") || strings.Contains(low, "complexity")) {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if lintLimitRe.MatchString(line) {
				hints = append(hints, fmt.Sprintf("%s: `%s`", name, strings.TrimSpace(line)))
			}
		}
		if len(hints) == 0 {
			hints = append(hints, name+": mentions a complexity linter (no explicit threshold found - tool defaults apply)")
		}
	}
	return hints
}

// testComplexityFeedback builds the rejection text for finish/run.
func testComplexityFeedback(over []FuncComplexity, limit int, metric string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "complexity limit is %d (%s) and these functions are over it:\n", limit, metric)
	for _, f := range over {
		fmt.Fprintf(&sb, "  - %s\n", f)
	}
	sb.WriteString("Split them: one small top-level Test function per scenario group, a flat table-driven loop with ONE if per case, and move repeated setup/assertion blocks into helper functions (helpers count separately). Do not delete scenarios to get under the limit.")
	return sb.String()
}

var _ = path.Base
