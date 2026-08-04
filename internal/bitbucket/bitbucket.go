// Package bitbucket is the Bitbucket adapter for botbooter. It receives pull
// request and issue comments as webhook deliveries over an inbound HTTP server
// and replies by creating comments through the Bitbucket REST API. It implements
// core.Adapter and, since Bitbucket threads a reply via a comment's parent id on
// both flavors, core.ThreadedSender. Optional Config callbacks (OnPullRequest,
// OnPush) additionally route PR and push deliveries on the same endpoint; unset,
// those deliveries are acked and dropped.
//
// One package serves both flavors, selected by Config.BaseURL: empty targets
// Bitbucket Cloud (REST 2.0 via ktrysmt/go-bitbucket), a value targets a Data
// Center instance (REST 1.0 via plain net/http). Cloud and Data Center share the
// webhook framing — X-Event-Key routing and the X-Hub-Signature HMAC — and
// nothing else: different event keys, different payload shapes, different reply
// bodies, different self-identity. That divergence lives behind the unexported
// flavor interface, so server.go never branches on flavor.
//
// Like the WhatsApp, Teams, GitHub and GitLab adapters, Connect binds a listener
// and serves until the run context is canceled; Disconnect shuts it down and
// drains in-flight dispatch. Bind a local Addr, put a TLS-terminating proxy in
// front, and register the public HTTPS URL as the repository webhook with a
// Secret and the comment trigger enabled (plus PR/push triggers when the matching
// callback is set).
//
// Bitbucket removed app passwords platform-wide on 2026-07-28, so the adapter
// authenticates as an Atlassian API token (HTTP Basic email:token) or a
// repository/project/workspace access token (Bearer). Access tokens are not bound
// to a user account, so GET /2.0/user 401s for them and Config.Self is required;
// Data Center has no whoami at all, so Self is required there too.
package bitbucket

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

	gobitbucket "github.com/ktrysmt/go-bitbucket"

	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath = "/webhook"

	// dataCenterAPIPath is the REST 1.0 prefix appended to Config.BaseURL for a
	// Data Center deployment.
	dataCenterAPIPath = "/rest/api/1.0"

	// The endpoint is public and each delivery is buffered whole before parsing,
	// so the body cap bounds attacker-controllable buffering. A comment-only bot
	// keeps the small cap; a bot that registers OnPullRequest/OnPush opts into the
	// larger events (push change sets can be big) and the memory cost. Mirrors the
	// GitLab sibling's split.
	smallRequestBytes = 4 << 20  // 4 MiB (comments and unhandled events)
	largeRequestBytes = 25 << 20 // 25 MiB (large PR / push deliveries)

	// maxConcurrentReads bounds concurrent inbound body read+parse; a delivery
	// past the bound is acked 200 without reading. maxConcurrentDispatch bounds
	// in-flight dispatch goroutines; a delivery past it is acked without
	// dispatching. Separate because a read is short-lived while a dispatch may run
	// a slow handler. See the GitLab sibling for the full rationale.
	maxConcurrentReads    = 16
	maxConcurrentDispatch = 256

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second

	// selfResolveTimeout bounds the Connect-path whoami probe so an
	// unreachable-but-non-rejecting API surfaces promptly instead of leaving the
	// listener bound with Serve never started. Mirrors the GitLab sibling.
	selfResolveTimeout = 15 * time.Second
)

