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
// function, or "type" for a type. Methods are skipped: vllm's types are
// aliases, so openaicompat's methods come with them.
func exports(t *testing.T, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	notTest := func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }
	pkgs, err := parser.ParseDir(fset, dir, notTest, 0) //nolint:staticcheck // SA1019: ParseDir is enough for one flat package
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	typeList := func(fl *ast.FieldList) string {
		if fl == nil {
			return ""
		}
		var parts []string
		for _, f := range fl.List {
			var b strings.Builder
			_ = printer.Fprint(&b, fset, f.Type)
			for range max(len(f.Names), 1) {
				parts = append(parts, b.String())
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
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									out[n.Name] = d.Tok.String()
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
	want, got := exports(t, "../openaicompat"), exports(t, ".")
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
