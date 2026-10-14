package main

// Coverage measurement: discovery of Go modules, running `go test`, parsing and
// de-duplicating coverage profiles, classifying files (generated / main /
// hard-to-test) and computing statement-weighted totals.

import (
	"bufio"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FileCov is the per-file coverage row. Paths are relative to the repo root.
type FileCov struct {
	Module  string `json:"module"`
	File    string `json:"file"`
	Pkg     string `json:"pkg"`
	Stmts   int    `json:"stmts"`
	Covered int    `json:"covered"`
	Class   string `json:"class,omitempty"` // generated | vendor | main
	Hard    bool   `json:"hard,omitempty"`  // imports cgo/os/exec/syscall/unsafe
}

func (f FileCov) Uncovered() int { return f.Stmts - f.Covered }

// Range is an uncovered line range [Start, End].
type Range struct{ Start, End int }

// UBlock is one uncovered coverage block. Span is the profile's
// "startLine.col,endLine.col", which is the same for a file whichever test
// binary reported it, so blocks from different runs can be matched exactly.
type UBlock struct {
	Span  string `json:"span"`
	Stmts int    `json:"stmts"`
}

// Measurement is the outcome of one `go test` coverage run.
type Measurement struct {
	Rows     []FileCov
	Uncov    map[string][]Range  // rel file -> uncovered ranges
	UncovB   map[string][]UBlock // rel file -> uncovered blocks
	Hit      map[string]bool     // "rel file:span" -> hit by this run
	TestExit int                 // non-zero => at least one failing test (profile still usable)
	Fallback bool                // -coverpkg had to be dropped
	Failing  []string            // packages go test reported as FAIL
	Log      string
}

type Totals struct {
	Stmts, Covered int
}

func (t Totals) Pct() float64 {
	if t.Stmts == 0 {
		return 0
	}
	return float64(t.Covered) * 100 / float64(t.Stmts)
}

// Module is one go.mod root inside a repo.
type Module struct {
	Dir       string `json:"dir"`  // relative to repo root, "." for root
	Path      string `json:"path"` // module import path
	GoVersion string `json:"go_version"`
	Testify   bool   `json:"testify"`
}

var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, "testdata": true}

func discoverModules(root string) ([]Module, error) {
	var mods []Module
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		m := Module{}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		m.Dir = filepath.ToSlash(rel)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "module "):
				m.Path = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
			case strings.HasPrefix(line, "go "):
				m.GoVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
			}
		}
		m.Testify = strings.Contains(string(data), "stretchr/testify")
		mods = append(mods, m)
		return nil
	})
	sort.Slice(mods, func(i, j int) bool { return mods[i].Dir < mods[j].Dir })
	return mods, err
}

type block struct {
	file       string
	span       string // "startLine.col,endLine.col"
	start, end int
	stmts      int
	hit        bool
}

var keyRe = regexp.MustCompile(`^(.*):(\d+)\.\d+,(\d+)\.\d+$`)

// parseProfile reads a Go coverage profile and DE-DUPLICATES blocks by key
// (with -coverpkg every test binary reports every package; summing raw lines
// would massively inflate totals). A block is covered if any binary hit it.
func parseProfile(p string) (map[string]*block, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blocks := map[string]*block{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		var stmts, cnt int
		fmt.Sscanf(parts[1], "%d", &stmts)
		fmt.Sscanf(parts[2], "%d", &cnt)
		b, ok := blocks[parts[0]]
		if !ok {
			m := keyRe.FindStringSubmatch(parts[0])
			if m == nil {
				continue
			}
			b = &block{file: m[1], span: parts[0][len(m[1])+1:], stmts: stmts}
			fmt.Sscanf(m[2], "%d", &b.start)
			fmt.Sscanf(m[3], "%d", &b.end)
			blocks[parts[0]] = b
		}
		if cnt > 0 {
			b.hit = true
		}
	}
	return blocks, sc.Err()
}

