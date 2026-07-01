package teams

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return testAddr("failing") }

type testAddr string

func (testAddr) Network() string  { return "test" }
func (a testAddr) String() string { return string(a) }

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
// Point an adapter's openIDURL at <server>/openid.
func jwksServer(t *testing.T) *httptest.Server {
	t.Helper()
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kid": testKID,
				"kty": "RSA",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
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
// its OpenID endpoint pointed at a local JWKS server.
func testAdapter(t *testing.T) *adapter {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = jwksServer(t).URL + "/openid"
	return a
}

func captureDeps(got *[]*core.Message, done chan<- struct{}) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) {
			*got = append(*got, m)
			if done != nil {
				done <- struct{}{}
			}
		},
	}
}

func awaitDispatch(t *testing.T, done <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for dispatch %d of %d", i+1, n)
		}
	}
}

func activityJSON(typ, text, serviceURL, fromID, recipientID, convID string) string {
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
	b, _ := json.Marshal(act)
	return string(b)
}

// activityJSONFromRole builds an inbound message Activity whose from account
// carries the given Bot Framework role ("user"/"bot"). Teams sets from.role to
// "bot" on messages authored by a bot (the bot's own echo or another bot in a
// shared channel), which is how such messages are identified and dropped.
func activityJSONFromRole(role, serviceURL, fromID, recipientID, convID string) string {
	act := map[string]any{
		"type":         "message",
		"id":           "act-1",
		"text":         "hi",
		"serviceUrl":   serviceURL,
		"timestamp":    "2026-06-30T12:00:00Z",
		"from":         map[string]string{"id": fromID, "name": "Ada", "role": role},
		"recipient":    map[string]string{"id": recipientID},
		"conversation": map[string]string{"id": convID},
	}
	b, _ := json.Marshal(act)
	return string(b)
}

// post drives handleMessages with a body and Authorization header, returning the
// recorder.
func post(a *adapter, deps core.AdapterDeps, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	a.handleMessages(context.Background(), context.Background(), w, r, deps)
	return w
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

func TestIsAllowedServiceHost(t *testing.T) {
	cases := map[string]bool{
		"https://smba.trafficmanager.net/amer/": true,
		"https://x.botframework.com/":           true,
		"https://botframework.com/":             true,
		"https://evil.example.com/":             false,
		"http://smba.trafficmanager.net/":       false, // not https
		"https://nottrafficmanager.net.evil/":   false,
		"https://attacker.trafficmanager.net/":  false, // broad TM namespace not allowlisted
		"https://x.botframework.com@evil.com/":  false, // userinfo trick
		"":                                      false,
	}
	for in, want := range cases {
		asserts.Equal(t, isAllowedServiceHost(in), want, "isAllowedServiceHost "+in)
	}
}

func TestSameServiceURL(t *testing.T) {
	asserts.True(t, sameServiceURL("https://x/", "https://x"), "trailing slash ignored")
	asserts.False(t, sameServiceURL("https://x", "https://y"), "different urls differ")
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"canonical", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"lowercase scheme", "bearer abc.def.ghi", "abc.def.ghi", true},
		{"uppercase scheme", "BEARER abc.def.ghi", "abc.def.ghi", true},
		{"multiple separating spaces", "Bearer    abc", "abc", true},
		{"surrounding whitespace", "  Bearer abc  ", "abc", true},
		{"empty header", "", "", false},
		{"scheme only", "Bearer", "", false},
		{"scheme and spaces only", "Bearer   ", "", false},
		{"no separating space", "Bearerabc", "", false},
		{"wrong scheme", "Basic abc", "", false},
	}
	for _, tc := range cases {
		got, ok := bearerToken(tc.header)
		asserts.Equal(t, ok, tc.ok, "bearerToken ok "+tc.name)
		asserts.Equal(t, got, tc.want, "bearerToken token "+tc.name)
	}
}

