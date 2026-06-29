package slack

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestSlackImportsNoForeignSDK locks in that the public Slack wrapper imports no
// foreign platform SDK directly (it legitimately imports slack-go). Direct-import
// guard only; transitive isolation is proven by the module-level deps test once
// root is stripped.
func TestSlackImportsNoForeignSDK(t *testing.T) {
	banned := []string{"discordgo", "go-telegram/bot"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	asserts.NoError(t, err, "parse slack package")

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"slack file "+name+" must not import "+b)
				}
			}
		}
	}
	asserts.True(t, checked > 0, "at least one non-test slack file must be inspected")
}