// goTest runs `go test` in modAbs, writing the profile to profPath and the
// combined output to logPath. It returns the exit code (-1 if go could not run).
func goTest(ctx context.Context, modAbs string, args []string, logPath string) int {
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, _ := os.Create(logPath)
	if lf != nil {
		defer lf.Close()
	}
	cmd := exec.CommandContext(ctx, "go", append([]string{"test"}, args...)...)
	cmd.Dir = modAbs
	if lf != nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// measure runs coverage for one module. pkgRel=="" => whole module with
// -coverpkg=./... (so untested packages appear at 0%, as SonarQube sees them);
// otherwise only that package directory (fast, used inside the work loop).
func measure(ctx context.Context, root string, mod Module, pkgRel string, flags []string, logPath string) (*Measurement, error) {
	modAbs := filepath.Join(root, filepath.FromSlash(mod.Dir))
	tmp, err := os.CreateTemp("", "gocov-*.out")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	run := func(coverpkg bool) int {
		args := []string{"-count=1"}
		if coverpkg && pkgRel == "" {
			args = append(args, "-coverpkg=./...")
		}
		args = append(args, "-coverprofile="+tmp.Name())
		args = append(args, flags...)
		if pkgRel == "" {
			args = append(args, "./...")
		} else {
			args = append(args, "./"+strings.TrimPrefix(pkgRel, "./"))
		}
		return goTest(ctx, modAbs, args, logPath)
	}
	m := &Measurement{Log: logPath}
	m.TestExit = run(true)
	blocks, _ := parseProfile(tmp.Name())
	if len(blocks) == 0 && pkgRel == "" {
		// -coverpkg can break cgo / build-tag-heavy repos: fall back, and say so.
		m.TestExit = run(false)
		m.Fallback = true
		blocks, _ = parseProfile(tmp.Name())
	}
	if m.TestExit != 0 {
		m.Failing = failingPkgs(logPath)
		if len(m.Failing) == 0 {
			// non-zero exit without a FAIL line (vet, go command error): still a failure
			m.Failing = []string{fmt.Sprintf("module %s (go test exit %d)", mod.Dir, m.TestExit)}
		}
	}
	if len(blocks) == 0 {
		return m, fmt.Errorf("no coverage profile produced for module %q (exit %d) - likely a BUILD failure, see %s", mod.Dir, m.TestExit, logPath)
	}
	m.Rows, m.Uncov = rowsFromBlocks(root, mod, blocks)
	m.UncovB, m.Hit = map[string][]UBlock{}, map[string]bool{}
	for _, b := range blocks {
		rel := relFile(mod, b.file)
		if b.hit {
			m.Hit[rel+":"+b.span] = true
		} else {
			m.UncovB[rel] = append(m.UncovB[rel], UBlock{Span: b.span, Stmts: b.stmts})
		}
	}
	for f := range m.UncovB {
		sort.Slice(m.UncovB[f], func(i, j int) bool { return m.UncovB[f][i].Span < m.UncovB[f][j].Span })
	}
	return m, nil
}

// "FAIL\t<pkg>\t0.1s" / "FAIL\t<pkg> [build failed]"; [ \t] so a bare "FAIL"
// line does not swallow the next line ("coverage: ...").
var failRe = regexp.MustCompile(`(?m)^FAIL[ \t]+(\S+)`)

// failingPkgs reads a `go test` log and returns the packages reported as FAIL
// (failing tests, build or setup failures), sorted and de-duplicated.
func failingPkgs(logPath string) []string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, m := range failRe.FindAllStringSubmatch(string(data), -1) {
		set[m[1]] = true
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// relFile converts a profile file (import path + name) to repo-relative.
func relFile(mod Module, file string) string {
	rel := file
	if mod.Path != "" && strings.HasPrefix(rel, mod.Path+"/") {
		rel = strings.TrimPrefix(rel, mod.Path+"/")
	}
	if mod.Dir != "." {
		rel = path.Join(mod.Dir, rel)
	}
	return rel
}

// spanLines turns a block span "12.5,14.2" into its line range.
func spanLines(span string) Range {
	var r Range
	var c1, c2 int
	fmt.Sscanf(span, "%d.%d,%d.%d", &r.Start, &c1, &r.End, &c2)
	return r
}

func rowsFromBlocks(root string, mod Module, blocks map[string]*block) ([]FileCov, map[string][]Range) {
	type acc struct{ stmts, cov int }
	files := map[string]*acc{}
	uncov := map[string][]Range{}
	for _, b := range blocks {
		rel := relFile(mod, b.file)
		a := files[rel]
		if a == nil {
			a = &acc{}
			files[rel] = a
		}
		a.stmts += b.stmts
		if b.hit {
			a.cov += b.stmts
		} else {
			uncov[rel] = append(uncov[rel], Range{b.start, b.end})
		}
	}
	var rows []FileCov
	for rel, a := range files {
		fc := FileCov{Module: mod.Dir, File: rel, Pkg: path.Dir(rel), Stmts: a.stmts, Covered: a.cov}
		fc.Class, fc.Hard = classify(root, rel)
		rows = append(rows, fc)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].File < rows[j].File })
	for k := range uncov {
		uncov[k] = mergeRanges(uncov[k])
	}
	return rows, uncov
}

