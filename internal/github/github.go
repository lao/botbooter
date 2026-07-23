// Package github is the GitHub adapter for botbooter. It receives issue and PR
// comments as issue_comment webhook events over an inbound HTTP server and
// replies by creating issue comments through the GitHub REST API (go-github).
// It implements core.Adapter. Optional Config callbacks (OnPullRequest,
// OnPush) additionally route pull_request and push deliveries on the same
// webhook endpoint; unset, those deliveries are acked and dropped.
//
// Like the WhatsApp and Teams adapters, Connect binds a listener and serves
// until the run context is canceled; Disconnect shuts it down and drains
// in-flight dispatch. Bind a local Addr, put a TLS-terminating proxy in front,
// and register the public HTTPS URL as the repository or App webhook URL
// (content type application/json, events: issue_comment plus pull_request
// and/or push when the matching callback is set, with a secret).
//
// Implementation split (mirrors Teams): github.go (config, auth wiring,
// accessors), server.go (webhook lifecycle), send.go (replies), message.go
// (payload mapping), reactions.go (opt-in polled reaction ingress — GitHub
// sends no webhook for reactions).
package github

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

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath = "/webhook"

	// The endpoint is public; cap bodies against memory exhaustion.
	// issue_comment payloads are tens of KB, but push and pull_request
	// deliveries (many commits, large PR bodies) can reach GitHub's 25 MiB
	// webhook ceiling, so cap there rather than at 1 MiB — a lower cap silently
	// dropped real push/PR deliveries at the body reader. Peak memory from
	// concurrent large reads is bounded by maxConcurrentReads (below), not by
	// this per-request cap alone.
	maxRequestBytes = 25 << 20 // 25 MiB (GitHub's webhook payload ceiling)

	// maxConcurrentReads bounds concurrent inbound processing (the
	// up-to-maxRequestBytes body read + HMAC verify + parse). The body must be
	// read before its signature can be checked, so without this an
	// unauthenticated flood of large POSTs could each buffer maxRequestBytes with
	// no limit; this caps peak read memory at maxConcurrentReads*maxRequestBytes
	// (~400 MiB), keeping the worst case friendly to memory-constrained
	// deployments (small containers). It is kept well above realistic GitHub
	// delivery concurrency, and a saturating burst is shed with 503 (GitHub
	// retries), so legitimate traffic is not dropped. It is separate from
	// maxConcurrentDispatch because a read is short-lived while a dispatch
	// goroutine may run a slow handler.
	maxConcurrentReads = 16

	// maxConcurrentDispatch bounds in-flight webhook dispatch goroutines. A
	// delivery that would exceed it is answered 503 (GitHub retries) instead of
	// spawning an unbounded goroutine under a flood. The reaction poller is
	// excluded — its cadence is self-bounded and it has no HTTP response to 503.
	maxConcurrentDispatch = 256

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("github: missing required config field")

// ErrAmbiguousAuth is returned by New when both PAT and App auth are configured.
var ErrAmbiguousAuth = errors.New("github: configure either Token or AppID/InstallationID/PrivateKey, not both")

// ErrBadReactionConfig is returned by New when a reaction-polling Config field
// is malformed: a ReactionPollRepos entry that is not "owner/name" or the
// wildcard "owner/*", or a negative ReactionPollInterval.
var ErrBadReactionConfig = errors.New("github: invalid reaction polling config")

