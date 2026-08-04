package botbooter_test

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestRootImportsNoPlatformSDK locks in that the root facade is SDK-free: it
// re-exports only shared types from internal/core and must import no platform
// SDK directly. The transitive closure is proven by TestIsolationDeps.
func TestRootImportsNoPlatformSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"ktrysmt/go-bitbucket", "discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github", "bradleyfalzon/ghinstallation", "gitlab-org/api/client-go"}, "botbooter")
}
