package teams

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestTeamsImportsNoPlatformSDK locks in that the public Teams wrapper imports no
// platform SDK directly — the adapter speaks the Bot Connector REST API over
// plain net/http (golang-jwt is a crypto library, not a platform SDK). Direct-
// import guard only; the transitive build closure is proven by the module-level
// isolation deps test.
func TestTeamsImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github"}, "teams")
}