// The adapter implements the mandatory core.Adapter and, because Bitbucket
// threads a reply via a comment's parent id, the optional core.ThreadedSender.
var (
	_ core.Adapter        = (*adapter)(nil)
	_ core.ThreadedSender = (*adapter)(nil)
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("bitbucket: missing required config field")

// ErrAmbiguousAuth is returned by New when neither or both auth modes are
// configured; exactly one of Email+APIToken or AccessToken is required.
var ErrAmbiguousAuth = errors.New("bitbucket: exactly one auth mode required (Email+APIToken or AccessToken)")

// Config configures a Bitbucket bot.
type Config struct {
	// Email and APIToken authenticate as an Atlassian API token (HTTP Basic).
	// Bitbucket removed app passwords on 2026-07-28, so this is the user-bound
	// credential. Both must be set together.
	Email    string
	APIToken string

	// AccessToken authenticates as a repository, project or workspace access token
	// (Bearer). Mutually exclusive with Email/APIToken; exactly one mode is
	// required. These tokens are not bound to a user account, so GET /2.0/user
	// 401s for them and Self becomes required.
	AccessToken string

	// Self is the bot's own actor identity, used to drop its own comments and
	// break reply loops: an account UUID ("{…}") on Cloud, a user slug on Data
	// Center. Optional on Cloud in API-token mode (resolved via GET /2.0/user at
	// Connect); REQUIRED in access-token mode and on Data Center, where no whoami
	// is available.
	Self string

	// Secret is the webhook's configured secret; the adapter verifies the
	// X-Hub-Signature HMAC against it. Required — without it the endpoint would
	// accept spoofed payloads.
	Secret string

	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A bare
	// port ("8080") is accepted as shorthand for ":8080". Required.
	Addr string

	// Path is the webhook route; it defaults to /webhook.
	Path string

	// BaseURL selects the flavor: empty targets Bitbucket Cloud
	// (api.bitbucket.org/2.0); a value like "https://bitbucket.example.com"
	// targets a Data Center instance, whose REST prefix (/rest/api/1.0) is
	// appended automatically.
	BaseURL string

	// OnPullRequest, when set, receives PR deliveries whose action creates or
	// changes reviewable content (Cloud: pullrequest:created/updated; Data Center:
	// pr:opened/pr:from_ref_updated). A delivery whose triggering actor is the bot
	// itself is dropped (the reply-loop guard, by actor identity — the actor is who
	// opened or updated the PR, which is the bot when it acts via the API). The callback
	// runs on a dispatch goroutine covered by Disconnect's drain, so it should hand
	// long work off and return promptly. Nil (the default) acks and drops PR
	// deliveries.
	OnPullRequest func(ctx context.Context, event *PullRequestEvent)

	// OnPush, when set, receives push deliveries unfiltered (Cloud: repo:push;
	// Data Center: repo:refs_changed) — ref filtering is the callback's job. Same
	// goroutine and drain contract as OnPullRequest. Nil (the default) acks and
	// drops.
	OnPush func(ctx context.Context, event *PushEvent)

	// HTTPClient is the base client for outbound Bitbucket API calls; a client
	// with a 30s timeout is used when nil.
	HTTPClient *http.Client
}

// eventCategory is what an X-Event-Key routes to. The flavor computes it (event
// keys differ between Cloud and Data Center), so server.go dispatches on the
// category and never names a flavor-specific key.
type eventCategory int

const (
	catUnknown eventCategory = iota // ping and every unhandled key (incl. comment edits)
	catComment
	catPullRequest
	catPush
)

// flavor is the per-deployment half of the adapter. Cloud and Data Center share
// the webhook framing (X-Event-Key routing, X-Hub-Signature HMAC) and nothing
// else, so one interface keeps that divergence out of the lifecycle code in
// server.go. The parse methods still take the raw eventKey because one category
// can cover several keys (a Cloud comment is a PR comment or an issue comment),
// and because reporting false lets server.go warn on an authentic-but-unparseable
// body without knowing the flavor.
type flavor interface {
	// category classifies an X-Event-Key. An edit (pullrequest:comment_updated,
	// pr:comment:edited) is deliberately catUnknown — edits are ignored.
	category(eventKey string) eventCategory
	// parseComment maps a comment delivery into a core.Message; false on an
	// unparseable body. The message's UserID is the actor identity the reply-loop
	// guard compares against the bot's own self, so the self-drop stays in
	// flavor-independent server.go.
	parseComment(eventKey string, payload []byte) (*core.Message, bool)
	// parsePullRequest maps a reviewable PR delivery, also returning the PR
	// author's identity for the self-drop; false on an unparseable body.
	parsePullRequest(eventKey string, payload []byte) (event *PullRequestEvent, authorID string, ok bool)
	// parsePush maps a push delivery; false on an unparseable body.
	parsePush(eventKey string, payload []byte) (*PushEvent, bool)
	// postComment creates a comment; parentID is 0 for an unthreaded reply.
	postComment(ctx context.Context, target commentTarget, text string, parentID int64) error
	// resolveSelf returns the bot's own actor identity, or "" when the flavor
	// cannot probe for it (Data Center) and Config.Self must supply it.
	resolveSelf(ctx context.Context) (string, error)
}

type adapter struct {
	cfg        Config
	fl         flavor
	dataCenter bool
	// cloudClient is the ktrysmt client on a Cloud bot, nil on Data Center;
	// returned by CloudClient.
	cloudClient *gobitbucket.Client

	mu sync.Mutex
	// self is the bot's own resolved actor identity for reply-loop prevention
	// (Cloud account UUID / Data Center user slug). Written under mu before the
	// first request is handled, read by the handler under mu; re-resolved on every
	// Connect, never cleared on Disconnect.
	self              string
	srv               *http.Server
	boundAddr         string
	detachedCancel    context.CancelFunc
	logger            *slog.Logger
	inflight          atomic.Int64
	sem               chan struct{}
	readSem           chan struct{}
	maxRequestBytes   int64
	drainBudget       time.Duration
	selfResolveBudget time.Duration
}

// requestByteLimit picks the inbound body cap: largeRequestBytes only when a
// large-body event callback is registered, else smallRequestBytes.
func requestByteLimit(cfg Config) int64 {
	if cfg.OnPullRequest != nil || cfg.OnPush != nil {
		return largeRequestBytes
	}
	return smallRequestBytes
}

// log returns the Bot's logger handed over at Connect, or slog.Default() before
// the first Connect.
func (a *adapter) log() *slog.Logger {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// New creates a Bitbucket bot. It returns ErrMissingConfig if a required field is
// absent, ErrAmbiguousAuth if the auth mode is under- or over-specified, and
// otherwise applies defaults for Path and HTTPClient. The webhook server is not
// started until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.BitbucketBotType, a), nil
}

// Addr returns the address the bot's webhook listener is bound to (host:port), or
// "" if b is not a Bitbucket bot or is not currently connected. It lets a caller
// that passed cfg.Addr ":0" discover the OS-assigned port.
func Addr(b *core.Bot) string {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.boundAddr
	}
	return ""
}

