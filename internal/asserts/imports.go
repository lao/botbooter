package asserts

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// CheckBannedImports parses every non-test Go file in dir and fails t if any
// file imports a path containing one of the banned substrings. label names the
// package for error messages (e.g. "cli", "core"). It is the shared body of the
// per-package import guards that lock in the platform-SDK decoupling.
func CheckBannedImports(t *testing.T, dir string, banned []string, label string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	NoError(t, err, "parse "+label+" package")
	if err != nil {
		return
	}
	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				for _, b := range banned {
					False(t, strings.Contains(imp.Path.Value, b),
						label+" file "+name+" must not import "+b)
				}
			}
		}
	}
	True(t, checked > 0, "at least one non-test "+label+" file must be inspected")
}
