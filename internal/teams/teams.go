// Package teams is the Microsoft Teams adapter for botbooter. It receives
// messages from the Azure Bot Framework over an inbound webhook and sends
// replies back through the Bot Connector REST API. It implements core.Adapter.
//
// Like the WhatsApp adapter — and unlike the dial-out adapters (Slack, Discord,
// Telegram) — the Bot Framework delivers inbound messages as HTTP webhook
// callbacks, so this adapter runs its own HTTP server: Connect binds a listener
// and serves until the run context is canceled, and Disconnect shuts the server
// down. Bind a local Addr, put a TLS-terminating reverse proxy in front, and
// register the public HTTPS URL as the messaging endpoint of your Azure Bot
// resource.
//
// Security: every inbound request is authenticated by validating the Bot
// Connector JWT (JWKS signature, audience == AppID, issuer, and a serviceurl
// claim that must match the Activity's serviceUrl), and outbound replies are
// only sent to allowlisted Bot Framework hosts. golang-jwt/jwt/v5 performs the
// signature/claims verification; the JWKS-to-key step is plain stdlib.
//
// Operator responsibilities: the JWT authenticates the Bot Connector and binds
// the serviceUrl, but the Activity body itself is channel-trusted (not
// individually signed) and there is no replay/jti tracking — so terminate TLS at
// a trusted reverse proxy and never expose the bind Addr in cleartext. The
// adapter does no application-level rate limiting; do that at the proxy.
package teams

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath = "/api/messages"

	// botConnectorIssuer is the issuer of production Bot Connector tokens. (The
	// Bot Framework Emulator uses an AAD issuer and is not supported; see README.)
	botConnectorIssuer = "https://api.botframework.com"
	// openIDConfigURL is the Bot Connector OpenID metadata document; its jwks_uri
	// points at the signing keys for inbound tokens.
	openIDConfigURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// tokenScope requests an app-only token for the Bot Connector service.
	tokenScope = "https://api.botframework.com/.default"

	// maxRequestBytes caps the inbound webhook body. The endpoint is public, so
	// this defends against memory exhaustion; real Activities are a few KB.
	maxRequestBytes = 1 << 20 // 1 MiB
	// maxErrorBodyBytes caps how much of a non-2xx response body is read into the
	// returned error, bounding memory and log size.
	maxErrorBodyBytes = 4 << 10 // 4 KiB
	// maxMetaBytes caps JWKS/OpenID/token JSON responses decoded from Microsoft.
	maxMetaBytes = 256 << 10 // 256 KiB

	// maxConversations bounds the conversation->serviceUrl map so a public
	// endpoint with many unique conversations cannot grow it without limit.
	maxConversations = 10000
	// jwksMinRefreshInterval rate-limits JWKS re-fetches so a flood of tokens
	// carrying unknown kids cannot turn into a fetch-per-request DoS.
	jwksMinRefreshInterval = time.Minute
	// tokenRefreshSkew refreshes the outbound token a little before it expires.
	tokenRefreshSkew = time.Minute
)

