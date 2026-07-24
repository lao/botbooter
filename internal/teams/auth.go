// Inbound authentication and reply-target safety: Bot Connector JWT validation
// against a cached, rate-limited JWKS, plus the SSRF allowlist and URL helpers that
// pin replies and the JWKS fetch to trusted hosts.

package teams

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// The Emulator uses an AAD issuer and is not supported.
	botConnectorIssuer = "https://api.botframework.com"
	openIDConfigURL    = "https://login.botframework.com/v1/.well-known/openidconfiguration"

	// jwksMinRefreshInterval rate-limits JWKS re-fetches so unknown kids cannot
	// turn into a fetch-per-request DoS.
	jwksMinRefreshInterval = time.Minute
	// jwksMaxAge forces a full refresh even for a known kid, so a key Microsoft
	// has retired stops being trusted. Matches the reference SDK's 24h interval.
	jwksMaxAge = 24 * time.Hour
	// jwksHardMaxAge bounds the fetch-failure fallback: past this age a stale
	// cached key is rejected outright, so a persistent outage cannot keep a
	// retired signing key trusted forever.
	jwksHardMaxAge = 2 * jwksMaxAge
	// jwksFetchTimeout bounds a cold JWKS resolution (metadata + keys) so it
	// stays under the server's WriteTimeout.
	jwksFetchTimeout = 15 * time.Second
)

// allowedServiceHosts / allowedServiceHostSuffixes are the only hosts replies may
// be POSTed to: the Activity's serviceUrl is attacker-influenced. The broad
// *.trafficmanager.net namespace (shared by every Azure tenant) is deliberately
// NOT allowlisted.
var (
	allowedServiceHosts        = map[string]bool{"botframework.com": true, "smba.trafficmanager.net": true}
	allowedServiceHostSuffixes = []string{".botframework.com"}
)

// errUnauthorized marks an inbound request that fails JWT validation.
var errUnauthorized = errors.New("teams: unauthorized inbound request")

// jwksKey is one Bot Connector signing key: its RSA public key plus the channel
// endorsements from the JWKS. A token signed by it may only authenticate an
// Activity whose channelId is endorsed; an empty list is the Emulator/Skill
// exemption.
type jwksKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// validateInbound verifies the bearer token on an inbound request: RS256
// signature against the JWKS, audience == AppID, issuer, exp, a serviceurl claim
// matching the Activity, and a signing key endorsed for the Activity's channelId
// (the endorsement is the only thing binding a key to a channel). This
// authenticates the channelId, not the from account.
func (a *adapter) validateInbound(ctx context.Context, authHeader, activityServiceURL, activityChannelID string) error {
	raw, ok := bearerToken(authHeader)
	if !ok {
		return fmt.Errorf("%w: missing or malformed Authorization header", errUnauthorized)
	}

	claims := jwt.MapClaims{}
	// Capture the signing key's endorsements during verification so the channel
	// check uses the key that actually signed the token.
	var keyEndorsements []string
	keyFunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		jk, err := a.publicKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		keyEndorsements = jk.endorsements
		return jk.pub, nil
	}
	tok, err := jwt.ParseWithClaims(raw, claims, keyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(a.cfg.AppID),
		jwt.WithIssuer(botConnectorIssuer),
		jwt.WithExpirationRequired(),
		// 5-minute clock-skew tolerance, matching the reference SDKs.
		jwt.WithLeeway(5*time.Minute),
	)
	if err != nil {
		gotIss, _ := claims["iss"].(string)
		gotAud := claims["aud"]
		gotAudStr := fmt.Sprintf("%v", gotAud)
		// %q: the rejected token's aud is attacker-controlled and logged, so
		// escape it to prevent log injection.
		return fmt.Errorf("%w: token validation failed (want aud=%q iss=%q; got aud=%q iss=%q): %w",
			errUnauthorized, a.cfg.AppID, botConnectorIssuer, gotAudStr, gotIss, err)
	}
	if !tok.Valid {
		return fmt.Errorf("%w: token reported invalid", errUnauthorized)
	}

	// The claim name is lowercase "serviceurl"; MapClaims lookup is case-sensitive.
	svc, _ := claims["serviceurl"].(string)
	if svc == "" {
		return fmt.Errorf("%w: token missing serviceurl claim", errUnauthorized)
	}
	if !sameServiceURL(svc, activityServiceURL) {
		return fmt.Errorf("%w: serviceurl claim %q does not match activity serviceUrl %q",
			errUnauthorized, svc, activityServiceURL)
	}

	// A token may only speak for a channel its key is endorsed for; an empty list
	// is the Emulator/Skill exemption, trusted as the reference SDKs do.
	if len(keyEndorsements) > 0 {
		if activityChannelID == "" {
			return fmt.Errorf("%w: activity missing channelId for an endorsed signing key", errUnauthorized)
		}
		if !slices.Contains(keyEndorsements, activityChannelID) {
			return fmt.Errorf("%w: signing key not endorsed for channel %q", errUnauthorized, activityChannelID)
		}
	}
	return nil
}