func mergeRanges(r []Range) []Range {
	sort.Slice(r, func(i, j int) bool { return r[i].Start < r[j].Start })
	var out []Range
	for _, x := range r {
		if n := len(out); n > 0 && x.Start <= out[n-1].End+1 {
			if x.End > out[n-1].End {
				out[n-1].End = x.End
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

var genNameRe = regexp.MustCompile(`(\.pb\.go|\.pb\.gw\.go|_gen\.go|\.gen\.go|_generated\.go|^zz_generated.*\.go|^mock_.*\.go|_mock\.go|_string\.go)$`)

// classify decides whether a file is generated / package main / hard to test.
// It is deliberately conservative: only machine-generated code is ever
// classed "generated" (the only code that may be excluded from the metric).
func classify(root, rel string) (class string, hard bool) {
	if strings.HasPrefix(rel, "vendor/") || strings.Contains(rel, "/vendor/") {
		return "vendor", false
	}
	if genNameRe.MatchString(path.Base(rel)) {
		return "generated", false
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if head := readHead(abs, 40); strings.Contains(head, "Code generated") && strings.Contains(head, "DO NOT EDIT") {
		return "generated", false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, parser.ImportsOnly)
	if err != nil {
		return "", false
	}
	if f.Name.Name == "main" {
		class = "main"
	}
	for _, imp := range f.Imports {
		switch strings.Trim(imp.Path.Value, `"`) {
		case "C", "os/exec", "syscall", "unsafe", "golang.org/x/sys/unix", "golang.org/x/sys/windows":
			hard = true
		}
	}
	return class, hard
}

func readHead(p string, lines int) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	var sb strings.Builder
	sc := bufio.NewScanner(f)
	for i := 0; i < lines && sc.Scan(); i++ {
		sb.WriteString(sc.Text())
		sb.WriteByte('\n')
	}
	return sb.String()
}

// matchGlob supports path.Match patterns plus a leading "**/".
func matchGlob(pat, rel string) bool {
	pat = filepath.ToSlash(pat)
	if ok, _ := path.Match(pat, rel); ok {
		return true
	}
	if strings.HasPrefix(pat, "**/") {
		rest := strings.TrimPrefix(pat, "**/")
		segs := strings.Split(rel, "/")
		for i := range segs {
			if ok, _ := path.Match(rest, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
	}
	return false
}

// inScope: no scope => everything; otherwise a file/dir prefix or glob match.
func inScope(scope []string, rel string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, s := range scope {
		s = strings.Trim(filepath.ToSlash(s), "/")
		s = strings.TrimPrefix(s, "./")
		s = strings.TrimSuffix(s, "/...")
		if s == "" || s == "." || rel == s || strings.HasPrefix(rel, s+"/") || matchGlob(s, rel) {
			return true
		}
	}
	return false
}

// sumRows aggregates rows. adjusted=true drops generated + vendor code.
// scope filters to the user's folders/files. Always statement-weighted.
func sumRows(rows []FileCov, scope []string, adjusted bool) Totals {
	var t Totals
	for _, r := range rows {
		if adjusted && (r.Class == "generated" || r.Class == "vendor") {
			continue
		}
		if !inScope(scope, r.File) {
			continue
		}
		t.Stmts += r.Stmts
		t.Covered += r.Covered
	}
	return t
}