// allowedServiceHosts / allowedServiceHostSuffixes are the only hosts replies
// may be POSTed to. The Activity's serviceUrl is attacker-influenced JSON, so
// even a valid token must not point the bot's outbound requests at an arbitrary
// host. Bot Connector serviceUrls live under smba.trafficmanager.net (regional
// paths) and *.botframework.com; the broad *.trafficmanager.net namespace is
// shared by every Azure tenant and is deliberately NOT allowlisted.
var (
	allowedServiceHosts        = map[string]bool{"botframework.com": true, "smba.trafficmanager.net": true}
	allowedServiceHostSuffixes = []string{".botframework.com"}
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("teams: missing required config field")

// errUnauthorized marks an inbound request that fails JWT validation.
var errUnauthorized = errors.New("teams: unauthorized inbound request")

// Config configures a Microsoft Teams (Azure Bot Framework) bot.
type Config struct {
	// AppID is the Microsoft App (client) ID of the Azure Bot resource. It is
	// the expected audience of inbound tokens and the client_id used to mint
	// outbound tokens.
	AppID string
	// AppPassword is the client secret paired with AppID.
	AppPassword string
	// TenantID scopes the outbound token endpoint to a single tenant. Optional:
	// when empty the multi-tenant ("botframework.com") endpoint is used.
	TenantID string
	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080".
	Addr string
	// Path is the webhook route; it defaults to /api/messages.
	Path       string
	HTTPClient *http.Client
}

// Message is the parsed payload of an inbound Bot Framework Activity. Raw holds
// the original Activity JSON for callers that need fields beyond these.
type Message struct {
	ID             string
	ConversationID string
	ServiceURL     string
	From           string
	AuthorName     string
	Text           string
	Timestamp      time.Time
	Raw            json.RawMessage
}

type cachedToken struct {
	value  string
	expiry time.Time
}

type adapter struct {
	cfg  Config
	http *http.Client

	// tokenURL and openIDURL are the Microsoft endpoints, fields so tests can
	// point them at an httptest server.
	tokenURL  string
	openIDURL string

	mu        sync.Mutex
	srv       *http.Server
	inflight  atomic.Int64
	convs     map[string]string // conversationID -> serviceUrl
	convOrder []string          // FIFO insertion order for bounded eviction
	token     cachedToken
	keys      map[string]*rsa.PublicKey // kid -> signing key
	keysAt    time.Time

	// fetchMu serializes JWKS refreshes so a burst of tokens carrying an unknown
	// kid triggers a single upstream fetch rather than one per request.
	fetchMu sync.Mutex
}

// New creates a Microsoft Teams bot backed by the Azure Bot Framework. It
// returns ErrMissingConfig if a required credential is absent, and otherwise
// applies defaults for Path, HTTPClient and the Microsoft endpoints. The webhook
// server is not started until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.TeamsBotType, a), nil
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.AppID == "" || cfg.AppPassword == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: AppID, AppPassword and Addr are required", ErrMissingConfig)
	}
	// A bare port ("8080") is shorthand for ":8080"; a host, host:port, :port or
	// IPv6 literal is left for net.Listen to validate.
	if _, err := strconv.Atoi(cfg.Addr); err == nil {
		cfg.Addr = ":" + cfg.Addr
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	// A pattern without a leading slash panics ServeMux at Connect; normalize one in.
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	tenant := cfg.TenantID
	if tenant == "" {
		tenant = "botframework.com"
	}
	return &adapter{
		cfg:       cfg,
		http:      cfg.HTTPClient,
		tokenURL:  "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token",
		openIDURL: openIDConfigURL,
	}, nil
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.handleMessages(ctx, w, r, deps)
	})

	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	a.mu.Lock()
	a.srv = srv
	a.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Done(err)
		}
	}()

	// Tear down when the run context is canceled.
	go func() {
		<-ctx.Done()
		a.mu.Lock()
		current := a.srv == srv
		a.mu.Unlock()
		if current {
			_ = deps.Disconnect()
		}
	}()

	return nil
}

func (a *adapter) handleMessages(ctx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var act inboundActivity
	if err := json.Unmarshal(body, &act); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Authenticate before trusting anything in the body: validate the Bot
	// Connector JWT and confirm its serviceurl claim matches the Activity's.
	if err := a.validateInbound(ctx, r.Header.Get("Authorization"), act.ServiceURL); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Even with a valid token, only POST replies to Bot Framework hosts (SSRF).
	if !isAllowedServiceHost(act.ServiceURL) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	w.WriteHeader(http.StatusOK)

	// Only user message Activities are dispatched; skip conversationUpdate,
	// typing, etc., and drop the bot's own messages to avoid reply loops.
	if act.Type != "message" {
		return
	}
	if act.From.ID != "" && act.From.ID == act.Recipient.ID {
		return
	}

	a.recordConversation(act.Conversation.ID, act.ServiceURL)

	msg := toMessage(&act, body)
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		deps.Dispatch(ctx, msg)
	}()
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	// Shutdown and drain each get their own budget: dispatch goroutines run
	// outside the HTTP handler lifecycle, so a slow Shutdown must not consume the
	// drain deadline and silently drop an already-acked in-flight message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	a.drainDispatch(drainCtx)
	return err
}

