package core

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCoreImportsNoPlatformSDK locks in the decoupling: the engine must not
// import any platform SDK. Adapters and the facade own those imports.
func TestCoreImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "gitlab-org/api/client-go"}, "core")
}
