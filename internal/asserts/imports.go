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
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		NoError(t, err, "parse "+label+" file "+entry.Name())
		if err != nil {
			return
		}
		checked++
		for _, imp := range file.Imports {
			for _, b := range banned {
				False(t, strings.Contains(imp.Path.Value, b),
					label+" file "+entry.Name()+" must not import "+b)
			}
		}
	}
	True(t, checked > 0, "at least one non-test "+label+" file must be inspected")
}