// drainDispatch waits for in-flight dispatch goroutines to finish so an acked
// message is processed rather than dropped at shutdown, bounded by ctx. It polls
// an atomic counter rather than a WaitGroup: the dispatch goroutines are started
// from request handlers that Shutdown may abandon at its deadline, and a
// WaitGroup Add racing that Wait would risk a misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Send posts a text reply to the conversation identified by channelID. The
// per-conversation serviceUrl is resolved from the map populated on inbound
// Activities, so Send fails if no Activity has been seen for channelID yet.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	a.mu.Lock()
	serviceURL := a.convs[channelID]
	a.mu.Unlock()
	if serviceURL == "" {
		return fmt.Errorf("teams: no serviceUrl known for conversation %q (no inbound activity seen)", channelID)
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}

	payload := map[string]any{"type": "message", "text": text}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	endpoint := strings.TrimRight(serviceURL, "/") + "/v3/conversations/" + url.PathEscape(channelID) + "/activities"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("teams: send failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Drain the success body so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Attachments returns the files attached to m, mapped from the Activity's
// attachments. Teams contentUrls are generally fetchable as-is, so this adapter
// implements no AttachmentResolver and rides the passthrough in
// Bot.ResolveAttachmentURL.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	tm, ok := RawMessage(m)
	if !ok || tm == nil {
		return nil, nil
	}
	var act inboundActivity
	if err := json.Unmarshal(tm.Raw, &act); err != nil {
		return nil, err
	}
	if len(act.Attachments) == 0 {
		return []core.Attachment{}, nil
	}
	out := make([]core.Attachment, 0, len(act.Attachments))
	for _, at := range act.Attachments {
		out = append(out, core.Attachment{
			IsImage: strings.HasPrefix(at.ContentType, "image/"),
			URL:     at.ContentURL,
		})
	}
	return out, nil
}

// recordConversation maps a conversation to its serviceUrl, bounding the map
// with FIFO eviction so a public endpoint cannot grow it without limit. Eviction
// is by first-seen, not last-active: replies happen inside the same dispatch as
// the recording, so request/response bots always resolve. A bot that stores a
// conversation to message it proactively much later (out of scope here) could
// find an oldest conversation evicted once maxConversations distinct
// conversations have been seen; Send then returns a clear error.
func (a *adapter) recordConversation(id, serviceURL string) {
	if id == "" || serviceURL == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.convs == nil {
		a.convs = make(map[string]string)
	}
	if _, exists := a.convs[id]; !exists {
		if len(a.convOrder) >= maxConversations {
			oldest := a.convOrder[0]
			a.convOrder = a.convOrder[1:]
			delete(a.convs, oldest)
		}
		a.convOrder = append(a.convOrder, id)
	}
	a.convs[id] = serviceURL
}

// accessToken returns a cached Bot Connector token, minting a fresh one via the
// client-credentials grant when the cache is empty or near expiry. The network
// call is made outside the lock.
func (a *adapter) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token.value != "" && time.Until(a.token.expiry) > tokenRefreshSkew {
		v := a.token.value
		a.mu.Unlock()
		return v, nil
	}
	a.mu.Unlock()

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.AppID},
		"client_secret": {a.cfg.AppPassword},
		"scope":         {tokenScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return "", fmt.Errorf("teams: token request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("teams: decode token response: %w", err)
	}
	// Drain any trailing bytes so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	if out.AccessToken == "" {
		return "", errors.New("teams: token response missing access_token")
	}
	// Azure AD always returns expires_in; floor a missing/zero value so a bad
	// response can't collapse the cache into minting a token on every Send.
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 300
	}

	a.mu.Lock()
	a.token = cachedToken{value: out.AccessToken, expiry: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}
	a.mu.Unlock()
	return out.AccessToken, nil
}

// validateInbound verifies the Bearer JWT on an inbound request: RS256 signature
// against the Bot Connector JWKS, audience == AppID, the expected issuer, exp,
// and a serviceurl claim equal to the Activity's serviceUrl.
func (a *adapter) validateInbound(ctx context.Context, authHeader, activityServiceURL string) error {
	raw, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || raw == "" {
		return errUnauthorized
	}

	claims := jwt.MapClaims{}
	keyFunc := func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("teams: unexpected signing method %q", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		return a.publicKey(ctx, kid)
	}
	tok, err := jwt.ParseWithClaims(raw, claims, keyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(a.cfg.AppID),
		jwt.WithIssuer(botConnectorIssuer),
		jwt.WithExpirationRequired(),
		// Bot Connector documents an industry-standard 5-minute clock-skew
		// tolerance; the reference SDKs apply the same leeway.
		jwt.WithLeeway(5*time.Minute),
	)
	if err != nil || !tok.Valid {
		return errUnauthorized
	}

	// The claim name is lowercase "serviceurl"; MapClaims lookup is case-sensitive.
	svc, _ := claims["serviceurl"].(string)
	if svc == "" || !sameServiceURL(svc, activityServiceURL) {
		return errUnauthorized
	}
	return nil
}

