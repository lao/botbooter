package discord

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestDiscordImportsNoForeignSDK locks in that the public Discord wrapper imports
// no foreign platform SDK directly (it legitimately imports discordgo).
// Direct-import guard only; transitive isolation is proven by the module-level
// deps test once root is stripped.
func TestDiscordImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"slack-go/slack", "go-telegram/bot"}, "discord")
}
