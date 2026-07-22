package discord

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestDiscordImportsNoForeignSDK locks in that the public Discord wrapper imports
// no foreign platform SDK directly (it legitimately imports discordgo).
// Direct-import guard only; the transitive build closure (this wrapper imports
// root botbooter, which is SDK-free) is proven by the module-level isolation
// deps test.
func TestDiscordImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"slack-go/slack", "go-telegram/bot", "google/go-github", "bradleyfalzon/ghinstallation"}, "discord")
}
