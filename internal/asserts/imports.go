package asserts

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// CheckBannedImports parses every non-test Go file in dir and fails t if any
// file imports a path containing one of the banned substrings. label names the
// package for error messages (e.g. "cli", "core"). It is the shared body of the
// per-package import guards that lock in the platform-SDK decoupling.
func CheckBannedImports(t TestingT, dir string, banned []string, label string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	NoError(t, err, "read "+label+" package dir")
	if err != nil {
		return
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		NoError(t, err, "parse "+label+"/"+name)
		if err != nil {
			return // a parse error aborts the scan, matching the prior ParseDir behavior
		}
		checked++
		for _, imp := range file.Imports {
			for _, b := range banned {
				False(t, strings.Contains(imp.Path.Value, b),
					label+" file "+name+" must not import "+b)
			}
		}
	}
	True(t, checked > 0, "at least one non-test "+label+" file must be inspected")
}
