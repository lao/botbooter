package cli

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCLIImportsNoPlatformSDK locks in that the public CLI wrapper imports no
// platform SDK directly. This is a direct-import guard only: transitive
// isolation (the wrapper imports root botbooter, still unstripped at this point)
// is proven once root is SDK-free, by the module-level isolation deps test.
func TestCLIImportsNoPlatformSDK(t *testing.T) {
	banned := []string{"discordgo", "slack-go/slack", "go-telegram/bot"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	asserts.NoError(t, err, "parse cli package")

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"cli file "+name+" must not import "+b)
				}
			}
		}
	}
	asserts.True(t, checked > 0, "at least one non-test cli file must be inspected")
}
