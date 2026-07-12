// Package github is the GitHub adapter for botbooter. It receives issue and PR
// comments as issue_comment webhook events over an inbound HTTP server and
// replies by creating issue comments through the GitHub REST API (go-github).
// It implements core.Adapter.
//
// Like the WhatsApp and Teams adapters, Connect binds a listener and serves
// until the run context is canceled; Disconnect shuts it down and drains
// in-flight dispatch. Bind a local Addr, put a TLS-terminating proxy in front,
// and register the public HTTPS URL as the repository or App webhook URL
// (content type application/json, events: issue_comment, with a secret).
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

	// The endpoint is public; cap bodies against memory exhaustion. Real
	// issue_comment payloads are tens of KB at most.
	maxRequestBytes = 1 << 20 // 1 MiB

	shutdownTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("github: missing required config field")

// ErrAmbiguousAuth is returned by New when both PAT and App auth are configured.
var ErrAmbiguousAuth = errors.New("github: configure either Token or AppID/InstallationID/PrivateKey, not both")

// ErrBadReactionConfig is returned by New when a reaction-polling Config field
// is malformed: a ReactionPollRepos entry that is not "owner/name", or a
// negative ReactionPollInterval or ReactionLookback.
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

	// HTTPClient is the base client for outbound GitHub API calls; a default
	// client with a 30s timeout is used when nil. In App mode only its
	// Transport (http.DefaultTransport when nil) and Timeout are used — the
	// Transport becomes the inner transport of the ghinstallation
	// token-refreshing transport, and other fields (Jar, CheckRedirect) are
	// ignored.
	HTTPClient *http.Client

	// ReactionPollRepos lists repositories ("owner/name") whose newest issue
	// comments are polled for emoji reactions, because GitHub sends no webhook
	// for them. Empty (the default) disables polling and OnReaction never
	// fires. Coverage is deliberately partial — only reactions on each repo's
	// newest comments are seen — and each listed repo costs at least one API
	// request per poll cycle.
	ReactionPollRepos []string
	// ReactionPollInterval is the delay between reaction poll cycles; it
	// defaults to 30 seconds and is also the reaction delivery latency.
	ReactionPollInterval time.Duration
	// ReactionStore dedups reactions across poll cycles; it defaults to an
	// in-process store, so a restart forgets what was already handled. Provide
	// a persistent implementation together with ReactionLookback to catch
	// reactions added while the bot was down.
	ReactionStore ReactionStore
	// ReactionLookback widens the dispatch window: a reaction is dispatched
	// only if the store has not seen it and it was created after (connect time
	// - ReactionLookback). The zero default dispatches only reactions added
	// while connected.
	ReactionLookback time.Duration
}

type adapter struct {
	cfg    Config
	client *gogithub.Client
	// pollRepos is cfg.ReactionPollRepos parsed once at New; empty means
	// reaction polling is disabled and Connect starts no poller.
	pollRepos []repoRef

	mu sync.Mutex
	// selfID identifies the bot's own account for reply-loop prevention in
	// PAT mode. Written under mu by the serve goroutine before the first
	// request is handled, read by the handler under mu; re-resolved on every
	// PAT-mode Connect, never cleared on Disconnect (stale values are
	// harmless with no server up). Zero in App mode, where the Bot-type
	// filter in isSelfOrBot does the job.
	selfID int64
	srv    *http.Server
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
	pollRepos, err := parsePollRepos(cfg.ReactionPollRepos)
	if err != nil {
		return nil, err
	}
	if cfg.ReactionPollInterval < 0 || cfg.ReactionLookback < 0 {
		return nil, fmt.Errorf("%w: ReactionPollInterval and ReactionLookback must not be negative", ErrBadReactionConfig)
	}
	if cfg.ReactionPollInterval == 0 {
		cfg.ReactionPollInterval = defaultReactionPollInterval
	}
	// The store outlives connections on purpose: a reconnect keeps its seen
	// set, so reactions handled before the reconnect are not re-dispatched.
	if cfg.ReactionStore == nil && len(pollRepos) > 0 {
		cfg.ReactionStore = newMemoryReactionStore()
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

	a := &adapter{cfg: cfg, pollRepos: pollRepos}
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
