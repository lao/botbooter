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
	// botConnectorIssuer is the issuer of production Bot Connector tokens. (The
	// Emulator uses an AAD issuer and is not supported.)
	botConnectorIssuer = "https://api.botframework.com"
	// openIDConfigURL is the Bot Connector OpenID metadata document; its jwks_uri
	// points at the signing keys for inbound tokens.
	openIDConfigURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"

	// jwksMinRefreshInterval rate-limits JWKS re-fetches so a flood of tokens with
	// unknown kids cannot turn into a fetch-per-request DoS.
	jwksMinRefreshInterval = time.Minute
	// jwksMaxAge forces a full refresh even for a known kid once the key set ages
	// out, so a key Microsoft has retired stops being trusted. Matches the
	// reference SDK's 24h interval.
	jwksMaxAge = 24 * time.Hour
)

// allowedServiceHosts / allowedServiceHostSuffixes are the only hosts replies may
// be POSTed to. The Activity's serviceUrl is attacker-influenced, so even a valid
// token must not aim outbound requests at an arbitrary host. The broad
// *.trafficmanager.net namespace (shared by every Azure tenant) is deliberately NOT
// allowlisted; only smba.trafficmanager.net and *.botframework.com are.
var (
	allowedServiceHosts        = map[string]bool{"botframework.com": true, "smba.trafficmanager.net": true}
	allowedServiceHostSuffixes = []string{".botframework.com"}
)

// errUnauthorized marks an inbound request that fails JWT validation.
var errUnauthorized = errors.New("teams: unauthorized inbound request")

// jwksKey is one Bot Connector signing key: its RSA public key plus the channel
// endorsements from the JWKS. A token signed by it may only authenticate an
// Activity whose channelId is endorsed; an empty list is the Emulator/Skill
// exemption. See validateInbound.
type jwksKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// validateInbound verifies the Bearer JWT on an inbound request: RS256 signature
// against the JWKS, audience == AppID, issuer, exp, a serviceurl claim matching the
// Activity, and a signing key endorsed for the Activity's channelId. The JWKS is
// shared across all of a resource's channels and aud/iss/signature alone cannot
// distinguish them; the endorsement is the only thing binding a key to a channel,
// so without it a token minted for another enabled channel could pose as Teams.
// This authenticates the channelId, not the from account.
func (a *adapter) validateInbound(ctx context.Context, authHeader, activityServiceURL, activityChannelID string) error {
	raw, ok := bearerToken(authHeader)
	if !ok {
		return fmt.Errorf("%w: missing or malformed Authorization header", errUnauthorized)
	}

	claims := jwt.MapClaims{}
	// Capture the signing key's endorsements during verification so the channel
	// check below uses the key that actually signed it, without a second lookup.
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
		// golang-jwt returns specific sentinels, so wrapping err names the exact
		// failing check. iss/aud are echoed from the rejected token for comparison.
		gotIss, _ := claims["iss"].(string)
		gotAud := claims["aud"]
		// %q (not %v): the rejected token's aud is attacker-controlled and logged,
		// so escape it to prevent log injection.
		return fmt.Errorf("%w: token validation failed (want aud=%q iss=%q; got aud=%q iss=%q): %w",
			errUnauthorized, a.cfg.AppID, botConnectorIssuer, gotAud, gotIss, err)
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

	// A token may only speak for a channel its key is endorsed for. An empty list is
	// the Emulator/Skill exemption, trusted to pass as the reference SDKs do. An
	// endorsed key with a blank channelId is rejected rather than skipped.
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
	a.mu.Lock()
	k := a.keys[kid]
	fresh := time.Since(a.keysFreshAt) < jwksMaxAge
	rateLimited := time.Since(a.keysAt) < jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil && fresh {
		return k, nil
	}
	if rateLimited {
		// Rate-limited: serve the cached key if present, else report the unknown kid.
		if k != nil {
			return k, nil
		}
		return nil, fmt.Errorf("teams: unknown signing key %q", kid)
	}

	// Serialize refreshes so a burst makes a single fetch. After winning fetchMu,
	// re-check: a concurrent refresh may have already resolved this kid.
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()

	a.mu.Lock()
	k = a.keys[kid]
	fresh = time.Since(a.keysFreshAt) < jwksMaxAge
	rateLimited = time.Since(a.keysAt) < jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil && fresh {
		return k, nil
	}
	if rateLimited {
		if k != nil {
			return k, nil
		}
		return nil, fmt.Errorf("teams: unknown signing key %q", kid)
	}

	// Advance keysAt (the attempt clock) before fetching so a failed fetch is still
	// rate-limited. keysFreshAt (the max-age clock) advances only on success below.
	a.mu.Lock()
	a.keysAt = time.Now()
	a.mu.Unlock()

	keys, err := a.fetchJWKS(ctx)
	if err != nil {
		// Fall back to the cached key so a transient outage does not reject valid
		// tokens; a genuine unknown-kid miss surfaces err.
		if k != nil {
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

// fetchJWKS resolves the Bot Connector OpenID metadata to its jwks_uri and decodes
// the RSA signing keys (with their channel endorsements) into a kid-indexed map.
func (a *adapter) fetchJWKS(ctx context.Context) (map[string]*jwksKey, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, a.openIDURL, &meta); err != nil {
		return nil, err
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("teams: openid metadata missing jwks_uri")
	}
	// Pin jwks_uri to the metadata endpoint's scheme and host so a tampered document
	// cannot redirect key fetching elsewhere or downgrade it to cleartext. Matching
	// the metadata scheme (not a hardcoded "https") keeps the http httptest tests
	// exercising this path.
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
		// Reject implausible keys (tiny modulus or degenerate exponent) to keep the map clean.
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

// bearerToken extracts the credential from an Authorization header. The scheme is
// matched case-insensitively and extra whitespace is tolerated (RFC 7235/6750), so
// "bearer  <t>" is accepted. Lenient parsing does not weaken auth: the token is
// still fully validated by the JWT parser.
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

// sameSchemeHost reports whether two URLs share a scheme and host, comparing
// case-insensitively and ignoring any explicit port (via url.Hostname). It pins a
// fetched jwks_uri to the origin of the OpenID metadata endpoint.
func sameSchemeHost(a, b string) bool {
	au, aErr := url.Parse(a)
	bu, bErr := url.Parse(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return strings.EqualFold(au.Scheme, bu.Scheme) && strings.EqualFold(au.Hostname(), bu.Hostname())
}
