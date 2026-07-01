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
	"log"
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
	// The Bot Connector JWKS at login.botframework.com is large — ~1 MB with a few
	// hundred keys — so this must comfortably exceed it; a tighter cap truncates
	// the body and the JSON decode fails with "unexpected EOF". These are trusted
	// HTTPS Microsoft endpoints fetched rarely (cached + rate-limited), so the
	// generous ceiling is purely a memory-exhaustion backstop.
	maxMetaBytes = 4 << 20 // 4 MiB

	// maxConversations bounds the conversation->serviceUrl map so a public
	// endpoint with many unique conversations cannot grow it without limit.
	maxConversations = 10000
	// jwksMinRefreshInterval rate-limits JWKS re-fetches so a flood of tokens
	// carrying unknown kids cannot turn into a fetch-per-request DoS.
	jwksMinRefreshInterval = time.Minute
	// jwksMaxAge bounds how long a cached signing-key set is trusted before a
	// refresh is forced even for a known kid. Without it a key Microsoft has
	// retired would stay accepted until an unseen-kid token happened to flush the
	// map; the forced refresh replaces the whole set, dropping retired keys. The
	// reference Bot Framework SDK uses the same fixed 24h refresh interval.
	jwksMaxAge = 24 * time.Hour
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

	// attachments is the Activity's attachments, decoded once at parse time so
	// Attachments need not re-unmarshal the (up to maxRequestBytes) Raw body on
	// every lookup. Unexported: callers reach attachment bytes via
	// Bot.Attachments or the full Raw JSON, so this stays an internal detail.
	attachments []activityAttachment
}

type cachedToken struct {
	value  string
	expiry time.Time
}

// conversation holds what a reply needs: the serviceUrl to POST to and the bot's
// own account (the inbound Activity's recipient), used as the reply's required
// from field. The reply's recipient is deliberately not stored: it is a
// per-activity value, but replies are delivered by conversation id, so a
// per-conversation recipient is a last-writer-wins race between concurrent senders
// in a shared group/channel — and the Bot Connector treats recipient as optional
// on a reply. The bot account is conversation-stable (always this bot), so it is
// safe to cache.
type conversation struct {
	serviceURL string
	bot        channelAccount
}

type adapter struct {
	cfg  Config
	http *http.Client

	// tokenURL and openIDURL are the Microsoft endpoints, fields so tests can
	// point them at an httptest server.
	tokenURL  string
	openIDURL string

	mu  sync.Mutex
	srv *http.Server
	// detachedCancel aborts the current connection's dispatch goroutines. Each
	// Connect derives one detached, cancelable context — context.WithCancel over
	// context.WithoutCancel(runCtx) — and threads the context itself through the
	// handler closure (like runCtx), so there is no shared context field to race on;
	// only the cancel is stored here. Disconnect calls it after the drain window so a
	// handler blocked on ctx.Done() or a reply on a no-timeout client cannot leak, and
	// clears it only when a reconnect has not already installed a newer connection.
	detachedCancel context.CancelFunc
	inflight       atomic.Int64
	convs          map[string]conversation // conversationID -> reply routing info
	convOrder      []string                // FIFO insertion order for bounded eviction
	token          cachedToken
	keys           map[string]*rsa.PublicKey // kid -> signing key
	// keysAt is the time of the last JWKS fetch *attempt* (success or failure); it
	// rate-limits refreshes to at most one per jwksMinRefreshInterval, bounding the
	// fetch rate even during an upstream outage. keysFreshAt is the time of the
	// last *successful* refresh; it drives the jwksMaxAge staleness gate so a
	// retired key cannot stay trusted indefinitely between unknown-kid misses.
	keysAt      time.Time
	keysFreshAt time.Time

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
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel drops runCtx's deadline/cancellation so an acked reply can finish
	// during the shutdown drain, WithCancel lets Disconnect abort stragglers after it.
	// The handler closure captures the context directly (like runCtx), so there is no
	// shared context field for a request and Disconnect to race on.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.handleMessages(ctx, detachedCtx, w, r, deps)
	})

	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		detachedCancel() // nothing will consume the context; release it.
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
	a.detachedCancel = detachedCancel
	a.mu.Unlock()

	go serve(srv, ln, deps.Done)

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

func serve(srv *http.Server, ln net.Listener, done func(error)) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		done(err)
	}
}