func TestSameSchemeHost(t *testing.T) {
	const openID = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	cases := []struct {
		name string
		jwks string
		want bool
	}{
		{"identical host", "https://login.botframework.com/v1/.well-known/keys", true},
		{"explicit default port on jwks_uri", "https://login.botframework.com:443/v1/.well-known/keys", true},
		{"case-insensitive host", "https://Login.BotFramework.com/v1/.well-known/keys", true},
		{"different host", "https://evil.example.com/v1/.well-known/keys", false},
		{"scheme downgrade", "http://login.botframework.com/v1/.well-known/keys", false},
	}
	for _, tc := range cases {
		asserts.Equal(t, sameSchemeHost(openID, tc.jwks), tc.want, "sameSchemeHost "+tc.name)
	}
}

func TestParseTimestamp(t *testing.T) {
	got := parseTimestamp("2026-06-30T12:00:00Z")
	asserts.Equal(t, got.IsZero(), false, "valid RFC3339 should parse")
	asserts.Equal(t, parseTimestamp("nonsense").IsZero(), true, "bad timestamp is zero")
	asserts.Equal(t, parseTimestamp("").IsZero(), true, "empty timestamp is zero")
}

func TestToMessage_Mapping(t *testing.T) {
	body := activityJSON("message", "hello", allowedServiceURL, "user-1", "bot-1", "conv-1")
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")

	m := toMessage(&act, json.RawMessage(body))
	asserts.Equal(t, m.ID, "act-1", "ID")
	asserts.Equal(t, m.UserID, "user-1", "UserID from from.id")
	asserts.Equal(t, m.AuthorName, "Ada", "AuthorName from from.name")
	asserts.Equal(t, m.ChannelID, "conv-1", "ChannelID from conversation.id")
	asserts.Equal(t, m.Content, "hello", "Content from text")

	tm, ok := RawMessage(m)
	asserts.True(t, ok, "RawMessage should report Teams origin")
	asserts.Equal(t, tm.ServiceURL, allowedServiceURL, "ServiceURL carried on raw message")
}

func TestToMessage_ReplyToID(t *testing.T) {
	// A threaded reply carries replyToId; a top-level message omits it.
	withReply := `{"type":"message","id":"act-2","text":"re","replyToId":"act-1"}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(withReply), &act), "unmarshal reply activity")
	m := toMessage(&act, json.RawMessage(withReply))
	asserts.Equal(t, m.ReplyToID, "act-1", "ReplyToID populated from replyToId")

	var plain inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(`{"type":"message","id":"act-3"}`), &plain), "unmarshal plain activity")
	asserts.Equal(t, toMessage(&plain, nil).ReplyToID, "", "ReplyToID empty when absent")
}

func TestStripRecipientMention(t *testing.T) {
	const botID = "bot-1"
	mention := func(id, text string) mentionEntity {
		return mentionEntity{Type: "mention", Text: text, Mentioned: channelAccount{ID: id}}
	}
	cases := []struct {
		name     string
		text     string
		entities []mentionEntity
		want     string
	}{
		{"no entities passthrough keeps spacing", "  hello  ", nil, "  hello  "},
		{"bot mention stripped and trimmed", "<at>Bot</at> echo hi", []mentionEntity{mention(botID, "<at>Bot</at>")}, "echo hi"},
		{
			"other user mention preserved", "<at>Bot</at> tell <at>John</at> hi",
			[]mentionEntity{mention(botID, "<at>Bot</at>"), mention("user-2", "<at>John</at>")},
			"tell <at>John</at> hi",
		},
		{"one removal per bot mention entity", "<at>Bot</at> hi <at>Bot</at>", []mentionEntity{mention(botID, "<at>Bot</at>")}, "hi <at>Bot</at>"},
		{"non-mention entity ignored", "hi", []mentionEntity{{Type: "clientInfo", Text: "x", Mentioned: channelAccount{ID: botID}}}, "hi"},
		{"empty entity text ignored", "hi", []mentionEntity{mention(botID, "")}, "hi"},
	}
	for _, tc := range cases {
		act := &inboundActivity{Text: tc.text, Recipient: channelAccount{ID: botID}, Entities: tc.entities}
		asserts.Equal(t, stripRecipientMention(act), tc.want, tc.name)
	}

	// An empty recipient id must never match an entity carrying an empty mentioned id.
	emptyRecip := &inboundActivity{Text: "hi", Entities: []mentionEntity{mention("", "<at>x</at>")}}
	asserts.Equal(t, stripRecipientMention(emptyRecip), "hi", "empty recipient id: no strip")
}

func TestToMessage_StripsBotMention(t *testing.T) {
	body := `{"type":"message","id":"a","text":"<at>Bot</at> echo hi","recipient":{"id":"bot-1"},` +
		`"entities":[{"type":"mention","text":"<at>Bot</at>","mentioned":{"id":"bot-1"}}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal")
	m := toMessage(&act, json.RawMessage(body))
	asserts.Equal(t, m.Content, "echo hi", "Content strips the bot mention so anchored handlers match")
	tm, ok := RawMessage(m)
	asserts.True(t, ok, "raw message present")
	asserts.Equal(t, tm.Text, "<at>Bot</at> echo hi", "raw Text keeps the original mention markup")
}

func TestHandleMessages_DispatchesText(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	done := make(chan struct{}, 1)
	deps := captureDeps(&got, done)

	body := activityJSON("message", "hi there", allowedServiceURL, "user-1", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))

	w := post(a, deps, body, "Bearer "+token)
	asserts.Equal(t, w.Code, http.StatusOK, "valid request should be 200")

	awaitDispatch(t, done, 1)
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].Content, "hi there", "dispatched content")

	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, a.convs["conv-1"].serviceURL, allowedServiceURL, "conversation serviceUrl recorded")
	asserts.Equal(t, a.convs["conv-1"].bot.ID, "bot-1", "bot account recorded for replies")
}

