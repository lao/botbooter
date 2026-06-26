package core

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCoreImportsNoPlatformSDK locks in the decoupling: the engine must not
// import any platform SDK. Adapters and the facade own those imports.
func TestCoreImportsNoPlatformSDK(t *testing.T) {
	banned := []string{"discordgo", "slack-go/slack", "go-telegram/bot"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	asserts.NoError(t, err, "parse core package")

	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"core file "+name+" must not import "+b)
				}
			}
		}
	}
}
