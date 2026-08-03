package bitbucket

import (
	"context"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// baseCloud is a minimal valid Cloud API-token config; tests tweak one field.
func baseCloud() Config {
	return Config{Secret: "topsecret", Addr: "127.0.0.1:0", Email: "e@x", APIToken: "tok"}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{name: "MissingSecret", cfg: Config{Addr: ":0", Email: "e", APIToken: "t"}, wantErr: ErrMissingConfig},
		{name: "MissingAddr", cfg: Config{Secret: "s", Email: "e", APIToken: "t"}, wantErr: ErrMissingConfig},
		{name: "NoAuth", cfg: Config{Secret: "s", Addr: ":0"}, wantErr: ErrMissingConfig},
		{name: "BothAuthModes", cfg: Config{Secret: "s", Addr: ":0", Email: "e", APIToken: "t", AccessToken: "a"}, wantErr: ErrAmbiguousAuth},
		{name: "AccessTokenNoSelf", cfg: Config{Secret: "s", Addr: ":0", AccessToken: "a"}, wantErr: ErrMissingConfig},
		{name: "DataCenterNoSelf", cfg: Config{Secret: "s", Addr: ":0", Email: "e", APIToken: "t", BaseURL: "https://bb.example.com"}, wantErr: ErrMissingConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			asserts.ErrorIs(t, err, tc.wantErr, "expected error")
		})
	}
}

func TestNewValid(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "CloudAPIToken", cfg: baseCloud()},
		{name: "CloudAccessToken", cfg: Config{Secret: "s", Addr: ":0", AccessToken: "a", Self: "{me}"}},
		{name: "DataCenter", cfg: Config{Secret: "s", Addr: ":0", Email: "e", APIToken: "t", BaseURL: "https://bb.example.com", Self: "botuser"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot, err := New(tc.cfg)
			asserts.NoError(t, err, "new")
			asserts.Equal(t, bot.BotType, core.BitbucketBotType, "bot type")
		})
	}
}

func TestNewNormalizesAddrAndPath(t *testing.T) {
	a, err := newAdapter(Config{Secret: "s", Addr: "8080", Path: "hook", Email: "e", APIToken: "t"})
	asserts.NoError(t, err, "new adapter")
	asserts.Equal(t, a.cfg.Addr, ":8080", "bare port normalized")
	asserts.Equal(t, a.cfg.Path, "/hook", "path gets leading slash")
}

func TestNewDefaultsPath(t *testing.T) {
	a, err := newAdapter(baseCloud())
	asserts.NoError(t, err, "new adapter")
	asserts.Equal(t, a.cfg.Path, "/webhook", "default path")
}

// CloudClient returns the ktrysmt client on a Cloud bot and nil on a Data Center
// bot (whose replies go out over plain net/http).
func TestCloudClientFlavorDependence(t *testing.T) {
	cloud, err := New(baseCloud())
	asserts.NoError(t, err, "cloud bot")
	asserts.NotNil(t, CloudClient(cloud), "cloud bot has a client")

	dc, err := New(Config{Secret: "s", Addr: ":0", Email: "e", APIToken: "t", BaseURL: "https://bb.example.com", Self: "botuser"})
	asserts.NoError(t, err, "dc bot")
	asserts.True(t, CloudClient(dc) == nil, "data center bot has no cloud client")
}

// The larger body cap is only allocated when a large-event callback is registered.
func TestRequestByteLimitScales(t *testing.T) {
	small, err := newAdapter(baseCloud())
	asserts.NoError(t, err, "small adapter")
	asserts.Equal(t, small.maxRequestBytes, int64(smallRequestBytes), "comment-only bot keeps small cap")

	cfg := baseCloud()
	cfg.OnPush = func(context.Context, *PushEvent) {}
	large, err := newAdapter(cfg)
	asserts.NoError(t, err, "large adapter")
	asserts.Equal(t, large.maxRequestBytes, int64(largeRequestBytes), "push bot raises the cap")
}
