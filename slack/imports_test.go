package slack

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestSlackImportsNoForeignSDK locks in that the public Slack wrapper imports no
// foreign platform SDK directly (it legitimately imports slack-go). Direct-import
// guard only; transitive isolation is proven by the module-level deps test once
// root is stripped.
func TestSlackImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "go-telegram/bot"}, "slack")
}
