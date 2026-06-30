package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
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

// post drives handleMessages with a body and Authorization header, returning the
// recorder.
func post(a *adapter, deps core.AdapterDeps, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	a.handleMessages(context.Background(), w, r, deps)
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
	asserts.Equal(t, a.convs["conv-1"], allowedServiceURL, "conversation serviceUrl recorded")
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

	// from.id == recipient.id marks the bot's own message.
	body := activityJSON("message", "echo", allowedServiceURL, "bot-1", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusOK, "still acked")
	asserts.Equal(t, len(got), 0, "bot's own message not dispatched")
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
	a.recordConversation("conv-1", srv.URL)

	err = a.Send(context.Background(), "conv-1", "hello world")
	asserts.NoError(t, err, "Send should succeed")
	asserts.Equal(t, gotPath, "/v3/conversations/conv-1/activities", "send URL path")
	asserts.Equal(t, gotAuth, "Bearer tok-123", "bearer token applied")
	asserts.True(t, strings.Contains(gotBody, `"text":"hello world"`), "text in body")
	asserts.True(t, strings.Contains(gotBody, `"type":"message"`), "type in body")
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
	a.recordConversation(convID, srv.URL)

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
	a.recordConversation("conv-1", srv.URL)

	err = a.Send(context.Background(), "conv-1", "hi")
	asserts.Error(t, err, "non-2xx send should error")
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
		a.recordConversation("c"+strconv.Itoa(i), allowedServiceURL)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, len(a.convs), maxConversations, "map capped at maxConversations")
	_, firstPresent := a.convs["c0"]
	asserts.False(t, firstPresent, "oldest entry evicted")
}

func TestAttachments(t *testing.T) {
	body := `{"type":"message","attachments":[{"contentType":"image/png","contentUrl":"https://x/i.png","name":"i.png"}]}`
	m := &core.Message{Raw: &Message{Raw: json.RawMessage(body)}}
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
	a, err := newAdapter(Config{AppID: appID, AppPassword: pw, TenantID: os.Getenv("TEAMS_TENANT_ID"), Addr: ":0"})
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
	m := &core.Message{Raw: &Message{Raw: json.RawMessage(`{"type":"message"}`)}}
	atts, err := (&adapter{}).Attachments(m)
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 0, "no attachments")
}

func TestAttachments_BadRawJSON(t *testing.T) {
	m := &core.Message{Raw: &Message{Raw: json.RawMessage(`{bad`)}}
	_, err := (&adapter{}).Attachments(m)
	asserts.Error(t, err, "unparseable raw should error")
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
	a.recordConversation("conv-1", allowedServiceURL)
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
