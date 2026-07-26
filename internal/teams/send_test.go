package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func TestSend_RetriesOnceOn401(t *testing.T) {
	var attempts int
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		tokens = append(tokens, r.Header.Get("Authorization"))
		if attempts == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mints int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", mints), "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL
	a.recordConversation("conv-1", srv.URL, channelAccount{ID: "bot-1", Name: "Bot"})

	err = a.Send(context.Background(), "conv-1", "hi", core.SendOptions{})
	asserts.NoError(t, err, "Send succeeds after a 401-triggered token refresh")
	asserts.Equal(t, attempts, 2, "exactly one retry after 401")
	asserts.Equal(t, mints, 2, "token minted fresh for the retry")
	asserts.Equal(t, tokens[0], "Bearer tok-1", "first attempt used the initially cached token")
	asserts.Equal(t, tokens[1], "Bearer tok-2", "retry used a freshly minted token")
}

func TestSend_UnknownConversationSentinel(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	err = a.Send(context.Background(), "never-seen", "hi", core.SendOptions{})
	asserts.ErrorIs(t, err, ErrUnknownConversation, "unknown conversation must be ErrUnknownConversation")
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

	err = a.Send(context.Background(), "conv-1", "hello world", core.SendOptions{})
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

	err = a.Send(context.Background(), convID, "hi", core.SendOptions{})
	asserts.NoError(t, err, "Send should succeed")
	asserts.Equal(t, gotPath, "/v3/conversations/19:abc@thread.tacv2/activities", "conversation id preserved in path")
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

	err = a.Send(context.Background(), "conv-1", "hi", core.SendOptions{})
	asserts.Error(t, err, "non-2xx send should error")
}

func TestSend_RequestError(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.token = cachedToken{value: "token", expiry: time.Now().Add(time.Hour)}
	a.recordConversation("conv-1", ":", channelAccount{})

	err = a.Send(context.Background(), "conv-1", "hi", core.SendOptions{})

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

	err = a.Send(context.Background(), "conv-1", "hi", core.SendOptions{})

	asserts.Error(t, err, "transport failure should fail Send")
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
	err = a.Send(context.Background(), "conv-1", "hi", core.SendOptions{})
	asserts.Error(t, err, "token failure should fail Send")
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

// TestAccessToken_ConcurrentMintCoalesces proves the single-flight: a burst of
// concurrent cold Sends must mint one token, not one per caller. tokenMu is held
// so every goroutine passes its first (empty) cache check and blocks on the mint
// lock; releasing it lets one caller mint while the rest re-check the freshly
// cached token (mirroring the JWKS fetchMu coalescing test).
func TestAccessToken_ConcurrentMintCoalesces(t *testing.T) {
	var mints atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.tokenURL = tokenSrv.URL

	const n = 8
	started := make(chan struct{}, n)
	results := make(chan error, n)
	a.tokenMu.Lock()
	for i := 0; i < n; i++ {
		go func() {
			started <- struct{}{}
			_, err := a.accessToken(context.Background())
			results <- err
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	time.Sleep(20 * time.Millisecond) // let all callers reach the mint lock
	a.tokenMu.Unlock()

	for i := 0; i < n; i++ {
		asserts.NoError(t, <-results, "accessToken")
	}
	asserts.Equal(t, mints.Load(), int64(1), "concurrent cold mints coalesced into a single token request")
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
