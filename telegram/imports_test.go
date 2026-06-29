package telegram

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestTelegramImportsNoForeignSDK locks in that the public Telegram wrapper
// imports no foreign platform SDK directly (it legitimately imports go-telegram).
// Direct-import guard only; transitive isolation is proven by the module-level
// deps test once root is stripped.
func TestTelegramImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack"}, "telegram")
}
