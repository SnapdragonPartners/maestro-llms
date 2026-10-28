package vllm_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// exports maps each exported top-level identifier of the package in dir
// (non-test files) to its signature: the parameter and result types of a
// function, or the declaration kind ("type", "const", "var"). Methods are
// skipped: vllm's types are aliases, so openaicompat's methods come with them.
//
// With forwardFrom set (the alias package), a type, const or var counts as
// its kind only when it forwards to the same-named identifier there: a type
// alias (type X = pkg.X) or a value (X = pkg.X). Anything else is recorded
// as a mismatch, so a defined type (which drops the methods) or a
// re-declared value (which can drift) fails the comparison.
func exports(t *testing.T, dir, forwardFrom string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	notTest := func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }
	pkgs, err := parser.ParseDir(fset, dir, notTest, 0) //nolint:staticcheck // SA1019: ParseDir is enough for one flat package
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	render := func(n ast.Node) string {
		var b strings.Builder
		_ = printer.Fprint(&b, fset, n)
		return b.String()
	}
	typeList := func(fl *ast.FieldList) string {
		if fl == nil {
			return ""
		}
		var parts []string
		for _, f := range fl.List {
			for range max(len(f.Names), 1) {
				parts = append(parts, render(f.Type))
			}
		}
		return strings.Join(parts, ", ")
	}
	out := map[string]string{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.IsExported() {
						out[d.Name.Name] = "func(" + typeList(d.Type.Params) + ") (" + typeList(d.Type.Results) + ")"
					}
				case *ast.GenDecl:
					for _, s := range d.Specs {
						switch s := s.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								out[s.Name.Name] = "type"
								if forwardFrom != "" && (!s.Assign.IsValid() || render(s.Type) != forwardFrom+"."+s.Name.Name) {
									out[s.Name.Name] = "type not an alias of " + forwardFrom + "." + s.Name.Name
								}
							}
						case *ast.ValueSpec:
							for i, n := range s.Names {
								if !n.IsExported() {
									continue
								}
								out[n.Name] = d.Tok.String()
								if forwardFrom != "" && (s.Type != nil || i >= len(s.Values) || render(s.Values[i]) != forwardFrom+"."+n.Name) {
									out[n.Name] = d.Tok.String() + " not forwarded from " + forwardFrom + "." + n.Name
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// The deprecated alias must export exactly what openaicompat exports, with
// the same signatures, so an import-path swap always compiles (ADR-0027,
// ADR-0035). Adding an exported identifier to openaicompat fails this test
// until vllm forwards it.
func TestExportParityWithOpenAICompat(t *testing.T) {
	want, got := exports(t, "../openaicompat", ""), exports(t, ".", "openaicompat")
	if len(want) == 0 {
		t.Fatal("found no openaicompat exports; the parser path is wrong")
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		switch g, ok := got[name]; {
		case !ok:
			t.Errorf("vllm does not export %s (openaicompat: %s)", name, want[name])
		case g != want[name]:
			t.Errorf("%s: vllm %s, openaicompat %s", name, g, want[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[name]; !ok {
			t.Errorf("vllm exports %s, which openaicompat does not", name)
		}
	}
}
