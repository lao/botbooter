package teams

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// A single shared signing key keeps the suite fast (RSA keygen is not free).
var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

const testKID = "test-kid"

// allowedServiceURL is a real-looking, allowlisted Bot Framework serviceUrl so
// inbound tests exercise the happy path without a live host.
const allowedServiceURL = "https://smba.trafficmanager.net/amer/"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

// jwksServer serves the Bot Connector OpenID metadata + JWKS for the shared key.
// Point an adapter's openIDURL at <server>/openid. Any endorsements passed are
// attached to the served key; with none the key is unendorsed (the Emulator/Skill
// shape, which the channel check exempts).
func jwksServer(t *testing.T, endorsements ...string) *httptest.Server {
	t.Helper()
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		key := map[string]any{
			"kid": testKID,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}
		if len(endorsements) > 0 {
			key["endorsements"] = endorsements
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{key}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// mintToken signs a Bot Connector-style token with the given claims and kid.
func mintToken(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(signingKey(t))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func validClaims(aud, serviceURL string) jwt.MapClaims {
	return jwt.MapClaims{
		"aud":        aud,
		"iss":        botConnectorIssuer,
		"exp":        time.Now().Add(time.Hour).Unix(),
		"serviceurl": serviceURL,
	}
}

func validConfig() Config {
	return Config{AppID: "app-id", AppPassword: "secret", Addr: "127.0.0.1:0"}
}

// testAdapter builds an adapter directly (bypassing New) with Path defaulted and
// its OpenID endpoint pointed at a local JWKS server. Any endorsements are attached
// to that server's signing key; with none the key is unendorsed (channel-check
// exempt), which is what the auth tests that don't set a channelId rely on.
func testAdapter(t *testing.T, endorsements ...string) *adapter {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = jwksServer(t, endorsements...).URL + "/openid"
	return a
}

// activityJSON builds an inbound Activity JSON body. channelID is optional; pass one
// to set the channelId field (used by the endorsement tests), omit it to leave it unset.
func activityJSON(typ, text, serviceURL, fromID, recipientID, convID string, channelID ...string) string {
	act := map[string]any{
		"type":         typ,
		"id":           "act-1",
		"text":         text,
		"serviceUrl":   serviceURL,
		"timestamp":    "2026-06-30T12:00:00Z",
		"from":         map[string]string{"id": fromID, "name": "Ada"},
		"recipient":    map[string]string{"id": recipientID},
		"conversation": map[string]string{"id": convID},
	}
	if len(channelID) > 0 {
		act["channelId"] = channelID[0]
	}
	b, _ := json.Marshal(act)
	return string(b)
}

func TestNew(t *testing.T) {
	bot, err := New(validConfig())

	asserts.NoError(t, err, "New with full config should succeed")
	asserts.NotNil(t, bot, "bot should be initialized")
	asserts.Equal(t, bot.BotType, core.TeamsBotType, "bot type should be Teams")
	asserts.Equal(t, bot.BotType.String(), "teams", "bot type string should be teams")
}

func TestNew_Defaults(t *testing.T) {
	a, err := newAdapter(validConfig())

	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.Equal(t, a.cfg.Path, defaultPath, "Path should default")
	asserts.True(t, a.http != nil, "HTTPClient should default")
	asserts.True(t, strings.Contains(a.tokenURL, "botframework.com"), "multi-tenant token URL by default")
	asserts.Equal(t, a.openIDURL, openIDConfigURL, "openID URL should default")
}

func TestNew_TenantScopedTokenURL(t *testing.T) {
	cfg := validConfig()
	cfg.TenantID = "contoso.onmicrosoft.com"
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.True(t, strings.Contains(a.tokenURL, "contoso.onmicrosoft.com"), "token URL should be tenant-scoped")
}

func TestNew_NormalizesBareAddr(t *testing.T) {
	cases := map[string]string{
		"8080":           ":8080",
		":9090":          ":9090",
		"127.0.0.1:8080": "127.0.0.1:8080",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			cfg := validConfig()
			cfg.Addr = in
			a, err := newAdapter(cfg)
			asserts.NoError(t, err, "newAdapter should succeed")
			asserts.Equal(t, a.cfg.Addr, want, "Addr should be normalized")
		})
	}
}

func TestNew_NormalizesPath(t *testing.T) {
	cfg := validConfig()
	cfg.Path = "messages"

	a, err := newAdapter(cfg)

	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.Equal(t, a.cfg.Path, "/messages", "Path should start with a slash")
}

func TestNew_MissingConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"appID":       func(c *Config) { c.AppID = "" },
		"appPassword": func(c *Config) { c.AppPassword = "" },
		"addr":        func(c *Config) { c.Addr = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			_, err := New(cfg)
			asserts.ErrorIs(t, err, ErrMissingConfig, "missing field should error")
		})
	}
}
