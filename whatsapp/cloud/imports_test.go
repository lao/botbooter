package cloud

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestWhatsAppCloudImportsNoPlatformSDK locks in that the public WhatsApp Cloud
// API wrapper imports no platform SDK directly — the adapter speaks the Cloud
// API over plain net/http, and the whatsmeow stack belongs only to the sibling
// whatsapp/whatsmeow flavor. Direct-import guard only; the transitive build
// closure (this wrapper imports root botbooter, which is SDK-free) is proven by
// the module-level isolation deps test.
func TestWhatsAppCloudImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "whatsmeow", "modernc.org/sqlite", "google/go-github", "bradleyfalzon/ghinstallation", "gitlab-org/api/client-go"}, "whatsapp/cloud")
}