func (a *adapter) handleMessages(ctx, dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
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
		log.Printf("teams: inbound request rejected with 401: %v", err)
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
	// typing, etc., and drop bot-authored messages to avoid reply loops. Teams
	// marks the sender's account with from.role == "bot" for the bot's own echo
	// and for any other bot in a shared channel; that role is the reliable signal
	// (from.id never equals recipient.id, since recipient is always this bot).
	if act.Type != "message" {
		return
	}
	if strings.EqualFold(act.From.Role, "bot") {
		return
	}

	a.recordConversation(act.Conversation.ID, act.ServiceURL, act.Recipient)

	msg := toMessage(&act, body)
	// Dispatch on the per-connection detached context (captured in Connect and passed
	// in): core cancels runCtx *before* calling Disconnect, whose drainDispatch waits
	// for this in-flight handler, so the reply must not be threaded onto runCtx or it
	// would fail with "context canceled" mid-drain. dispatchCtx keeps runCtx's values
	// but drops its deadline/cancellation so an acked message finishes within the drain
	// window; Disconnect cancels it after the drain so a stuck reply is bounded
	// (otherwise only by the outbound HTTP client's timeout — 30s for the default
	// client; set one on a custom cfg.HTTPClient). Add before starting the goroutine so
	// the count is visible to drainDispatch: srv.Shutdown waits for this handler to
	// return, so the increment lands before the drain observes the counter.
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		deps.Dispatch(dispatchCtx, msg)
	}()
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	cancelDispatch := a.detachedCancel
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

	// If the drain timed out, dispatch goroutines are still running and the
	// cancelDispatch below force-aborts them; surface that instead of aborting
	// silently, since a forced abort of an already-acked message is operationally
	// significant.
	if n := a.inflight.Load(); n > 0 {
		log.Printf("teams: drain deadline reached; canceling %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not already installed a newer
	// connection (identity-compare on srv): core clears its own cancel before this
	// up-to-10s Disconnect returns, so a fresh Connect can legitimately run
	// concurrently, and nil-ing unconditionally could clobber its live cancel. Either
	// way, cancel THIS connection's own detached context after the drain so a handler
	// blocked on ctx.Done() (or a reply on a no-timeout client) cannot leak past
	// shutdown. CancelFunc is idempotent and goroutine-safe.
	a.mu.Lock()
	if a.srv == srv {
		a.srv = nil
		a.detachedCancel = nil
	}
	a.mu.Unlock()

	if cancelDispatch != nil {
		cancelDispatch()
	}
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
// serviceUrl and the bot's own account are resolved from the map populated on
// inbound Activities, so Send fails if no Activity has been seen for channelID
// yet.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	a.mu.Lock()
	conv, ok := a.convs[channelID]
	a.mu.Unlock()
	if !ok || conv.serviceURL == "" {
		return fmt.Errorf("teams: no serviceUrl known for conversation %q (no inbound activity seen)", channelID)
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}

	// Bot Connector requires the reply's from field (the bot's account), captured
	// from the inbound Activity's recipient when the conversation was recorded. The
	// reply's recipient is left unset: delivery is keyed by the conversation id in
	// the URL, and recipient is per-activity, so caching one would misattribute a
	// concurrent sender's reply.
	payload := outboundActivity{
		Type: "message",
		Text: text,
		From: conv.bot,
	}
	// channelAccount holds only strings, which encoding/json cannot reject.
	body, _ := json.Marshal(payload)

	endpoint := strings.TrimRight(conv.serviceURL, "/") + "/v3/conversations/" + url.PathEscape(channelID) + "/activities"
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

	// A reply has no response body to decode; decodeJSON just validates the status
	// and drains for keep-alive.
	if err := decodeJSON(resp, nil); err != nil {
		return fmt.Errorf("teams: send failed: %w", err)
	}
	return nil
}

// Attachments returns the files attached to m, mapped from the Activity's
// attachments decoded at parse time. Teams contentUrls are generally fetchable
// as-is, so this adapter implements no AttachmentResolver and rides the
// passthrough in Bot.ResolveAttachmentURL. The error return is kept for the
// core.Adapter contract but is always nil: the Activity is parsed once at
// ingress, so there is nothing left here to fail.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	tm, ok := RawMessage(m)
	if !ok || tm == nil {
		return nil, nil
	}
	if len(tm.attachments) == 0 {
		return []core.Attachment{}, nil
	}
	out := make([]core.Attachment, 0, len(tm.attachments))
	for _, at := range tm.attachments {
		out = append(out, core.Attachment{
			IsImage: strings.HasPrefix(at.ContentType, "image/"),
			URL:     at.ContentURL,
		})
	}
	return out, nil
}

