package telegram

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestTelegramImportsNoForeignSDK locks in that the public Telegram wrapper
// imports no foreign platform SDK directly (it legitimately imports go-telegram).
// Direct-import guard only; transitive isolation is proven by the module-level
// deps test once root is stripped.
func TestTelegramImportsNoForeignSDK(t *testing.T) {
	banned := []string{"discordgo", "slack-go/slack"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	asserts.NoError(t, err, "parse telegram package")

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			checked++
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"telegram file "+name+" must not import "+b)
				}
			}
		}
	}
	asserts.True(t, checked > 0, "at least one non-test telegram file must be inspected")
}
