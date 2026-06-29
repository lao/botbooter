package botbooter_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// goListDeps returns the transitive build-dependency import paths of pkg as
// reported by `go list -deps`. It deliberately runs without -test: a wrapper's
// own _test.go may import any SDK, but those imports never reach a consumer
// binary, so only the non-test build closure matters for isolation.
func goListDeps(t *testing.T, pkg string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping transitive isolation check")
	}
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	asserts.NoError(t, err, "go list -deps "+pkg+": "+string(out))
	return string(out)
}

// TestIsolationDeps is the transitive guard the per-package ParseDir guards
// cannot provide: it proves each public package's full build closure excludes
// the other platforms' SDKs (and that root excludes all three), while each
// platform wrapper still includes its own SDK. A future cross-package import
// (e.g. internal/telegram pulling in internal/slack) would pass every
// direct-import guard yet fail here.
func TestIsolationDeps(t *testing.T) {
	const (
		discordgo  = "github.com/bwmarrin/discordgo"
		slackgo    = "github.com/slack-go/slack"
		gotelegram = "github.com/go-telegram/bot"
	)
	cases := []struct {
		pkg     string
		absent  []string
		present []string
	}{
		{"github.com/lao/botbooter", []string{discordgo, slackgo, gotelegram}, nil},
		{"github.com/lao/botbooter/cli", []string{discordgo, slackgo, gotelegram}, nil},
		{"github.com/lao/botbooter/slack", []string{discordgo, gotelegram}, []string{slackgo}},
		{"github.com/lao/botbooter/discord", []string{slackgo, gotelegram}, []string{discordgo}},
		{"github.com/lao/botbooter/telegram", []string{discordgo, slackgo}, []string{gotelegram}},
	}
	for _, tc := range cases {
		closure := goListDeps(t, tc.pkg)
		for _, sdk := range tc.absent {
			asserts.False(t, strings.Contains(closure, sdk), tc.pkg+" build closure must not contain "+sdk)
		}
		for _, sdk := range tc.present {
			asserts.True(t, strings.Contains(closure, sdk), tc.pkg+" build closure should contain its own SDK "+sdk)
		}
	}
}
