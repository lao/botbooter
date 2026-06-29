package cli

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCLIImportsNoPlatformSDK locks in that the public CLI wrapper imports no
// platform SDK directly. This is a direct-import guard only: transitive
// isolation (the wrapper imports root botbooter, still unstripped at this point)
// is proven once root is SDK-free, by the module-level isolation deps test.
func TestCLIImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "cli")
}