// CloudClient returns the underlying ktrysmt Cloud client, or nil if b is not a
// Bitbucket bot OR is a Data Center bot. The name makes the flavor-dependence
// explicit at the call site: the Cloud REST 2.0 client has no Data Center
// equivalent (Data Center replies go out over plain net/http), so it is nil there.
func CloudClient(b *core.Bot) *gobitbucket.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.cloudClient
	}
	return nil
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.Secret == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: Secret and Addr are required", ErrMissingConfig)
	}

	apiTokenMode := cfg.Email != "" && cfg.APIToken != ""
	accessMode := cfg.AccessToken != ""
	// An access token alongside ANY Basic-auth field — even just one of
	// Email/APIToken — is ambiguous: the caller has half-specified two modes, and
	// silently ignoring the stray field would pick a mode they may not have meant.
	if accessMode && (cfg.Email != "" || cfg.APIToken != "") {
		return nil, ErrAmbiguousAuth
	}
	if !apiTokenMode && !accessMode {
		return nil, fmt.Errorf("%w: set Email+APIToken or AccessToken", ErrMissingConfig)
	}

	dataCenter := cfg.BaseURL != ""
	// An access token carries no bound user account (GET /2.0/user 401s), and Data
	// Center has no whoami at all, so neither can resolve Self and it must be
	// supplied.
	if cfg.Self == "" && (accessMode || dataCenter) {
		return nil, fmt.Errorf("%w: Self is required in access-token mode and on Data Center", ErrMissingConfig)
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

	a := &adapter{
		cfg:               cfg,
		dataCenter:        dataCenter,
		sem:               make(chan struct{}, maxConcurrentDispatch),
		readSem:           make(chan struct{}, maxConcurrentReads),
		maxRequestBytes:   requestByteLimit(cfg),
		drainBudget:       drainTimeout,
		selfResolveBudget: selfResolveTimeout,
		self:              cfg.Self,
	}

	if dataCenter {
		a.fl = &serverFlavor{
			baseURL:   strings.TrimRight(cfg.BaseURL, "/") + dataCenterAPIPath,
			http:      cfg.HTTPClient,
			applyAuth: authFunc(cfg, accessMode),
		}
	} else {
		client, err := newCloudClient(cfg, accessMode)
		if err != nil {
			return nil, err
		}
		client.HttpClient = cfg.HTTPClient
		a.cloudClient = client
		a.fl = &cloudFlavor{client: client}
	}
	return a, nil
}

// newCloudClient builds the ktrysmt Cloud client for the selected auth mode.
func newCloudClient(cfg Config, accessMode bool) (*gobitbucket.Client, error) {
	var (
		client *gobitbucket.Client
		err    error
	)
	if accessMode {
		client, err = gobitbucket.NewOAuthbearerToken(cfg.AccessToken)
	} else {
		client, err = gobitbucket.NewAPITokenAuth(cfg.Email, cfg.APIToken)
	}
	if err != nil {
		return nil, fmt.Errorf("bitbucket: build client: %w", err)
	}
	return client, nil
}

// authFunc returns the request authorizer for the Data Center net/http path:
// Bearer for an access token, HTTP Basic (email:token) for an API token.
func authFunc(cfg Config, accessMode bool) func(*http.Request) {
	if accessMode {
		token := cfg.AccessToken
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	email, apiToken := cfg.Email, cfg.APIToken
	return func(r *http.Request) { r.SetBasicAuth(email, apiToken) }
}

// isSelf reports whether id is the bot's own resolved identity, the reply-loop
// guard. Empty before self resolves, so it never drops a real user.
func (a *adapter) isSelf(id string) bool {
	a.mu.Lock()
	self := a.self
	a.mu.Unlock()
	return self != "" && id == self
}

// Attachments implements core.Adapter. Bitbucket comments are markdown with no
// upload channel worth modeling; v1 has no attachment support.
func (a *adapter) Attachments(_ *core.Message) ([]core.Attachment, error) {
	return nil, nil
}
