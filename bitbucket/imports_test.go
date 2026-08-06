package bitbucket

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestBitbucketImportsNoForeignSDK locks in that the public Bitbucket wrapper
// imports no other platform's SDK directly — ktrysmt/go-bitbucket is its own.
// Direct-import guard only; the transitive build closure is proven by the
// module-level isolation deps test.
func TestBitbucketImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github", "bradleyfalzon/ghinstallation", "gitlab-org/api/client-go"}, "bitbucket")
}
