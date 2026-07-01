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
// (signature, audience == AppID, issuer, a serviceurl claim matching the Activity,
// and a signing key endorsed for the Activity's channelId), and replies go only to
// allowlisted Bot Framework hosts. The endorsement check authenticates the channel,
// not the from account, so operators must not enable untrusted channels on the same
// bot resource. The Activity body itself is channel-trusted, not individually
// signed, with no replay tracking: terminate TLS at a trusted proxy, never expose
// the bind Addr in cleartext, and rate-limit at the proxy.
//
// Implementation is split across this package: server.go (webhook lifecycle),
// auth.go (JWT/JWKS + SSRF checks), send.go (reply + token), message.go (parsing),
// attachments.go (attachment mapping) and http.go (HTTP plumbing).
package teams

import (
	"context"
	"errors"
	"fmt"
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
	Path       string
	HTTPClient *http.Client
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
	// detachedCancel aborts the current connection's dispatch goroutines. Connect
	// derives one cancelable context per connection (WithCancel over
	// WithoutCancel(runCtx)) and passes it through the handler closure, so only the
	// cancel is stored here, not a shared context field. Disconnect calls it after
	// the drain window so a stuck handler or reply cannot leak.
	detachedCancel context.CancelFunc
	inflight       atomic.Int64
	convs          map[string]conversation // conversationID -> reply routing info
	convOrder      []string                // FIFO insertion order for bounded eviction
	token          cachedToken
	keys           map[string]*jwksKey // kid -> signing key + channel endorsements
	// keysAt is the last JWKS fetch attempt (success or failure); it rate-limits
	// refreshes to one per jwksMinRefreshInterval. keysFreshAt is the last
	// successful refresh; it drives the jwksMaxAge staleness gate so a retired key
	// cannot stay trusted indefinitely.
	keysAt      time.Time
	keysFreshAt time.Time

	// fetchMu serializes JWKS refreshes so a burst of unknown-kid tokens triggers a
	// single upstream fetch rather than one per request.
	fetchMu sync.Mutex
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
