package signal

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestSignalImportsNoPlatformSDK locks in that the public Signal wrapper
// imports no platform SDK directly — the adapter speaks JSON-RPC to a
// signal-cli daemon over plain TCP. Direct-import guard only; the transitive
// build closure is proven by the module-level isolation deps test.
func TestSignalImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "signal")
}
