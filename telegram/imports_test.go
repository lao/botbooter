package telegram

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestTelegramImportsNoForeignSDK locks in that the public Telegram wrapper
// imports no foreign platform SDK directly (it legitimately imports go-telegram).
// Direct-import guard only; the transitive build closure (this wrapper imports
// root botbooter, which is SDK-free) is proven by the module-level isolation
// deps test.
func TestTelegramImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "google/go-github"}, "telegram")
}