// TestHandleMessages_DispatchesOnDetachedCtx guards the drain: core cancels the run
// context before Disconnect drains in-flight dispatch, so dispatch must ride the
// detached context passed in, not runCtx — otherwise a handler's reply would fail
// with "context canceled" mid-drain.
func TestHandleMessages_DispatchesOnDetachedCtx(t *testing.T) {
	a := testAdapter(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{
		Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c },
	}

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	a.handleMessages(runCtx, dispatchCtx, httptest.NewRecorder(), r, deps)

	select {
	case c := <-gotCtx:
		cancelRun() // core cancels runCtx at shutdown, before draining.
		asserts.NoError(t, c.Err(), "dispatch ctx must not cancel with the run ctx")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestHandleMessages_RejectsMissingToken(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	w := post(a, deps, body, "")

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "no token should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsBadSignature(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	// Sign with the right key but claim the wrong audience.
	token := mintToken(t, testKID, validClaims("someone-else", allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "wrong audience should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsServiceURLClaimMismatch(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	// Token is otherwise valid but its serviceurl claim does not match the body.
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, "https://smba.trafficmanager.net/other/"))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "serviceurl mismatch should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsNonAllowlistedHost(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	const evil = "https://evil.example.com/"
	body := activityJSON("message", "hi", evil, "user-1", "bot-1", "conv-1")
	// Token validates (claim matches the body), but the host is not allowlisted.
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, evil))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusForbidden, "non-allowlisted serviceUrl should be 403")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_DropsBotMessage(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	// A real bot-authored Activity — the bot's own echo, or another bot in a
	// shared channel — has from != recipient (recipient is always this bot) but
	// carries from.role == "bot". That role is what marks it, per the Teams docs.
	body := activityJSONFromRole("bot", allowedServiceURL, "bot-2", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusOK, "still acked")
	// Dispatch is async, so assert on the synchronous side effect: a dropped
	// Activity returns before recordConversation, so no conversation is recorded.
	_, recorded := a.convs["conv-1"]
	asserts.False(t, recorded, "a bot-role message is dropped before dispatch")
	asserts.Equal(t, len(got), 0, "a bot-role message is not dispatched")
}

func TestHandleMessages_IgnoresNonMessage(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("conversationUpdate", "", allowedServiceURL, "user-1", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusOK, "still acked")
	asserts.Equal(t, len(got), 0, "non-message activity not dispatched")
}

