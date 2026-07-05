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
// binary, so only the non-test build closure matters for isolation. A go list
// failure is fatal for the calling subtest so no assertion ever runs against a
// malformed (error-text) closure.
func goListDeps(t *testing.T, pkg string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping transitive isolation check")
	}
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return string(out)
}

// TestIsolationDeps is the transitive guard the per-package ParseDir guards
// cannot provide: it proves each public package's full build closure excludes
// the other platforms' SDKs (and that root excludes all three), while each
// platform wrapper still includes its own SDK. A future cross-package import
// (e.g. internal/telegram pulling in internal/slack) would pass every
// direct-import guard yet fail here. Each package runs as its own subtest so one
// failure neither hides nor is hidden by the others.
func TestIsolationDeps(t *testing.T) {
	const (
		discordgo  = "github.com/bwmarrin/discordgo"
		slackgo    = "github.com/slack-go/slack"
		gotelegram = "github.com/go-telegram/bot"
		// jwtv5 is the Teams adapter's only third-party dependency. It must stay
		// confined to the teams closure and never leak into another package. The
		// full versioned module path matches the other constants and how
		// `go list -deps` emits it.
		jwtv5 = "github.com/golang-jwt/jwt/v5"
		// gogithubSDK and ghinstall are version-agnostic substrings: go-github
		// cuts majors often (currently v88) and ghinstallation pins its own
		// go-github major, so a fully versioned path would miss a future bump.
		gogithubSDK = "github.com/google/go-github"
		ghinstall   = "github.com/bradleyfalzon/ghinstallation"
		// jwtv4 is what ghinstallation (v2.19.0) actually pulls in — confirmed via
		// `go mod graph` — not jwtv5. It is a different major than Teams' jwtv5
		// and the two must never be confused: the github row expects jwtv4 present
		// and jwtv5 absent.
		jwtv4 = "github.com/golang-jwt/jwt/v4"
	)
	// The SDK checks above miss a cross-import of a marker-less internal package
	// (internal/cli and internal/whatsapp pull in no third-party SDK), so also
	// assert directly on the first-party internal/<platform> build paths: each
	// public package must contain only its own platform's internal package.
	const internalBase = "github.com/lao/botbooter/internal/"
	allPlatforms := []string{"cli", "slack", "discord", "telegram", "whatsapp", "teams", "github"}
	cases := []struct {
		pkg         string
		absent      []string
		present     []string
		internalOwn string // the one internal/<platform> its closure may contain ("" = none)
	}{
		{"github.com/lao/botbooter", []string{discordgo, slackgo, gotelegram, jwtv5, gogithubSDK, ghinstall}, nil, ""},
		{"github.com/lao/botbooter/cli", []string{discordgo, slackgo, gotelegram, jwtv5, gogithubSDK, ghinstall}, nil, "cli"},
		{"github.com/lao/botbooter/slack", []string{discordgo, gotelegram, jwtv5, gogithubSDK, ghinstall}, []string{slackgo}, "slack"},
		{"github.com/lao/botbooter/discord", []string{slackgo, gotelegram, jwtv5, gogithubSDK, ghinstall}, []string{discordgo}, "discord"},
		{"github.com/lao/botbooter/telegram", []string{discordgo, slackgo, jwtv5, gogithubSDK, ghinstall}, []string{gotelegram}, "telegram"},
		{"github.com/lao/botbooter/whatsapp", []string{discordgo, slackgo, gotelegram, jwtv5, gogithubSDK, ghinstall}, nil, "whatsapp"},
		{"github.com/lao/botbooter/teams", []string{discordgo, slackgo, gotelegram, gogithubSDK, ghinstall}, []string{jwtv5}, "teams"},
		// The github row's closure legitimately contains jwtv4 (pulled in by
		// ghinstallation) but must not contain jwtv5, which is Teams' own major.
		{"github.com/lao/botbooter/github", []string{discordgo, slackgo, gotelegram, jwtv5}, []string{gogithubSDK, ghinstall, jwtv4}, "github"},
	}
	for _, tc := range cases {
		t.Run(tc.pkg, func(t *testing.T) {
			closure := goListDeps(t, tc.pkg)
			for _, sdk := range tc.absent {
				asserts.False(t, strings.Contains(closure, sdk), tc.pkg+" build closure must not contain "+sdk)
			}
			for _, sdk := range tc.present {
				asserts.True(t, strings.Contains(closure, sdk), tc.pkg+" build closure should contain its own SDK "+sdk)
			}
			for _, p := range allPlatforms {
				path := internalBase + p
				// Match a whole path segment so internal/telegram does not satisfy a
				// check for internal/teams.
				contains := strings.Contains(closure, path+"\n") || strings.HasSuffix(closure, path)
				if p == tc.internalOwn {
					asserts.True(t, contains, tc.pkg+" should contain its own "+path)
				} else {
					asserts.False(t, contains, tc.pkg+" must not contain "+path)
				}
			}
		})
	}
}
