package cli

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCLIImportsNoPlatformSDK locks in that the public CLI wrapper imports no
// platform SDK directly. Direct-import guard only; the transitive build closure
// (this wrapper imports root botbooter, which is SDK-free) is proven by the
// module-level isolation deps test.
func TestCLIImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github", "bradleyfalzon/ghinstallation", "gitlab-org/api/client-go"}, "cli")
}
