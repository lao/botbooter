package slack

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestSlackImportsNoForeignSDK locks in that the public Slack wrapper imports no
// foreign platform SDK directly (it legitimately imports slack-go). Direct-import
// guard only; the transitive build closure (this wrapper imports root botbooter,
// which is SDK-free) is proven by the module-level isolation deps test.
func TestSlackImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "go-telegram/bot", "google/go-github", "bradleyfalzon/ghinstallation", "gitlab-org/api/client-go"}, "slack")
}
