package whatsmeow

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestWhatsMeowImportsNoForeignSDK locks in that the public whatsmeow wrapper
// imports only its own platform SDK (whatsmeow) and no other platform's. Direct-
// import guard only; the transitive build closure is proven by the module-level
// isolation deps test.
func TestWhatsMeowImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "golang-jwt", "google/go-github", "bradleyfalzon/ghinstallation"}, "whatsmeow")
}
