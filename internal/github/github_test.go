package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// testKeyPEM returns a freshly generated PKCS#1 RSA private key in PEM form,
// the format GitHub issues for App keys and ghinstallation parses.
func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	asserts.NoError(t, err, "generate test RSA key")
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func patConfig() Config {
	return Config{Token: "ghp_test", WebhookSecret: "hook-secret", Addr: "127.0.0.1:0"}
}

func appConfig(t *testing.T) Config {
	return Config{AppID: 7, InstallationID: 11, PrivateKey: testKeyPEM(t),
		WebhookSecret: "hook-secret", Addr: "127.0.0.1:0"}
}

func TestNewAdapter_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"NoAuth", Config{WebhookSecret: "s", Addr: ":0"}, ErrMissingConfig},
		{"BothModes", Config{Token: "t", AppID: 1, InstallationID: 2, PrivateKey: []byte("k"), WebhookSecret: "s", Addr: ":0"}, ErrAmbiguousAuth},
		{"PartialAppTriple", Config{AppID: 1, WebhookSecret: "s", Addr: ":0"}, ErrMissingConfig},
		{"MissingSecret", Config{Token: "t", Addr: ":0"}, ErrMissingConfig},
		{"MissingAddr", Config{Token: "t", WebhookSecret: "s"}, ErrMissingConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newAdapter(tc.cfg)
			asserts.ErrorIs(t, err, tc.want, tc.name)
		})
	}
}

func TestNewAdapter_Normalization(t *testing.T) {
	cfg := patConfig()
	cfg.Addr = "8080"
	cfg.Path = "hooks"
	a, err := newAdapter(cfg)

	asserts.NoError(t, err, "valid PAT config")
	asserts.Equal(t, a.cfg.Addr, ":8080", "bare port gets a colon")
	asserts.Equal(t, a.cfg.Path, "/hooks", "path gets a leading slash")
	asserts.NotNil(t, a.cfg.HTTPClient, "default HTTP client applied")
	asserts.NotNil(t, a.client, "go-github client built")
}

func TestNewAdapter_DefaultPath(t *testing.T) {
	a, err := newAdapter(patConfig())

	asserts.NoError(t, err, "valid PAT config")
	asserts.Equal(t, a.cfg.Path, "/webhook", "default path")
}

// Guards the ghinstallation nil-RoundTripper panic: App mode with the default
// (nil-Transport) HTTPClient must normalize to http.DefaultTransport before
// handing the transport to ghinstallation.
func TestNewAdapter_AppModeDefaultTransport(t *testing.T) {
	a, err := newAdapter(appConfig(t))

	asserts.NoError(t, err, "App mode with default HTTPClient")
	asserts.NotNil(t, a.client, "client built without panicking on a nil Transport")
}

func TestNewAdapter_AppModeBadKey(t *testing.T) {
	cfg := appConfig(t)
	cfg.PrivateKey = []byte("not a pem key")
	_, err := newAdapter(cfg)

	asserts.Error(t, err, "unparseable private key should error")
}

func TestNew_BotType(t *testing.T) {
	bot, err := New(patConfig())

	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, bot.BotType, core.GitHubBotType, "bot type should be GitHub")
}

func TestNew_InvalidConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "New surfaces newAdapter's error")
}

func TestAddr_NotConnected(t *testing.T) {
	bot, err := New(patConfig())
	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}

func TestAddr_NotGitHubBot(t *testing.T) {
	asserts.Equal(t, Addr(core.New(core.CLIBotType, nil)), "", "Addr on a non-GitHub bot")
}

func TestClient_GitHubBot(t *testing.T) {
	bot, err := New(patConfig())

	asserts.NoError(t, err, "new GitHub bot")
	asserts.NotNil(t, Client(bot), "client for a GitHub bot")
}

func TestClient_NotGitHubBot(t *testing.T) {
	asserts.True(t, Client(core.New(core.CLIBotType, nil)) == nil, "no client for a non-GitHub bot")
}