func TestPublicKey_RefreshesOnKidMiss(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()

	// Cold cache (keysAt zero ⇒ stale) fetches and resolves the known kid.
	k, err := a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "known kid resolves after fetch")
	asserts.NotNil(t, k, "key returned")
	cached, err := a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "known kid resolves from cache")
	asserts.Equal(t, cached, k, "cached key returned")

	// Now the cache is fresh: an unknown kid is rejected without a re-fetch.
	_, err = a.publicKey(ctx, "rotated-kid")
	asserts.Error(t, err, "unknown kid within refresh interval is rejected")

	// Force staleness ⇒ a miss triggers a re-fetch (still no such kid ⇒ error,
	// but this exercises the refresh path).
	a.mu.Lock()
	a.keysAt = time.Now().Add(-2 * jwksMinRefreshInterval)
	a.mu.Unlock()
	_, err = a.publicKey(ctx, "rotated-kid")
	asserts.Error(t, err, "still unknown after refresh")
}

func TestPublicKey_LargeJWKSDecodes(t *testing.T) {
	// Regression: the real Bot Connector JWKS is ~1 MB (hundreds of keys). A read
	// cap below that truncates the body mid-object and json decode fails with
	// "unexpected EOF", leaving the key map empty so every request 401s. Serve a
	// JWKS larger than the old 256 KiB cap and confirm the target kid resolves.
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		keys := []map[string]string{{
			"kid": testKID,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}
		// Pad with enough filler keys to push the body well past 256 KiB.
		filler := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("a"), 2048))
		for i := 0; i < 200; i++ {
			keys = append(keys, map[string]string{
				"kid": "pad-" + strconv.Itoa(i),
				"kty": "RSA",
				"n":   filler,
				"e":   "AQAB",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = srv.URL + "/openid"

	k, err := a.publicKey(context.Background(), testKID)
	asserts.NoError(t, err, "target kid resolves from a >256 KiB JWKS")
	asserts.NotNil(t, k, "key returned")
}

func TestPublicKey_ConcurrentRefreshRechecksCache(t *testing.T) {
	a := testAdapter(t)

	type result struct {
		key *rsa.PublicKey
		err error
	}
	known := make(chan result, 1)
	missing1 := make(chan result, 1)
	missing2 := make(chan result, 1)
	started := make(chan struct{})
	run := func(kid string, out chan<- result) {
		go func() {
			started <- struct{}{}
			key, err := a.publicKey(context.Background(), kid)
			out <- result{key: key, err: err}
		}()
	}

	// Make every caller observe the stale cache before allowing one of them to
	// refresh it. The remaining callers then re-check the newly populated cache.
	a.fetchMu.Lock()
	run(testKID, known)
	run("missing-1", missing1)
	run("missing-2", missing2)
	<-started
	<-started
	<-started
	time.Sleep(20 * time.Millisecond)
	a.fetchMu.Unlock()

	knownResult := <-known
	asserts.NoError(t, knownResult.err, "known kid")
	asserts.NotNil(t, knownResult.key, "known kid should use refreshed cache")
	asserts.Error(t, (<-missing1).err, "first unknown kid")
	asserts.Error(t, (<-missing2).err, "second unknown kid")
}

func TestPublicKey_RejectsForeignJWKSURI(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	openid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": foreign.URL + "/keys"})
	}))
	defer openid.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = openid.URL

	// jwks_uri pointing at a host other than the OpenID metadata host is an SSRF
	// vector and must be rejected before any key fetch.
	_, err = a.publicKey(context.Background(), "any-kid")
	asserts.Error(t, err, "foreign jwks_uri host must be rejected")
}

func TestPublicKey_ExpiresStaleKeysPastMaxAge(t *testing.T) {
	// A cached kid that Microsoft has rotated out must stop being accepted once
	// the key set ages past jwksMaxAge. The forced refresh replaces the whole map,
	// so the retired kid no longer resolves; without the max-age gate it would be
	// served from cache indefinitely until an unseen-kid token happened to flush it.
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var serveKID, base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": serveKID,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = srv.URL + "/openid"
	ctx := context.Background()

	// First fetch caches the original kid.
	serveKID = "old-kid"
	k, err := a.publicKey(ctx, "old-kid")
	asserts.NoError(t, err, "old kid resolves on first fetch")
	asserts.NotNil(t, k, "key returned")

	// Microsoft rotates: the endpoint now serves only a new kid. Within jwksMaxAge
	// the old kid is still served from cache without a refresh.
	serveKID = "new-kid"
	cached, err := a.publicKey(ctx, "old-kid")
	asserts.NoError(t, err, "old kid still cached within max age")
	asserts.Equal(t, cached, k, "cached key returned within max age")

	// Age the key set past jwksMaxAge: the next lookup forces a refresh that
	// replaces the map and drops the retired kid.
	a.mu.Lock()
	a.keysFreshAt = time.Now().Add(-2 * jwksMaxAge)
	a.keysAt = time.Now().Add(-2 * jwksMaxAge)
	a.mu.Unlock()

	_, err = a.publicKey(ctx, "old-kid")
	asserts.Error(t, err, "retired kid rejected after max-age refresh")

	nk, err := a.publicKey(ctx, "new-kid")
	asserts.NoError(t, err, "rotated-in kid resolves after refresh")
	asserts.NotNil(t, nk, "new key returned")
}

