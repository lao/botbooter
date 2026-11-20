// Package gitlab receives issue and merge request notes over a webhook and
// replies through the GitLab API. Internal notes are ignored because the reply API
// creates ordinary notes. The webhook token is checked before reading the request body.
package gitlab

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

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath = "/webhook"

	// A Note includes the issue or merge request description. Allow larger merge request and push payloads.
	smallRequestBytes = 4 << 20  // 4 MiB (Note and unhandled events)
	largeRequestBytes = 25 << 20 // 25 MiB (large Merge Request / Push deliveries)

	// Concurrent reads bound buffered bodies to maxConcurrentReads times the request limit.
	maxConcurrentReads = 16

	maxConcurrentDispatch = 256

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second

	// selfResolveTimeout bounds the Connect-path GET /user probe so an
	// unreachable-but-non-rejecting API surfaces as an error promptly. The default
	// HTTPClient carries a 30s timeout, but Config.HTTPClient is a supported knob
	// and a consumer-supplied client need not set one; combined with a run context
	// that has no deadline and an owner that never calls Disconnect, the probe
	// would otherwise hang forever — with the listener already bound, so GitLab's
	// deliveries pile up in the accept backlog and time out while Serve never
	// starts and Run never returns an error. Mirrors the Telegram adapter's
	// getMeTimeout.
	selfResolveTimeout = 15 * time.Second
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("gitlab: missing required config field")

// Config configures a GitLab bot.
type Config struct {
	// Token is an access token the bot authenticates and posts notes as: a
	// personal, project, or group access token. Required.
	Token string

	// Secret is matched (constant-time) against the X-Gitlab-Token header on
	// inbound webhook requests. Required: without it the endpoint would accept
	// spoofed payloads. It is the "Secret token" configured on the GitLab webhook.
	Secret string

	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080". Required.
	Addr string

	// Path is the webhook route; it defaults to /webhook.
	Path string

	// BaseURL is the GitLab instance URL for a self-hosted deployment, e.g.
	// "https://gitlab.example.com". Empty (the default) targets gitlab.com. The
	// /api/v4 suffix is appended automatically, so either form is accepted.
	BaseURL string

	// OnMergeRequest, when set, receives Merge Request webhook deliveries whose
	// action is "open", "reopen" or "update" — the deliveries that create or
	// change an MR's reviewable content. Other actions, and MRs authored by this
	// bot's own account, are acked and dropped, mirroring the comment path's
	// reply-loop filter. The callback runs on a dispatch goroutine covered by
	// Disconnect's drain (same contract as message dispatch), so it should hand
	// long work off and return promptly. Nil (the default) acks and drops Merge
	// Request deliveries; the webhook must also enable the Merge request events
	// trigger, or GitLab never delivers one.
	OnMergeRequest func(ctx context.Context, event *gogitlab.MergeEvent)

	// OnPush, when set, receives Push webhook deliveries, unfiltered — ref
	// filtering (e.g. default branch only) is the callback's job. Same goroutine
	// and drain contract as OnMergeRequest. Nil (the default) acks and drops; the
	// webhook must also enable the Push events trigger.
	OnPush func(ctx context.Context, event *gogitlab.PushEvent)

	// HTTPClient is the base client for outbound GitLab API calls; a client with
	// a 30s timeout is used when nil.
	HTTPClient *http.Client
}

type adapter struct {
	cfg    Config
	client *gogitlab.Client

	mu sync.Mutex
	// selfID identifies the bot's own account for reply-loop prevention: GitLab
	// note webhooks carry no reliable bot flag, so the bot's own notes are
	// recognized by author id. Written under mu by the serve goroutine before the
	// first request is handled, read by the handler under mu; re-resolved on every
	// Connect, never cleared on Disconnect (a stale value is harmless with no
	// server up).
	selfID int64
	srv    *http.Server
	// boundAddr is the listener's resolved address, so a cfg.Addr of ":0" is
	// recoverable via Addr. Set with srv, cleared with it.
	boundAddr string
	// detachedCancel aborts the current connection's dispatch goroutines. Each
	// Connect derives one detached, cancelable context and threads it through the
	// handler closure, so only the cancel is shared state. Disconnect calls it
	// after the drain window so a stuck handler cannot leak, and clears it only
	// when a reconnect has not already installed a newer connection.
	detachedCancel context.CancelFunc
	// logger is the Bot's logger handed over at Connect; read via log().
	logger   *slog.Logger
	inflight atomic.Int64
	// Replaced on Connect so a stuck handler consumes only its own connection's slot.
	dispatchSlots   chan struct{}
	readSlots       chan struct{}
	maxRequestBytes int64
	// drainBudget is how long Disconnect waits for in-flight dispatch goroutines.
	// Fixed at New to drainTimeout and never reassigned afterwards; it is a field
	// rather than the constant so a test can shorten it and exercise the
	// deadline-reached path in milliseconds instead of seconds.
	drainBudget time.Duration
	// selfResolveBudget bounds the Connect-path self-identity probe. Fixed at New
	// to selfResolveTimeout and never reassigned afterwards; a field for the same
	// reason as drainBudget — a test can shorten it to exercise the timeout path
	// without waiting out the real bound.
	selfResolveBudget time.Duration
}

// requestByteLimit picks the inbound body cap: largeRequestBytes only when a
// large-body event callback (OnMergeRequest/OnPush) is registered, else
// smallRequestBytes, so a bot that handles only comments never buffers the large
// ceiling it has no use for.
func requestByteLimit(cfg Config) int64 {
	if cfg.OnMergeRequest != nil || cfg.OnPush != nil {
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

// New creates a GitLab bot. It returns ErrMissingConfig if a required field is
// absent, and otherwise applies defaults for Path and HTTPClient. The webhook
// server is not started until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.GitLabBotType, a), nil
}

// Addr returns the address the bot's webhook listener is bound to (host:port),
// or "" if b is not a GitLab bot or is not currently connected. It lets a caller
// that passed cfg.Addr ":0" discover the OS-assigned port.
func Addr(b *core.Bot) string {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.boundAddr
	}
	return ""
}

// Client returns the underlying client-go client, or nil if b is not a GitLab
// bot. Use it for API calls beyond the adapter's send path (labels, awards,
// pipelines); it is safe for concurrent use.
func Client(b *core.Bot) *gogitlab.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.Token == "" || cfg.Secret == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: Token, Secret and Addr are required", ErrMissingConfig)
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

	opts := []gogitlab.ClientOptionFunc{gogitlab.WithHTTPClient(cfg.HTTPClient)}
	if cfg.BaseURL != "" {
		opts = append(opts, gogitlab.WithBaseURL(cfg.BaseURL))
	}
	client, err := gogitlab.NewClient(cfg.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build client: %w", err)
	}

	return &adapter{
		cfg:               cfg,
		client:            client,
		dispatchSlots:     make(chan struct{}, maxConcurrentDispatch),
		readSlots:         make(chan struct{}, maxConcurrentReads),
		maxRequestBytes:   requestByteLimit(cfg),
		drainBudget:       drainTimeout,
		selfResolveBudget: selfResolveTimeout,
	}, nil
}