// publicKey returns the signing key for kid. A cached key is served while the set
// is within jwksMaxAge; past that a refresh is forced even on a hit so retired keys
// stop being trusted. An unknown kid also triggers a refresh. Refreshes run at most
// once per jwksMinRefreshInterval, and a failed one falls back to the cached key so
// a transient outage does not reject valid tokens.
func (a *adapter) publicKey(ctx context.Context, kid string) (*jwksKey, error) {
	if k, done, err := a.lookupCached(kid); done {
		return k, err
	}

	// Serialize refreshes so a burst makes a single fetch; re-check after winning
	// fetchMu since a concurrent refresh may have resolved this kid.
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()

	k, done, err := a.lookupCached(kid)
	if done {
		return k, err
	}

	// Advance keysAt (the attempt clock) before fetching so a failed fetch is
	// still rate-limited; keysFreshAt (the max-age clock) advances only on success.
	a.mu.Lock()
	a.keysAt = time.Now()
	a.mu.Unlock()

	keys, err := a.fetchJWKS(ctx)
	if err != nil {
		// Fall back to the cached key so a transient outage does not reject valid
		// tokens — but only within jwksHardMaxAge, so a persistent outage cannot
		// keep a retired key trusted indefinitely.
		a.mu.Lock()
		withinHardCeiling := time.Since(a.keysFreshAt) < jwksHardMaxAge
		a.mu.Unlock()
		if k != nil && withinHardCeiling {
			return k, nil
		}
		return nil, err
	}
	a.mu.Lock()
	a.keys = keys
	a.keysFreshAt = time.Now()
	k = a.keys[kid]
	a.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("teams: unknown signing key %q after refresh", kid)
	}
	return k, nil
}

// lookupCached reads the cache for kid and reports whether publicKey should
// return immediately: a fresh hit returns (k, true, nil); when a refresh is
// rate-limited it returns the cached key (k, true, nil) or an unknown-kid error
// (nil, true, err); otherwise it returns (k, false, nil) — k may be a stale key
// to use as the fetch-failure fallback — signaling the caller to fetch.
func (a *adapter) lookupCached(kid string) (*jwksKey, bool, error) {
	a.mu.Lock()
	k := a.keys[kid]
	fresh := time.Since(a.keysFreshAt) < jwksMaxAge
	withinHardCeiling := time.Since(a.keysFreshAt) < jwksHardMaxAge
	rateLimited := time.Since(a.keysAt) < jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil && fresh {
		return k, true, nil
	}
	if rateLimited {
		// The rate-limit fast path serves most requests during an outage, so it
		// must honor jwksHardMaxAge too, or a retired key would be served
		// indefinitely between fetch attempts.
		if k != nil && withinHardCeiling {
			return k, true, nil
		}
		return nil, true, fmt.Errorf("teams: unknown signing key %q", kid)
	}
	return k, false, nil
}

// fetchJWKS resolves the Bot Connector OpenID metadata to its jwks_uri and decodes
// the RSA signing keys (with their channel endorsements) into a kid-indexed map.
func (a *adapter) fetchJWKS(ctx context.Context) (map[string]*jwksKey, error) {
	// Detach from the caller's cancellation: this refresh serves the shared key
	// cache for every in-flight request, and publicKey advances the rate-limit
	// clock (keysAt) before calling here. If one client canceling its request
	// aborted the fetch, that failure would also burn the 1-minute refresh window
	// and 401 subsequent legitimate requests. The own 15s timeout below still
	// bounds the two-hop resolution independently of the server's WriteTimeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jwksFetchTimeout)
	defer cancel()

	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, a.openIDURL, &meta); err != nil {
		return nil, err
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("teams: openid metadata missing jwks_uri")
	}
	// Pin jwks_uri to the metadata endpoint's scheme and host so a tampered
	// document cannot redirect key fetching elsewhere. Matching the metadata
	// scheme (not a hardcoded "https") keeps the httptest tests on this path.
	if !sameSchemeHost(a.openIDURL, meta.JWKSURI) {
		return nil, errors.New("teams: jwks_uri scheme/host does not match the OpenID metadata endpoint")
	}

	var set struct {
		Keys []struct {
			Kid          string   `json:"kid"`
			Kty          string   `json:"kty"`
			N            string   `json:"n"`
			E            string   `json:"e"`
			Endorsements []string `json:"endorsements"`
		} `json:"keys"`
	}
	if err := a.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return nil, err
	}

	keys := make(map[string]*jwksKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(eBytes) == 0 || len(eBytes) > 8 {
			// An exponent wider than 8 bytes would silently truncate via Int64.
			continue
		}
		n := new(big.Int).SetBytes(nBytes)
		e := int(new(big.Int).SetBytes(eBytes).Int64())
		// Reject implausible keys (tiny modulus or degenerate exponent).
		if n.BitLen() < 1024 || e < 2 {
			continue
		}
		keys[k.Kid] = &jwksKey{pub: &rsa.PublicKey{N: n, E: e}, endorsements: k.Endorsements}
	}
	if len(keys) == 0 {
		return nil, errors.New("teams: no usable RSA keys in JWKS")
	}
	return keys, nil
}

func isAllowedServiceHost(serviceURL string) bool {
	u, err := url.Parse(serviceURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if allowedServiceHosts[host] {
		return true
	}
	for _, suffix := range allowedServiceHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// sameServiceURL compares two serviceUrls ignoring a trailing slash and case,
// matching the reference SDK.
func sameServiceURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// bearerToken extracts the credential from an Authorization header, tolerating
// case and extra whitespace per RFC 7235/6750; the token is still fully
// validated by the JWT parser.
func bearerToken(authHeader string) (string, bool) {
	const scheme = "bearer"
	h := strings.TrimSpace(authHeader)
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	// The scheme must be followed by whitespace, else "BearerX" would match.
	rest := h[len(scheme):]
	if rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// sameSchemeHost reports whether two URLs share a scheme and host,
// case-insensitively and ignoring any explicit port.
func sameSchemeHost(a, b string) bool {
	au, aErr := url.Parse(a)
	bu, bErr := url.Parse(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return strings.EqualFold(au.Scheme, bu.Scheme) && strings.EqualFold(au.Hostname(), bu.Hostname())
}
