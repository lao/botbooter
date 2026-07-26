// Webhook server lifecycle: bind and serve the endpoint, authenticate and dispatch
// Activities, drain in-flight dispatch on shutdown, and record the
// conversation -> reply-routing map that Send reads.

package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lao/botbooter/internal/core"
)

const (
	// maxRequestBytes caps the inbound webhook body. The endpoint is public, so
	// this defends against memory exhaustion; real Activities are a few KB.
	maxRequestBytes = 1 << 20 // 1 MiB
	// maxConversations bounds the conversation->serviceUrl map so a public endpoint
	// cannot grow it without limit.
	maxConversations = 10000
	// maxConcurrentDispatch bounds in-flight dispatch goroutines so a burst of
	// inbound Activities cannot spawn unbounded work. The handler acquires a slot
	// before acking and sheds load with 503 (the platform retries) when full.
	maxConcurrentDispatch = 256
)

// conversation holds what a reply needs: the serviceUrl to POST to and the bot's
// own account (the reply's required from field). The recipient is not cached: it
// is per-activity while replies are keyed by conversation id, so caching one
// would race concurrent senders in a shared channel.
type conversation struct {
	serviceURL string
	bot        channelAccount
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the drain, WithCancel lets
	// Disconnect abort stragglers after it.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.handleMessages(detachedCtx, w, r, deps)
	})

	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		detachedCancel()
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
	a.boundAddr = ln.Addr().String()
	a.detachedCancel = detachedCancel
	a.logger = deps.Logger
	// Fresh per-connection dispatch semaphore: a slot a hung handler never
	// releases dies with this connection instead of leaking across reconnects.
	a.dispatchSem = make(chan struct{}, maxConcurrentDispatch)
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

func (a *adapter) handleMessages(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
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

	// Authenticate before trusting the body. Use the request context, not runCtx:
	// core cancels runCtx before srv.Shutdown drains this handler, so a JWKS
	// refresh during drain must ride r.Context() or it would 401 an
	// already-in-flight request.
	if err := a.validateInbound(r.Context(), r.Header.Get("Authorization"), act.ServiceURL, act.ChannelID); err != nil {
		a.log().Warn("teams: inbound request rejected with 401", "error", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Even with a valid token, only POST replies to Bot Framework hosts (SSRF).
	if !isAllowedServiceHost(act.ServiceURL) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Only user "message" Activities are dispatched; skip other types and drop
	// bot-authored messages (from.role == "bot") to avoid reply loops. These are
	// acked (200) with nothing to do, so they consume no dispatch slot.
	if act.Type != "message" || strings.EqualFold(act.From.Role, "bot") {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Bound concurrent dispatch with a counting semaphore: acquire a slot before
	// acking so saturation returns 503 (the platform retries) rather than acking a
	// message the adapter would then drop. Non-blocking so a burst sheds load
	// instead of stalling the handler. Snapshot the per-connection semaphore once so
	// the acquire here and the release below target the SAME channel across a
	// reconnect that swaps a.dispatchSem.
	a.mu.Lock()
	sem := a.dispatchSem
	a.mu.Unlock()
	select {
	case sem <- struct{}{}:
	default:
		a.log().Warn("teams: dispatch concurrency limit reached; shedding with 503", "limit", maxConcurrentDispatch)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	a.recordConversation(act.Conversation.ID, act.ServiceURL, act.Recipient)

	w.WriteHeader(http.StatusOK)

	msg := toMessage(&act, body)
	// Dispatch on the detached context: core cancels runCtx before Disconnect's
	// drain waits for this handler, so a reply on runCtx would fail mid-drain.
	// The increment lands before Shutdown returns, so drainDispatch observes it.
	// The semaphore slot is released when dispatch returns.
	a.inflight.Add(1)
	go func() {
		defer func() { <-sem }()
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
	// Shutdown and drain get separate budgets: dispatch runs outside the handler
	// lifecycle, so a slow Shutdown must not consume the drain deadline and drop an
	// acked message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	// If the drain timed out, surface it: cancelDispatch below force-aborts
	// already-acked messages, which is operationally significant.
	var drainErr error
	if n := a.inflight.Load(); n > 0 {
		a.log().Warn("teams: drain deadline reached; canceling in-flight dispatches", "inflight", n)
		drainErr = fmt.Errorf("teams: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv), so a concurrent Connect's live state
	// is not clobbered. Either way cancel this connection's own context after the
	// drain so a blocked handler cannot leak.
	a.mu.Lock()
	if a.srv == srv {
		a.srv = nil
		a.boundAddr = ""
		a.detachedCancel = nil
	}
	a.mu.Unlock()

	if cancelDispatch != nil {
		cancelDispatch()
	}
	if err != nil {
		return err
	}
	return drainErr
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

// drainDispatch waits, bounded by ctx, for in-flight dispatch so an acked
// message is not dropped at shutdown. It polls an atomic counter rather than a
// WaitGroup: an Add racing Wait would risk a misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// recordConversation maps a conversation to its serviceUrl and the bot account,
// with FIFO eviction so a public endpoint cannot grow the map without limit. A
// much-later proactive send may find an evicted conversation, and Send then
// returns [ErrUnknownConversation].
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
