package whatsapp

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestWhatsAppImportsNoPlatformSDK locks in that the public WhatsApp wrapper
// imports no platform SDK directly — the adapter speaks the Cloud API over plain
// net/http. Direct-import guard only; the transitive closure is proven by the
// module-level isolation deps test.
func TestWhatsAppImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "whatsapp")
}