// Config configures a GitHub bot. Exactly one auth mode must be set: Token
// (PAT mode) or the AppID/InstallationID/PrivateKey triple (App mode).
type Config struct {
	// Token is a personal access token (classic or fine-grained) for PAT mode.
	// The bot posts comments as the token's user.
	Token string

	// AppID is the GitHub App ID for App mode.
	AppID int64
	// InstallationID is the App installation to act as; it is also visible in
	// webhook payloads and on the installation settings page.
	InstallationID int64
	// PrivateKey is the App's RSA private key, PEM-encoded.
	PrivateKey []byte

	// WebhookSecret verifies the X-Hub-Signature-256 HMAC on inbound webhook
	// requests. Required: without it the endpoint would accept spoofed payloads.
	WebhookSecret string
	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080".
	Addr string
	// Path is the webhook route; it defaults to /webhook.
	Path string

	// OnPullRequest, when set, receives pull_request webhook deliveries whose
	// action is "opened", "reopened" or "synchronize" — the deliveries that
	// create or change a PR's reviewable content. Other actions, and PRs
	// authored by any bot or by this bot's own account, are acked and dropped,
	// mirroring the comment path's reply-loop filter. The callback runs on a
	// dispatch goroutine covered by Disconnect's drain (same contract as
	// message dispatch), so it should hand long work off and return promptly.
	// Nil (the default) keeps the previous behavior: pull_request deliveries
	// are acked and dropped. The webhook must also be subscribed to the
	// pull_request event, or GitHub never delivers one.
	OnPullRequest func(ctx context.Context, event *gogithub.PullRequestEvent)

	// OnPush, when set, receives push webhook deliveries, unfiltered — ref
	// filtering (e.g. default branch only) is the callback's job. Same
	// goroutine and drain contract as OnPullRequest. Nil (the default) acks
	// and drops; the webhook must also be subscribed to the push event.
	OnPush func(ctx context.Context, event *gogithub.PushEvent)

	// HTTPClient is the base client for outbound GitHub API calls; a default
	// client with a 30s timeout is used when nil. In App mode only its
	// Transport (http.DefaultTransport when nil) and Timeout are used — the
	// Transport becomes the inner transport of the ghinstallation
	// token-refreshing transport, and other fields (Jar, CheckRedirect) are
	// ignored.
	HTTPClient *http.Client

	// ReactionPollRepos lists repositories whose newest issue comments are
	// polled for emoji reactions, because GitHub sends no webhook for them.
	// An entry is either explicit ("owner/name") or a wildcard ("owner/*"):
	// every repository of that owner the credentials can see, minus archived
	// ones — the authenticated user's own repos (private included) in PAT
	// mode when the owner is the bot itself, that owner's public repos
	// otherwise, and the installation's repos in App mode. Wildcards are
	// re-resolved every ~10 poll cycles, so new repositories are picked up
	// without a restart. Empty (the default) disables polling and OnReaction
	// never fires. The poller also requires an OnReaction handler registered
	// before the bot connects — with repos listed but no handler, no poller
	// starts and no API requests are spent. Coverage is deliberately partial
	// — only reactions on each repo's newest comments are seen — and each
	// polled repo costs at least one API request per poll cycle (worst case
	// ~11, when every window comment's reaction count changed). Duplicate
	// entries collapse to one.
	ReactionPollRepos []string
	// ReactionPollInterval is the delay between reaction poll cycles; it
	// defaults to 30 seconds and is also the reaction delivery latency.
	// When the polled repo count at this interval would exceed the poller's
	// API request budget (3000 requests/hour), the adapter logs a warning and
	// automatically raises the effective interval to fit — unless
	// ReactionPollNoAutoInterval is set. Reaction dedup across cycles is
	// in-process only: a restart forgets what was handled, and only reactions
	// added while connected are dispatched, so reactions added while the bot
	// was down are missed.
	ReactionPollInterval time.Duration
	// ReactionPollNoAutoInterval disables the automatic raising of the
	// effective poll interval when the polled repo count would exceed the API
	// request budget. The over-budget warning is still logged; the configured
	// ReactionPollInterval is honored and the rate limit may be exhausted.
	ReactionPollNoAutoInterval bool
}

type adapter struct {
	cfg    Config
	client *gogithub.Client
	// pollRepos and wildcardOwners are cfg.ReactionPollRepos parsed once at
	// New: explicit "owner/name" entries and "owner/*" wildcard owners. Both
	// empty means reaction polling is disabled and Connect starts no poller.
	pollRepos      []repoRef
	wildcardOwners []string
	// reactions dedups polled reactions across cycles. Allocated at New only
	// when polling is on, and shared across reconnects on purpose: a reaction
	// handled before a reconnect is not re-dispatched after it.
	reactions *reactionStore

	mu sync.Mutex
	// selfID identifies the bot's own account for reply-loop prevention in
	// PAT mode. Written under mu by the serve goroutine before the first
	// request is handled, read by the handler under mu; re-resolved on every
	// PAT-mode Connect, never cleared on Disconnect (stale values are
	// harmless with no server up). Zero in App mode, where the Bot-type
	// filter in isSelfOrBot does the job. selfLogin is the matching login,
	// used by wildcard repo discovery to route the bot's own owner through
	// the authenticated-user listing (which sees private repos).
	selfID    int64
	selfLogin string
	srv       *http.Server
	// boundAddr is the listener's resolved address, so a cfg.Addr of ":0" is
	// recoverable via Addr. Set with srv, cleared with it.
	boundAddr string
	// detachedCancel aborts the current connection's dispatch goroutines. Each
	// Connect derives one detached, cancelable context and threads it through
	// the handler closure, so only the cancel is shared state. Disconnect calls
	// it after the drain window so a stuck handler cannot leak, and clears it
	// only when a reconnect has not already installed a newer connection.
	detachedCancel context.CancelFunc
	// logger is the Bot's logger handed over at Connect; read via log().
	logger   *slog.Logger
	inflight atomic.Int64
	// sem bounds concurrent webhook dispatch goroutines (see
	// maxConcurrentDispatch). A non-blocking acquire before the ack turns a
	// saturating flood into 503s instead of unbounded goroutines. Buffered at
	// New and shared across reconnects; the reaction poller does not use it.
	sem chan struct{}
	// readSem bounds concurrent inbound request processing so a flood of large
	// bodies can't buffer unbounded memory before the HMAC check (see
	// maxConcurrentReads); separate from sem, which bounds dispatch goroutines.
	readSem chan struct{}
}

