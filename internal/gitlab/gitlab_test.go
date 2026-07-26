package gitlab

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func testConfig() Config {
	return Config{Token: "glpat-test", Secret: "hook-secret", Addr: "127.0.0.1:0"}
}

func TestNewAdapter_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"NoToken", Config{Secret: "s", Addr: ":0"}},
		{"NoSecret", Config{Token: "t", Addr: ":0"}},
		{"NoAddr", Config{Token: "t", Secret: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newAdapter(tc.cfg)
			asserts.ErrorIs(t, err, ErrMissingConfig, tc.name)
		})
	}
}

func TestNewAdapter_Normalization(t *testing.T) {
	cfg := testConfig()
	cfg.Addr = "8080"
	cfg.Path = "hooks"
	a, err := newAdapter(cfg)

	asserts.NoError(t, err, "valid config")
	asserts.Equal(t, a.cfg.Addr, ":8080", "bare port gets a colon")
	asserts.Equal(t, a.cfg.Path, "/hooks", "path gets a leading slash")
	asserts.NotNil(t, a.cfg.HTTPClient, "default HTTP client applied")
	asserts.NotNil(t, a.client, "client built")
}

func TestNewAdapter_DefaultPath(t *testing.T) {
	a, err := newAdapter(testConfig())

	asserts.NoError(t, err, "valid config")
	asserts.Equal(t, a.cfg.Path, "/webhook", "default path")
}

// The inbound body cap follows the configured capability: a bot that handles
// only comments caps at smallRequestBytes, and registering either large-event
// callback raises it to largeRequestBytes.
func TestNewAdapter_RequestByteLimitFollowsConfig(t *testing.T) {
	base, err := newAdapter(testConfig())
	asserts.NoError(t, err, "comment-only config")
	asserts.Equal(t, base.maxRequestBytes, int64(smallRequestBytes), "no large-event callback caps small")

	push := testConfig()
	push.OnPush = func(context.Context, *gogitlab.PushEvent) {}
	withPush, err := newAdapter(push)
	asserts.NoError(t, err, "OnPush config")
	asserts.Equal(t, withPush.maxRequestBytes, int64(largeRequestBytes), "OnPush raises the cap")

	mr := testConfig()
	mr.OnMergeRequest = func(context.Context, *gogitlab.MergeEvent) {}
	withMR, err := newAdapter(mr)
	asserts.NoError(t, err, "OnMergeRequest config")
	asserts.Equal(t, withMR.maxRequestBytes, int64(largeRequestBytes), "OnMergeRequest raises the cap")
}

// A self-hosted BaseURL is accepted and the client is built; the /api/v4 suffix
// is appended by client-go, so a bare instance URL is valid.
func TestNewAdapter_SelfHostedBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "https://gitlab.example.com"
	a, err := newAdapter(cfg)

	asserts.NoError(t, err, "self-hosted base URL")
	asserts.NotNil(t, a.client, "client built for a self-hosted instance")
}

func TestNew_BotType(t *testing.T) {
	bot, err := New(testConfig())

	asserts.NoError(t, err, "new GitLab bot")
	asserts.Equal(t, bot.BotType, core.GitLabBotType, "bot type should be GitLab")
}

func TestNew_InvalidConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "New surfaces newAdapter's error")
}

func TestAddr_NotConnected(t *testing.T) {
	bot, err := New(testConfig())
	asserts.NoError(t, err, "new GitLab bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}

func TestAddr_NotGitLabBot(t *testing.T) {
	asserts.Equal(t, Addr(core.New(core.CLIBotType, nil)), "", "Addr on a non-GitLab bot")
}

// Addr's whole purpose is recovering the OS-assigned port after binding ":0",
// so exercise that through the accessor while connected.
func TestAddr_Connected(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()
	defer func() { _ = a.Disconnect() }()
	bot := core.New(core.GitLabBotType, a)

	addr := Addr(bot)
	asserts.True(t, addr != "", "Addr reports the bound address while connected")
	_, port, err := net.SplitHostPort(addr)
	asserts.NoError(t, err, "Addr is host:port")
	asserts.True(t, port != "" && port != "0", "OS-assigned port recovered")

	waitForSelfID(t, a, 777) // settle async resolution before tearing down
	asserts.NoError(t, a.Disconnect(), "disconnect")
	asserts.Equal(t, Addr(bot), "", "Addr empty again after Disconnect")
}

func TestClient_GitLabBot(t *testing.T) {
	bot, err := New(testConfig())

	asserts.NoError(t, err, "new GitLab bot")
	asserts.NotNil(t, Client(bot), "client for a GitLab bot")
}

func TestClient_NotGitLabBot(t *testing.T) {
	asserts.True(t, Client(core.New(core.CLIBotType, nil)) == nil, "no client for a non-GitLab bot")
}

func TestLog_DefaultBeforeConnect(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	asserts.True(t, a.log() == slog.Default(), "default logger before Connect")
}

func TestLog_PrefersConnectLogger(t *testing.T) {
	a, err := newAdapter(testConfig())
	asserts.NoError(t, err, "new adapter")
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	a.mu.Lock()
	a.logger = custom
	a.mu.Unlock()
	asserts.True(t, a.log() == custom, "logger handed over at Connect wins")
}
