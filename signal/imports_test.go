package signal

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestSignalImportsNoPlatformSDK locks in that the public Signal wrapper
// imports no platform SDK directly — the adapter speaks REST + WebSocket to a
// signal-cli-rest-api container (gorilla/websocket is a transport library, not
// a platform SDK). Direct-import guard only; the transitive build closure is
// proven by the module-level isolation deps test.
func TestSignalImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "signal")
}
