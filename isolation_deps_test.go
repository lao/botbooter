package botbooter_test

import (
	"os/exec"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// Inspect the build closure a consumer gets without test-only dependencies.
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

// Each public package may import only its own platform SDKs and internal adapter.
func TestIsolationDeps(t *testing.T) {
	const module = "github.com/lao/botbooter"
	platforms := []struct {
		name string
		sdks []string
	}{
		{"", nil},
		{"cli", nil},
		{"slack", []string{"github.com/slack-go/slack"}},
		{"discord", []string{"github.com/bwmarrin/discordgo"}},
		{"telegram", []string{"github.com/go-telegram/bot"}},
		{"whatsapp/cloud", nil},
		{"whatsapp/whatsmeow", []string{"go.mau.fi/whatsmeow", "modernc.org/sqlite"}},
		{"teams", []string{"github.com/golang-jwt/jwt/v5"}},
		{"github", []string{"github.com/google/go-github", "github.com/bradleyfalzon/ghinstallation", "github.com/golang-jwt/jwt/v4"}},
		{"gitlab", []string{"gitlab.com/gitlab-org/api/client-go"}},
		{"bitbucket", []string{"github.com/ktrysmt/go-bitbucket"}},
		{"signal", nil},
	}

	for _, platform := range platforms {
		pkg := path.Join(module, platform.name)
		t.Run(pkg, func(t *testing.T) {
			deps := strings.Fields(goListDeps(t, pkg))
			for _, owner := range platforms {
				if owner.name == "" {
					continue
				}
				want := platform.name == owner.name
				internal := path.Join(module, "internal", owner.name)
				asserts.Equal(t, slices.Contains(deps, internal), want, internal+" in dependency tree")
				for _, sdk := range owner.sdks {
					present := slices.ContainsFunc(deps, func(dep string) bool {
						return dep == sdk || strings.HasPrefix(dep, sdk+"/")
					})
					asserts.Equal(t, present, want, sdk+" in dependency tree")
				}
			}
			// Slack and Discord also use Gorilla WebSocket transitively.
			if platform.name == "signal" {
				asserts.True(t, slices.Contains(deps, "github.com/gorilla/websocket"), "Signal WebSocket transport")
			}
		})
	}
}