// recordConversation maps a conversation to its serviceUrl and the bot's own
// account (the reply's required from field), bounding the map with FIFO eviction
// so a public endpoint cannot grow it without limit. Eviction is by first-seen,
// not last-active: replies happen inside the same dispatch as the recording, so
// request/response bots always resolve. A bot that stores a conversation to
// message it proactively much later (out of scope here) could find an oldest
// conversation evicted once maxConversations distinct conversations have been
// seen; Send then returns a clear error.
func (a *adapter) recordConversation(id, serviceURL string, bot channelAccount) {
	if id == "" || serviceURL == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.convs == nil {
		a.convs = make(map[string]conversation)
	}
	if _, exists := a.convs[id]; !exists {
		if len(a.convOrder) >= maxConversations {
			oldest := a.convOrder[0]
			a.convOrder = a.convOrder[1:]
			delete(a.convs, oldest)
		}
		a.convOrder = append(a.convOrder, id)
	}
	a.convs[id] = conversation{serviceURL: serviceURL, bot: bot}
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

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return "", fmt.Errorf("teams: token request: %w", err)
	}
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
	raw, ok := bearerToken(authHeader)
	if !ok {
		return fmt.Errorf("%w: missing or malformed Authorization header", errUnauthorized)
	}

	claims := jwt.MapClaims{}
	keyFunc := func(t *jwt.Token) (any, error) {
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
	if err != nil {
		// golang-jwt/v5 returns specific sentinels (ErrTokenInvalidAudience,
		// ErrTokenInvalidIssuer, ErrTokenExpired, ErrTokenSignatureInvalid, ...),
		// so wrapping err names the exact failing check. iss/aud are echoed from
		// the (now-rejected) token to compare against the configured AppID.
		gotIss, _ := claims["iss"].(string)
		gotAud := claims["aud"]
		// %q (not %v) on gotAud: the rejected token's claims are attacker-controlled
		// and this error is logged, so an unescaped aud could inject newlines/escapes
		// into the log. %q escapes control bytes whether aud decodes to a string or
		// an array.
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
	return nil
}

// publicKey returns the signing key for kid. A cached key is served directly
// while the key set is within jwksMaxAge; once it ages past that a refresh is
// forced even on a hit, so a key Microsoft has retired stops being trusted
// rather than lingering until an unknown-kid token flushes it. An unknown kid
// also triggers a refresh (to pick up key rotation). Refreshes happen at most
// once per jwksMinRefreshInterval, and a refresh that fails falls back to the
// cached key so a transient JWKS outage does not reject otherwise-valid tokens.
func (a *adapter) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	k := a.keys[kid]
	fresh := time.Since(a.keysFreshAt) < jwksMaxAge
	rateLimited := time.Since(a.keysAt) < jwksMinRefreshInterval
	a.mu.Unlock()
	if k != nil && fresh {
		return k, nil
	}
	if rateLimited {
		// Within the rate-limit window we cannot re-fetch: serve the cached key if
		// present (fallback while a refresh is pending), else report the unknown kid.
		if k != nil {
			return k, nil
		}
		return nil, fmt.Errorf("teams: unknown signing key %q", kid)
	}

	// Serialize refreshes so a burst of unknown-kid or simultaneously-aged tokens
	// makes a single upstream fetch. After winning fetchMu, re-check the cache: a
	// concurrent refresh may have already resolved this kid or reset the windows.
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

	// Advance keysAt (the attempt clock) before fetching, not just on success, so
	// a failed fetch is still rate-limited to once per jwksMinRefreshInterval.
	// Otherwise, during a JWKS-endpoint outage every request would re-trigger a
	// fetch. keysFreshAt (the max-age clock) advances only on success below.
	a.mu.Lock()
	a.keysAt = time.Now()
	a.mu.Unlock()

	keys, err := a.fetchJWKS(ctx)
	if err != nil {
		// Fall back to the cached key (if any) so a transient JWKS outage does not
		// reject otherwise-valid tokens; a genuine unknown-kid miss surfaces err.
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
	// Pin jwks_uri to the same scheme AND host as the (hardcoded, TLS) OpenID
	// metadata endpoint so a tampered/redirected document cannot point key fetching
	// at an arbitrary host or downgrade it to cleartext. The host match ignores an
	// explicit port (via url.Hostname), matching isAllowedServiceHost, so a default
	// port Microsoft might include on jwks_uri (e.g. login.botframework.com:443)
	// does not spuriously reject a legitimate document. Matching the metadata scheme
	// (https in production) rather than hardcoding "https" keeps the httptest-based
	// tests, which serve both over http, exercising this path.
	if !sameSchemeHost(a.openIDURL, meta.JWKSURI) {
		return nil, errors.New("teams: jwks_uri scheme/host does not match the OpenID metadata endpoint")
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
	if err := decodeJSON(resp, v); err != nil {
		return fmt.Errorf("teams: GET %s: %w", endpoint, err)
	}
	return nil
}

// decodeJSON is the shared tail for the adapter's outbound HTTP calls. It treats
// a non-2xx status as an error (naming the status and a capped snippet of the
// body), decodes the 2xx body as JSON into v when v is non-nil (reading at most
// maxMetaBytes), then drains any trailing bytes so the connection can be reused
// (keep-alive). Pass a nil v for calls with no response body to decode (e.g. a
// reply POST) — the status check and keep-alive drain still run. Callers wrap the
// returned error with call-site context. Keeping this in one place means a later
// tweak to the non-2xx handling or the keep-alive drain applies to Send,
// accessToken and getJSON alike.
func decodeJSON(resp *http.Response, v any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if v != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(v); err != nil {
			return fmt.Errorf("decode body: %w", err)
		}
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

// bearerToken extracts the credential from an Authorization header. Per RFC 7235
// the auth-scheme is matched case-insensitively, and RFC 6750 allows one or more
// spaces between "Bearer" and the token, so this accepts e.g. "bearer <t>" and
// collapses extra surrounding/separating whitespace rather than demanding an
// exact "Bearer " prefix. It returns the token and whether the header is a
// well-formed bearer credential; the token itself is still fully validated by the
// JWT parser, so lenient scheme parsing does not weaken auth.
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
// case-insensitively and ignoring any explicit port. It pins a fetched jwks_uri
// to the origin of the hardcoded OpenID metadata endpoint. url.Hostname strips
// the port (unlike url.Host, which is "host or host:port"), so an explicit
// default port on either side does not reject an otherwise-matching host — the
// same port-insensitive stance isAllowedServiceHost takes.
func sameSchemeHost(a, b string) bool {
	au, aErr := url.Parse(a)
	bu, bErr := url.Parse(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return strings.EqualFold(au.Scheme, bu.Scheme) && strings.EqualFold(au.Hostname(), bu.Hostname())
}

type channelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role is the Bot Framework RoleType of the account: "user" or "bot". Teams
	// sets from.role to "bot" on messages authored by a bot (the bot's own echo,
	// or another bot in a shared channel), which is how those are dropped.
	Role string `json:"role"`
}

type activityAttachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name"`
}

// mentionEntity is a Bot Framework "mention" entity from an Activity's entities
// array. Text is the literal markup span in the message text (e.g.
// "<at>Bot Name</at>") and Mentioned identifies who was mentioned.
type mentionEntity struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Mentioned channelAccount `json:"mentioned"`
}

// outboundActivity is the reply posted to the Bot Connector. from is required by
// the service. recipient is deliberately omitted: it is per-activity while replies
// are delivered by conversation id, so a cached recipient would misattribute a
// concurrent sender's reply, and the service treats it as optional on a reply.
type outboundActivity struct {
	Type string         `json:"type"`
	Text string         `json:"text"`
	From channelAccount `json:"from"`
}

type inboundActivity struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Text         string         `json:"text"`
	ServiceURL   string         `json:"serviceUrl"`
	Timestamp    string         `json:"timestamp"`
	ReplyToID    string         `json:"replyToId"`
	From         channelAccount `json:"from"`
	Recipient    channelAccount `json:"recipient"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	Attachments []activityAttachment `json:"attachments"`
	Entities    []mentionEntity      `json:"entities"`
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
		attachments:    act.Attachments,
	}
	return &core.Message{
		ID:         act.ID,
		UserID:     act.From.ID,
		AuthorName: act.From.Name,
		ChannelID:  act.Conversation.ID,
		Content:    stripRecipientMention(act),
		Timestamp:  ts,
		ReplyToID:  act.ReplyToID,
		Raw:        tm,
	}
}

// stripRecipientMention removes the bot's own @mention markup from the Activity
// text. In Teams group and channel chats the inbound text is prefixed with markup
// like "<at>Bot Name</at> echo hi", which would stop an anchored command pattern
// (e.g. ^echo) from ever matching. Only the mention entity targeting Recipient.ID
// (the bot) is removed — by deleting its literal Text span, mirroring the Bot
// Framework SDK's removeRecipientMention — so mentions of other users survive for
// handlers that want them. The internal Message.Text keeps the raw wire text.
//
// The result is trimmed only when a mention was actually removed, so a plain 1:1
// message (no entities) is passed through byte-for-byte.
func stripRecipientMention(act *inboundActivity) string {
	if act.Recipient.ID == "" {
		return act.Text
	}
	text := act.Text
	stripped := false
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Text != "" && e.Mentioned.ID == act.Recipient.ID {
			text = strings.Replace(text, e.Text, "", 1)
			stripped = true
		}
	}
	if !stripped {
		return act.Text
	}
	return strings.TrimSpace(text)
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
