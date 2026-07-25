// Package gitlab is the GitLab adapter for botbooter. It receives issue and
// merge-request comments as Note Hook webhook events over an inbound HTTP
// server and replies by creating notes through the GitLab REST API
// (gitlab.com/gitlab-org/api/client-go). It implements core.Adapter. Optional
// Config callbacks (OnMergeRequest, OnPush) additionally route Merge Request
// and Push deliveries on the same webhook endpoint; unset, those deliveries are
// acked and dropped.
//
// Like the WhatsApp, Teams and GitHub adapters, Connect binds a listener and
// serves until the run context is canceled; Disconnect shuts it down and drains
// in-flight dispatch. Bind a local Addr, put a TLS-terminating proxy in front,
// and register the public HTTPS URL as the project (or group) webhook URL, with
// the Comments trigger enabled — and Confidential comments too, to also answer
// on confidential issues (internal notes are dropped: the reply path can only
// create a plain note) — (plus Merge request and Push events when the matching
// callback is set) and a Secret token.
//
// Unlike GitHub (which signs the body with an HMAC), GitLab authenticates a
// delivery with a plain X-Gitlab-Token header compared against Config.Secret, so
// the adapter can reject an unauthenticated request BEFORE buffering its body —
// a stronger pre-auth posture than the sibling webhook adapters can offer.
//
// Implementation split (mirrors GitHub/Teams): gitlab.go (config, client wiring,
// accessors), server.go (webhook lifecycle), send.go (replies), message.go
// (payload mapping). GitLab reactions ride a native Emoji webhook rather than
// the REST-poll GitHub needs, so reaction ingress is deliberately left to a
// future slice; v1 has none (like the Teams adapter).
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

	// The endpoint is public and each delivery is buffered whole before it is
	// parsed, so the body cap bounds attacker-controllable buffering. The
	// effective cap is chosen per adapter by requestByteLimit. A Note payload
	// carries the note plus the whole noteable, and GitLab caps a note body at
	// ~1M characters and an issue/merge-request description at 1 MiB, so a
	// legitimate comment on a long issue can approach a few MiB once JSON
	// escaping and the rest of the object are counted: smallRequestBytes sits
	// above that worst case (a dropped comment cannot be recovered — GitLab does
	// not re-deliver) while still keeping a comment-only bot far below the tens of
	// megabytes a large event can carry. Merge Request and Push deliveries (many
	// commits, many changed paths) can be much larger, so a bot that registers
	// OnMergeRequest/OnPush raises the cap to largeRequestBytes — it has opted
	// into large events and the memory cost.
	smallRequestBytes = 4 << 20  // 4 MiB (Note and unhandled events)
	largeRequestBytes = 25 << 20 // 25 MiB (large Merge Request / Push deliveries)

	// maxConcurrentReads bounds concurrent inbound processing (the body read +
	// parse). Even though the token is verified before the body is read, a flood
	// of authenticated large POSTs could each buffer the request cap; this caps
	// peak read memory at maxConcurrentReads times the effective cap. A delivery
	// arriving past the bound is logged and acked 200 without being read: GitLab
	// never re-delivers it either way, and a 503 would additionally push the hook
	// toward being auto-disabled. It is separate from maxConcurrentDispatch because
	// a read is short-lived while a dispatch goroutine may run a slow handler.
	maxConcurrentReads = 16

	// maxConcurrentDispatch bounds in-flight webhook dispatch goroutines. A
	// delivery that would exceed it is logged and acked 200 without dispatching
	// (see ackAndDispatch), instead of spawning an unbounded goroutine under a
	// flood.
	maxConcurrentDispatch = 256

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
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
	// sem bounds concurrent webhook dispatch goroutines (see
	// maxConcurrentDispatch). A non-blocking acquire before the ack turns a
	// saturating flood into logged, acked drops instead of unbounded
	// goroutines. Re-created per
	// Connect and captured at acquire, so a slot that a context-ignoring handler
	// never releases is confined to that one connection.
	sem chan struct{}
	// readSem bounds concurrent inbound request processing so a flood of large
	// bodies can't buffer unbounded memory (see maxConcurrentReads); separate
	// from sem, which bounds dispatch goroutines.
	readSem chan struct{}
	// maxRequestBytes is the inbound body cap, fixed at New by requestByteLimit:
	// smallRequestBytes unless OnMergeRequest/OnPush is registered, then
	// largeRequestBytes.
	maxRequestBytes int64
	// drainBudget is how long Disconnect waits for in-flight dispatch goroutines.
	// Fixed at New to drainTimeout and never reassigned afterwards; it is a field
	// rather than the constant so a test can shorten it and exercise the
	// deadline-reached path in milliseconds instead of seconds.
	drainBudget time.Duration
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
		cfg:             cfg,
		client:          client,
		sem:             make(chan struct{}, maxConcurrentDispatch),
		readSem:         make(chan struct{}, maxConcurrentReads),
		maxRequestBytes: requestByteLimit(cfg),
		drainBudget:     drainTimeout,
	}, nil
}
