package core

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCoreImportsNoPlatformSDK locks in the decoupling: the engine must not
// import any platform SDK. Adapters and the facade own those imports.
func TestCoreImportsNoPlatformSDK(t *testing.T) {
	banned := []string{"discordgo", "slack-go/slack", "go-telegram/bot"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	asserts.NoError(t, err, "parse core package")

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"core file "+name+" must not import "+b)
				}
			}
		}
	}
	asserts.True(t, checked > 0, "at least one non-test core file must be inspected")
}