func TestPublicKey_ServesCachedKeyWhenRefreshFails(t *testing.T) {
	// A max-age-triggered refresh that fails must fall back to the cached key so a
	// transient JWKS outage does not reject otherwise-valid tokens (the reference
	// Bot Framework SDK likewise keeps serving cached keys when a refresh fails).
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": testKID,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = srv.URL + "/openid"
	ctx := context.Background()

	k, err := a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "warm cache")
	asserts.NotNil(t, k, "key returned")

	// Simulate a JWKS outage and age the cache past jwksMaxAge.
	srv.Close()
	a.mu.Lock()
	a.keysFreshAt = time.Now().Add(-2 * jwksMaxAge)
	a.keysAt = time.Now().Add(-2 * jwksMaxAge)
	a.mu.Unlock()

	got, err := a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "known kid served from cache when refresh fails")
	asserts.NotNil(t, got, "cached key returned on refresh failure")
}

func TestSend(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-123", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	a.recordConversation("conv-1", srv.URL, channelAccount{ID: "bot-1", Name: "Bot"})

	err = a.Send(context.Background(), "conv-1", "hello world")
	asserts.NoError(t, err, "Send should succeed")
	asserts.Equal(t, gotPath, "/v3/conversations/conv-1/activities", "send URL path")
	asserts.Equal(t, gotAuth, "Bearer tok-123", "bearer token applied")
	asserts.True(t, strings.Contains(gotBody, `"text":"hello world"`), "text in body")
	asserts.True(t, strings.Contains(gotBody, `"type":"message"`), "type in body")
	asserts.True(t, strings.Contains(gotBody, `"from":{"id":"bot-1"`), "bot from account in body")
	asserts.False(t, strings.Contains(gotBody, "recipient"), "no recipient in reply body (delivery is by conversation id)")
}

func TestSend_EscapesConversationID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL

	// A realistic Teams conversation id carries ':' and '@', both valid in a path
	// segment and preserved verbatim by url.PathEscape (it escapes spaces and '/').
	const convID = "19:abc@thread.tacv2"
	a.recordConversation(convID, srv.URL, channelAccount{})

	err = a.Send(context.Background(), convID, "hi")
	asserts.NoError(t, err, "Send should succeed")
	asserts.Equal(t, gotPath, "/v3/conversations/19:abc@thread.tacv2/activities", "conversation id preserved in path")
}

func TestSend_UnknownConversation(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	err = a.Send(context.Background(), "never-seen", "hi")
	asserts.Error(t, err, "Send to unknown conversation should error")
}

func TestSend_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad request")
	}))
	defer srv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	a.recordConversation("conv-1", srv.URL, channelAccount{})

	err = a.Send(context.Background(), "conv-1", "hi")
	asserts.Error(t, err, "non-2xx send should error")
}

func TestSend_RequestError(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.token = cachedToken{value: "token", expiry: time.Now().Add(time.Hour)}
	a.recordConversation("conv-1", ":", channelAccount{})

	err = a.Send(context.Background(), "conv-1", "hi")

	asserts.Error(t, err, "malformed service URL should fail request creation")
}

func TestSend_TransportError(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.token = cachedToken{value: "token", expiry: time.Now().Add(time.Hour)}
	a.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed")
	})}
	a.recordConversation("conv-1", allowedServiceURL, channelAccount{})

	err = a.Send(context.Background(), "conv-1", "hi")

	asserts.Error(t, err, "transport failure should fail Send")
}

