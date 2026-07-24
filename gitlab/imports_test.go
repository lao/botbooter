package gitlab

import (
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestGitLabImportsNoForeignSDK locks in that the public GitLab wrapper imports
// no other platform's SDK directly — client-go is its own. Direct-import guard
// only; the transitive build closure is proven by the module-level isolation
// deps test.
func TestGitLabImportsNoForeignSDK(t *testing.T) {
	asserts.CheckBannedImports(t, ".",
		[]string{"discordgo", "slack-go/slack", "go-telegram/bot", "google/go-github"}, "gitlab")
}