// publicKey returns the signing key for kid, refreshing the JWKS once on a miss
// (to handle key rotation) but no more often than jwksMinRefreshInterval.
func (a *adapter) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	k := a.keys[kid]
	stale := time.Since(a.keysAt) >= jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("teams: unknown signing key %q", kid)
	}

	// Serialize refreshes so a burst of unknown-kid tokens makes a single upstream
	// fetch. After winning fetchMu, re-check the cache: a concurrent refresh may
	// have already resolved this kid or reset the staleness window.
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()

	a.mu.Lock()
	k = a.keys[kid]
	stale = time.Since(a.keysAt) >= jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("teams: unknown signing key %q", kid)
	}

	// Advance keysAt before fetching, not just on success, so a failed fetch is
	// still rate-limited to once per jwksMinRefreshInterval. Otherwise, during a
	// JWKS-endpoint outage every unknown-kid request would re-trigger a fetch.
	a.mu.Lock()
	a.keysAt = time.Now()
	a.mu.Unlock()

	keys, err := a.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.keys = keys
	k = a.keys[kid]
	a.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("teams: unknown signing key %q after refresh", kid)
	}
	return k, nil
}

// fetchJWKS resolves the Bot Connector OpenID metadata to its jwks_uri and
// decodes the RSA signing keys into a kid-indexed map.
func (a *adapter) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, a.openIDURL, &meta); err != nil {
		return nil, err
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("teams: openid metadata missing jwks_uri")
	}
	// Pin jwks_uri to the same host as the (hardcoded, TLS) OpenID metadata
	// endpoint so a tampered/redirected document cannot point key fetching at an
	// arbitrary host. Microsoft serves both under the same host.
	if ou, err := url.Parse(a.openIDURL); err != nil || !sameHost(ou, meta.JWKSURI) {
		return nil, errors.New("teams: jwks_uri host does not match the OpenID metadata host")
	}

	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := a.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return nil, err
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
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
		// Reject implausible keys: a zero/small modulus or a degenerate exponent
		// could only ever fail to verify, but skipping them keeps the map clean.
		if n.BitLen() < 1024 || e < 2 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: n, E: e}
	}
	if len(keys) == 0 {
		return nil, errors.New("teams: no usable RSA keys in JWKS")
	}
	return keys, nil
}

func (a *adapter) getJSON(ctx context.Context, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("teams: GET %s failed with status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(v); err != nil {
		return fmt.Errorf("teams: decode %s: %w", endpoint, err)
	}
	// Drain any trailing bytes so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// RawMessage returns the parsed Teams message carried on m, reporting whether m
// originated from Teams.
func RawMessage(m *core.Message) (*Message, bool) {
	tm, ok := m.Raw.(*Message)
	return tm, ok
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

// sameServiceURL compares two serviceUrls ignoring a trailing slash and case;
// the .NET reference SDK compares the serviceUrl claim case-insensitively.
func sameServiceURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// sameHost reports whether rawURL parses and shares host (case-insensitively)
// with u.
func sameHost(u *url.URL, rawURL string) bool {
	v, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(u.Host, v.Host)
}

type channelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type activityAttachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name"`
}

type inboundActivity struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Text         string         `json:"text"`
	ServiceURL   string         `json:"serviceUrl"`
	Timestamp    string         `json:"timestamp"`
	From         channelAccount `json:"from"`
	Recipient    channelAccount `json:"recipient"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	Attachments []activityAttachment `json:"attachments"`
}

func toMessage(act *inboundActivity, raw json.RawMessage) *core.Message {
	ts := parseTimestamp(act.Timestamp)
	tm := &Message{
		ID:             act.ID,
		ConversationID: act.Conversation.ID,
		ServiceURL:     act.ServiceURL,
		From:           act.From.ID,
		AuthorName:     act.From.Name,
		Text:           act.Text,
		Timestamp:      ts,
		Raw:            raw,
	}
	return &core.Message{
		ID:         act.ID,
		UserID:     act.From.ID,
		AuthorName: act.From.Name,
		ChannelID:  act.Conversation.ID,
		Content:    act.Text,
		Timestamp:  ts,
		Raw:        tm,
	}
}

func parseTimestamp(s string) time.Time {
	// RFC3339Nano is a superset of RFC3339 (fractional seconds optional), so it
	// also parses plain RFC3339 timestamps.
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