func TestAccessToken_Caches(t *testing.T) {
	var hits int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL

	for i := 0; i < 3; i++ {
		tok, err := a.accessToken(context.Background())
		asserts.NoError(t, err, "accessToken")
		asserts.Equal(t, tok, "tok", "token value")
	}
	asserts.Equal(t, hits, 1, "token minted once and cached")
}

func TestRecordConversation_BoundedEviction(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	// Insert one over the cap; the oldest must be evicted.
	for i := 0; i <= maxConversations; i++ {
		a.recordConversation("c"+strconv.Itoa(i), allowedServiceURL, channelAccount{})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, len(a.convs), maxConversations, "map capped at maxConversations")
	_, firstPresent := a.convs["c0"]
	asserts.False(t, firstPresent, "oldest entry evicted")
}

func TestRecordConversation_IgnoresIncompleteMapping(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")

	a.recordConversation("", allowedServiceURL, channelAccount{})
	a.recordConversation("conv-1", "", channelAccount{})

	asserts.Equal(t, len(a.convs), 0, "incomplete mappings should not be recorded")
}

func TestAttachments(t *testing.T) {
	body := `{"type":"message","attachments":[{"contentType":"image/png","contentUrl":"https://x/i.png","name":"i.png"}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")
	// Attachments reads the parse-time attachments carried on the Message, so go
	// through toMessage rather than hand-building a Message with only Raw.
	m := toMessage(&act, json.RawMessage(body))
	atts, err := (&adapter{}).Attachments(m)
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.True(t, atts[0].IsImage, "image attachment flagged")
	asserts.Equal(t, atts[0].URL, "https://x/i.png", "attachment URL")
}

func TestConnectDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}

	asserts.NoError(t, a.Connect(ctx, deps), "Connect should bind and start")
	asserts.NoError(t, a.Disconnect(), "Disconnect should shut down cleanly")
	asserts.NoError(t, a.Disconnect(), "Disconnect should be idempotent")
}

// TestDisconnect_CancelsDispatchContext guards the leak fix: Disconnect must cancel
// the connection's detached dispatch context after the drain, so a handler blocked
// on ctx.Done() cannot leak past shutdown. The connection's cancel is wired by hand
// so the test controls the exact context the handler dispatches on.
func TestDisconnect_CancelsDispatchContext(t *testing.T) {
	a := testAdapter(t)
	dispatchCtx, cancelDispatch := context.WithCancel(context.WithoutCancel(context.Background()))
	// srv must be non-nil so Disconnect runs its teardown rather than early-returning;
	// an unstarted server's Shutdown returns immediately.
	a.mu.Lock()
	a.srv = &http.Server{}
	a.detachedCancel = cancelDispatch
	a.mu.Unlock()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}
	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	a.handleMessages(context.Background(), dispatchCtx, httptest.NewRecorder(), r, deps)

	var c context.Context
	select {
	case c = <-gotCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
	asserts.NoError(t, c.Err(), "dispatch ctx live before Disconnect")
	asserts.NoError(t, a.Disconnect(), "Disconnect")
	asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch ctx canceled after drain")
}

func TestServe_ReportsUnexpectedError(t *testing.T) {
	want := errors.New("accept failed")
	done := make(chan error, 1)

	serve(&http.Server{}, failingListener{err: want}, func(err error) {
		done <- err
	})

	select {
	case got := <-done:
		asserts.ErrorIs(t, got, want, "unexpected serve error should be reported")
	default:
		t.Fatal("unexpected serve error was not reported")
	}
}

// TestAccessToken_Network exercises the real Azure AD client-credentials flow.
// It is hermetic-suite-excluded: skipped unless BOTBOOTER_TEAMS_NETWORK_TEST is
// set together with live app credentials (mirrors BOTBOOTER_SLACK_NETWORK_TEST).
func TestAccessToken_Network(t *testing.T) {
	if os.Getenv("BOTBOOTER_TEAMS_NETWORK_TEST") == "" {
		t.Skip("set BOTBOOTER_TEAMS_NETWORK_TEST to run the live Azure AD token test")
	}
	appID, pw := os.Getenv("TEAMS_APP_ID"), os.Getenv("TEAMS_APP_PASSWORD")
	if appID == "" || pw == "" {
		t.Skip("set TEAMS_APP_ID and TEAMS_APP_PASSWORD for the live token test")
	}
	a, err := newAdapter(Config{AppID: appID, AppPassword: pw, TenantID: os.Getenv("TEAMS_APP_TENANT_ID"), Addr: ":0"})
	asserts.NoError(t, err, "newAdapter")

	tok, err := a.accessToken(context.Background())
	asserts.NoError(t, err, "mint live Bot Connector token")
	asserts.True(t, len(tok) > 0, "non-empty access token")
}

func TestHandleMessages_BadJSON(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	w := post(a, captureDeps(&got, nil), "{not json", "")
	asserts.Equal(t, w.Code, http.StatusBadRequest, "unparseable body should be 400")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_OversizedBody(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	big := strings.Repeat("a", maxRequestBytes+16)
	w := post(a, captureDeps(&got, nil), big, "")
	asserts.Equal(t, w.Code, http.StatusBadRequest, "oversized body should be 400")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestAttachments_NonTeams(t *testing.T) {
	atts, err := (&adapter{}).Attachments(&core.Message{Raw: "not-a-teams-message"})
	asserts.NoError(t, err, "non-Teams message")
	asserts.True(t, atts == nil, "nil attachments for non-Teams message")
}

func TestAttachments_None(t *testing.T) {
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(`{"type":"message"}`), &act), "unmarshal activity")
	m := toMessage(&act, json.RawMessage(`{"type":"message"}`))
	atts, err := (&adapter{}).Attachments(m)
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 0, "no attachments")
}

func TestAccessToken_HTTPError(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer tokenSrv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	_, err = a.accessToken(context.Background())
	asserts.Error(t, err, "non-2xx token response should error")
}

func TestAccessToken_RequestError(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = ":"

	_, err = a.accessToken(context.Background())

	asserts.Error(t, err, "malformed token URL should fail request creation")
}

func TestAccessToken_TransportError(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed")
	})}

	_, err = a.accessToken(context.Background())

	asserts.Error(t, err, "transport failure should fail token request")
}

func TestAccessToken_InvalidJSON(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not-json")
	}))
	defer tokenSrv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL

	_, err = a.accessToken(context.Background())

	asserts.Error(t, err, "invalid token response should fail decoding")
}

func TestAccessToken_MissingToken(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"expires_in": 3600})
	}))
	defer tokenSrv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	_, err = a.accessToken(context.Background())
	asserts.Error(t, err, "missing access_token should error")
}

func TestAccessToken_ExpiresInFloor(t *testing.T) {
	var hits int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		// No expires_in: the floor must still cache it rather than re-mint each call.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
	}))
	defer tokenSrv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	for i := 0; i < 3; i++ {
		_, err := a.accessToken(context.Background())
		asserts.NoError(t, err, "accessToken")
	}
	asserts.Equal(t, hits, 1, "missing expires_in still caches via the floor")
}

func TestSend_TokenError(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer tokenSrv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	a.recordConversation("conv-1", allowedServiceURL, channelAccount{})
	err = a.Send(context.Background(), "conv-1", "hi")
	asserts.Error(t, err, "token failure should fail Send")
}

func TestPublicKey_OpenIDError(t *testing.T) {
	openid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer openid.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = openid.URL
	_, err = a.publicKey(context.Background(), "kid")
	asserts.Error(t, err, "OpenID metadata error should propagate")
}

func TestValidateInbound_RejectsUnexpectedSigningMethod(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(a.cfg.AppID, allowedServiceURL))
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString([]byte("secret"))
	asserts.NoError(t, err, "sign token")

	err = a.validateInbound(context.Background(), "Bearer "+raw, allowedServiceURL)

	asserts.ErrorIs(t, err, errUnauthorized, "non-RS256 token should be unauthorized")
}

func TestFetchJWKS_MissingURI(t *testing.T) {
	openid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer openid.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = openid.URL

	_, err = a.fetchJWKS(context.Background())

	asserts.Error(t, err, "metadata without jwks_uri should fail")
}

func TestFetchJWKS_KeyEndpointError(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	defer srv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = base + "/openid"

	_, err = a.fetchJWKS(context.Background())

	asserts.Error(t, err, "JWKS endpoint error should propagate")
}

func TestFetchJWKS_SkipsInvalidKeys(t *testing.T) {
	pub := signingKey(t).Public().(*rsa.PublicKey)
	validN := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	validE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kid": "not-rsa", "kty": "EC", "n": validN, "e": validE},
			{"kid": "bad-modulus", "kty": "RSA", "n": "!", "e": validE},
			{"kid": "bad-exponent", "kty": "RSA", "n": validN, "e": "!"},
			{"kid": "wide-exponent", "kty": "RSA", "n": validN, "e": base64.RawURLEncoding.EncodeToString(make([]byte, 9))},
			{"kid": "small-modulus", "kty": "RSA", "n": base64.RawURLEncoding.EncodeToString([]byte{1}), "e": validE},
			{"kid": testKID, "kty": "RSA", "n": validN, "e": validE},
		}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	defer srv.Close()
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = base + "/openid"

	keys, err := a.fetchJWKS(context.Background())

	asserts.NoError(t, err, "valid key should keep JWKS usable")
	asserts.Equal(t, len(keys), 1, "invalid keys should be skipped")
	asserts.NotNil(t, keys[testKID], "valid key should be retained")
}

func TestPublicKey_NoUsableKeys(t *testing.T) {
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	defer srv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = base + "/openid"
	_, err = a.publicKey(context.Background(), "kid")
	asserts.Error(t, err, "empty JWKS should error")
}

func TestConnect_BadAddr(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.cfg.Addr = "127.0.0.1:999999" // port out of range
	err = a.Connect(context.Background(), core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	asserts.Error(t, err, "binding an invalid address should fail Connect")
}

func TestConnect_RoutesWebhookMethods(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "reserve loopback address")
	addr := ln.Addr().String()
	asserts.NoError(t, ln.Close(), "release loopback address")

	cfg := validConfig()
	cfg.Addr = addr
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := core.AdapterDeps{Done: func(error) {}, Disconnect: a.Disconnect}
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	defer func() { asserts.NoError(t, a.Disconnect(), "Disconnect") }()

	resp, err := http.Get("http://" + addr + a.cfg.Path)
	asserts.NoError(t, err, "GET webhook")
	asserts.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed, "webhook should require POST")
	_ = resp.Body.Close()

	resp, err = http.Post("http://"+addr+a.cfg.Path, "application/json", strings.NewReader("{"))
	asserts.NoError(t, err, "POST webhook")
	defer func() { _ = resp.Body.Close() }()
	asserts.Equal(t, resp.StatusCode, http.StatusBadRequest, "POST should reach message handler")
}

func TestGetJSON_Errors(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		err = a.getJSON(context.Background(), ":", &struct{}{})
		asserts.Error(t, err, "malformed URL should fail request creation")
	})

	t.Run("transport", func(t *testing.T) {
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		a.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport failed")
		})}
		err = a.getJSON(context.Background(), "https://example.com", &struct{}{})
		asserts.Error(t, err, "transport failure should propagate")
	})

	t.Run("decode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "not-json")
		}))
		defer srv.Close()
		a, err := newAdapter(validConfig())
		asserts.NoError(t, err, "newAdapter")
		err = a.getJSON(context.Background(), srv.URL, &struct{}{})
		asserts.Error(t, err, "invalid JSON should fail decoding")
	})
}

func TestConnect_ContextCancelDisconnects(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	ctx, cancel := context.WithCancel(context.Background())
	disc := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Done: func(error) {},
		Disconnect: func() error {
			disc <- struct{}{}
			return a.Disconnect()
		},
	}
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	cancel()
	select {
	case <-disc:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not trigger Disconnect")
	}
}

func TestDrainDispatch_ContextCancel(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.inflight.Add(1) // never decremented
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.drainDispatch(ctx) // must return promptly on a canceled ctx
	asserts.Equal(t, a.inflight.Load(), int64(1), "drain returns on ctx cancel without waiting")
}

func TestDrainDispatch_WaitsForInflight(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.inflight.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		a.inflight.Add(-1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(ctx)

	asserts.Equal(t, a.inflight.Load(), int64(0), "drain should wait until in-flight dispatch reaches zero")
}
