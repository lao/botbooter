package teams

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lao/botbooter/internal/asserts"
)

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

func TestValidateInbound_RejectsUnexpectedSigningMethod(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(a.cfg.AppID, allowedServiceURL))
	tok.Header["kid"] = testKID
	raw, err := tok.SignedString([]byte("secret"))
	asserts.NoError(t, err, "sign token")

	err = a.validateInbound(context.Background(), "Bearer "+raw, allowedServiceURL, "msteams")

	asserts.ErrorIs(t, err, errUnauthorized, "non-RS256 token should be unauthorized")
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
	pub := signingKey(t).Public().(*rsa.PublicKey)
	var keyFetches atomic.Int64
	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		keyFetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": testKID,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.openIDURL = srv.URL + "/openid"

	type result struct {
		key *jwksKey
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

	// Hold fetchMu so every caller passes its first cache check (cache empty,
	// keysAt zero, so not rate-limited) and blocks on the refresh lock before any
	// fetch sets keysAt; releasing it lets exactly one caller fetch while the rest
	// re-check the freshly populated cache. The short settle lets all three reach
	// fetchMu; the fetch-count assertion below then proves the coalescing actually
	// happened rather than silently not exercising the recheck path.
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
	asserts.Equal(t, keyFetches.Load(), int64(1), "the concurrent burst coalesced into a single JWKS fetch")
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

	// Simulate a JWKS outage and age the cache past jwksMaxAge (forcing a
	// refresh) but within jwksHardMaxAge, so the transient-outage fallback still
	// serves the cached key.
	srv.Close()
	a.mu.Lock()
	a.keysFreshAt = time.Now().Add(-jwksMaxAge - time.Hour)
	a.keysAt = time.Now().Add(-2 * jwksMaxAge)
	a.mu.Unlock()

	got, err := a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "known kid served from cache when refresh fails within the hard ceiling")
	asserts.NotNil(t, got, "cached key returned on refresh failure")
}

// TestPublicKey_RejectsStaleCacheBeyondHardCeiling guards the retirement bound:
// once the cached key set is older than jwksHardMaxAge, a failed refresh must
// reject it rather than keep trusting a possibly-retired key indefinitely.
func TestPublicKey_RejectsStaleCacheBeyondHardCeiling(t *testing.T) {
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

	_, err = a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "warm cache")

	// Outage plus a cache aged beyond the hard ceiling: the key must be rejected.
	srv.Close()
	a.mu.Lock()
	a.keysFreshAt = time.Now().Add(-jwksHardMaxAge - time.Hour)
	a.keysAt = time.Now().Add(-2 * jwksHardMaxAge)
	a.mu.Unlock()

	_, err = a.publicKey(ctx, testKID)
	asserts.Error(t, err, "stale-beyond-ceiling cached key must be rejected when refresh fails")
}

// TestPublicKey_RejectsStaleCacheInRateLimitWindow guards the hard ceiling on the
// rate-limited fast path: a just-attempted (rate-limited) lookup of a cache aged
// beyond jwksHardMaxAge must reject rather than serve the retired key. Without
// the ceiling check in lookupCached this path would serve a stale key for most
// requests during a sustained outage.
func TestPublicKey_RejectsStaleCacheInRateLimitWindow(t *testing.T) {
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

	_, err = a.publicKey(ctx, testKID)
	asserts.NoError(t, err, "warm cache")

	// Cache aged beyond the hard ceiling, and a fetch was "just attempted" so the
	// rate-limit fast path is taken (no fetch this call).
	a.mu.Lock()
	a.keysFreshAt = time.Now().Add(-jwksHardMaxAge - time.Hour)
	a.keysAt = time.Now()
	a.mu.Unlock()

	_, err = a.publicKey(ctx, testKID)
	asserts.Error(t, err, "rate-limited fast path must not serve a key aged beyond the hard ceiling")
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

// TestFetchJWKS_IgnoresParentCancellation guards the shared-cache refresh: a
// JWKS fetch runs on behalf of every in-flight request, so a single client
// canceling its request must not abort the refresh. publicKey advances the
// rate-limit clock (keysAt) before fetching, so an aborted fetch would also burn
// the 1-minute refresh window and 401 subsequent legitimate requests. fetchJWKS
// therefore detaches from the caller's cancellation; its own 15s timeout still
// bounds it.
func TestFetchJWKS_IgnoresParentCancellation(t *testing.T) {
	a := testAdapter(t) // openIDURL points at a working JWKS server

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's request is already gone

	keys, err := a.fetchJWKS(ctx)

	asserts.NoError(t, err, "fetchJWKS must ignore parent cancellation")
	asserts.NotNil(t, keys[testKID], "JWKS still fetched despite a canceled request context")
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