// pollingConfigured reports whether ReactionPollRepos named anything to poll,
// explicitly or by wildcard.
func (a *adapter) pollingConfigured() bool {
	return len(a.pollRepos) > 0 || len(a.wildcardOwners) > 0
}

// log returns the Bot's logger handed over at Connect, or slog.Default()
// before the first Connect.
func (a *adapter) log() *slog.Logger {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// New creates a GitHub bot. It returns ErrMissingConfig if a required field is
// absent and ErrAmbiguousAuth if both auth modes are set, and otherwise applies
// defaults for Path and HTTPClient. The webhook server is not started until the
// bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.GitHubBotType, a), nil
}

// Addr returns the address the bot's webhook listener is bound to (host:port),
// or "" if b is not a GitHub bot or is not currently connected. It lets a
// caller that passed cfg.Addr ":0" discover the OS-assigned port.
func Addr(b *core.Bot) string {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.boundAddr
	}
	return ""
}

// Client returns the underlying go-github client, or nil if b is not a GitHub
// bot. Use it for API calls beyond the adapter's send path (labels, reactions,
// checks); it is safe for concurrent use.
func Client(b *core.Bot) *gogithub.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

func newAdapter(cfg Config) (*adapter, error) {
	patMode := cfg.Token != ""
	appMode := cfg.AppID != 0 || cfg.InstallationID != 0 || len(cfg.PrivateKey) > 0
	switch {
	case patMode && appMode:
		return nil, ErrAmbiguousAuth
	case !patMode && !appMode:
		return nil, fmt.Errorf("%w: Token or AppID/InstallationID/PrivateKey is required", ErrMissingConfig)
	case appMode && (cfg.AppID == 0 || cfg.InstallationID == 0 || len(cfg.PrivateKey) == 0):
		return nil, fmt.Errorf("%w: App mode needs AppID, InstallationID and PrivateKey", ErrMissingConfig)
	}
	if cfg.WebhookSecret == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: WebhookSecret and Addr are required", ErrMissingConfig)
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
	pollRepos, wildcardOwners, err := parsePollRepos(cfg.ReactionPollRepos)
	if err != nil {
		return nil, err
	}
	if cfg.ReactionPollInterval < 0 {
		return nil, fmt.Errorf("%w: ReactionPollInterval must not be negative", ErrBadReactionConfig)
	}
	if cfg.ReactionPollInterval == 0 {
		cfg.ReactionPollInterval = defaultReactionPollInterval
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	// ghinstallation calls its inner transport without a nil check, and the
	// default HTTPClient has a nil Transport — normalize explicitly.
	baseTransport := cfg.HTTPClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}

	a := &adapter{
		cfg:            cfg,
		pollRepos:      pollRepos,
		wildcardOwners: wildcardOwners,
		sem:            make(chan struct{}, maxConcurrentDispatch),
		readSem:        make(chan struct{}, maxConcurrentReads),
	}
	if a.pollingConfigured() {
		a.reactions = newReactionStore()
	}
	if patMode {
		client, err := gogithub.NewClient(
			gogithub.WithHTTPClient(cfg.HTTPClient),
			gogithub.WithAuthToken(cfg.Token),
		)
		if err != nil {
			return nil, fmt.Errorf("github: build client: %w", err)
		}
		a.client = client
	} else {
		itr, err := ghinstallation.New(baseTransport, cfg.AppID, cfg.InstallationID, cfg.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("github: build installation transport: %w", err)
		}
		client, err := gogithub.NewClient(gogithub.WithHTTPClient(
			&http.Client{Transport: itr, Timeout: cfg.HTTPClient.Timeout},
		))
		if err != nil {
			return nil, fmt.Errorf("github: build client: %w", err)
		}
		a.client = client
	}
	return a, nil
}
