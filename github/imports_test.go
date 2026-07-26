package github

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestGitHubImportsNoForeignSDK locks in that the public GitHub wrapper imports
// no other platform's SDK directly — go-github and ghinstallation are its own.
// Direct-import guard only; the transitive build closure is proven by the
// module-level isolation deps test.
func TestGitHubImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "gitlab-org/api/client-go"}, "github")
}
