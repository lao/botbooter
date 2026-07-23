// Package teams is the Microsoft Teams adapter for botbooter. It implements
// core.Adapter, receiving messages from the Azure Bot Framework over an inbound
// webhook and replying through the Bot Connector REST API.
//
// Like the WhatsApp adapter, the Bot Framework pushes inbound messages as HTTP
// callbacks, so this adapter runs its own server: Connect binds a listener and
// serves until the run context is canceled; Disconnect shuts it down. Bind a local
// Addr, put a TLS-terminating proxy in front, and register the public HTTPS URL as
// your Azure Bot resource's messaging endpoint.
//
// Security: every inbound request is authenticated against the Bot Connector JWT
// (signature, audience, issuer, serviceurl claim, channel endorsement), and
// replies go only to allowlisted Bot Framework hosts. The Activity body itself is
// channel-trusted, not individually signed, with no replay tracking: terminate
// TLS at a trusted proxy, never expose the bind Addr in cleartext, and rate-limit
// at the proxy.
//
// Implementation split: server.go (webhook lifecycle), auth.go (JWT/JWKS + SSRF
// checks), send.go (reply + token), message.go (parsing), attachments.go
// (attachment mapping), http.go (HTTP plumbing).
package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lao/botbooter/internal/core"
)

const defaultPath = "/api/messages"

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("teams: missing required config field")

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
	Path string
	// HTTPClient overrides the client used for outbound Bot Connector calls
	// (token minting, replies, and the JWKS/OpenID metadata fetches); a default
	// client with a 30s timeout is used when nil.
	HTTPClient *http.Client
}

type adapter struct {
	cfg  Config
	http *http.Client

	// tokenURL and openIDURL are fields so tests can point them at an httptest server.
	tokenURL  string
	openIDURL string

	mu     sync.Mutex
	logger *slog.Logger // set from AdapterDeps at Connect; guarded by mu
	srv    *http.Server
	// boundAddr is the listener's resolved address, so a cfg.Addr of ":0" is
	// recoverable via Addr. Set with srv, cleared with it.
	boundAddr string
	// detachedCancel aborts the current connection's dispatch goroutines. Connect
	// derives one cancelable detached context per connection and passes it through
	// the handler closure, so only the cancel is shared state. Disconnect calls it
	// after the drain window so a stuck handler or reply cannot leak.
	detachedCancel context.CancelFunc
	inflight       atomic.Int64
	// dispatchSem is a counting semaphore (capacity maxConcurrentDispatch) that
	// bounds in-flight dispatch goroutines: the handler acquires a slot before
	// acking and sheds load with 503 when it is full. Allocated once in newAdapter
	// so the bound is adapter-wide across reconnects.
	dispatchSem chan struct{}
	convs       map[string]conversation // conversationID -> reply routing info
	convOrder   []string                // FIFO insertion order for bounded eviction
	token       cachedToken
	keys        map[string]*jwksKey // kid -> signing key + channel endorsements
	// keysAt is the last JWKS fetch attempt (rate-limits refreshes); keysFreshAt
	// is the last successful refresh (drives the jwksMaxAge staleness gate).
	keysAt      time.Time
	keysFreshAt time.Time

	// fetchMu serializes JWKS refreshes so a burst of unknown-kid tokens triggers
	// a single upstream fetch.
	fetchMu sync.Mutex
	// tokenMu serializes cold outbound-token mints so a burst of concurrent Sends
	// makes a single client-credentials request, mirroring fetchMu.
	tokenMu sync.Mutex
}

// New creates a Microsoft Teams bot backed by the Azure Bot Framework. It returns
// ErrMissingConfig if a required credential is absent, and otherwise applies
// defaults for Path, HTTPClient and the Microsoft endpoints. The webhook server is
// not started until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.TeamsBotType, a), nil
}

// Addr returns the address the bot's webhook listener is bound to (host:port),
// or "" if b is not a Teams bot or is not currently connected. It lets a caller
// that passed cfg.Addr ":0" discover the OS-assigned port.
func Addr(b *core.Bot) string {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.boundAddr
	}
	return ""
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.AppID == "" || cfg.AppPassword == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: AppID, AppPassword and Addr are required", ErrMissingConfig)
	}
	// A bare port ("8080") is shorthand for ":8080".
	if _, err := strconv.Atoi(cfg.Addr); err == nil {
		cfg.Addr = ":" + cfg.Addr
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	// A pattern without a leading slash panics ServeMux at Connect.
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
		cfg:         cfg,
		http:        cfg.HTTPClient,
		tokenURL:    "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token",
		openIDURL:   openIDConfigURL,
		dispatchSem: make(chan struct{}, maxConcurrentDispatch),
	}, nil
}
