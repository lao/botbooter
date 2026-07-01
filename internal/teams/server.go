// Webhook server lifecycle: bind and serve the endpoint, authenticate and dispatch
// Activities, drain in-flight dispatch on shutdown, and record the
// conversation -> reply-routing map that Send reads.

package teams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
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
)

// conversation holds what a reply needs: the serviceUrl to POST to and the bot's
// own account (used as the reply's required from field). The reply's recipient is
// not stored: it is per-activity while replies are keyed by conversation id, so
// caching one would race concurrent senders in a shared channel. The bot account is
// conversation-stable, so it is safe to cache.
type conversation struct {
	serviceURL string
	bot        channelAccount
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the drain, WithCancel lets
	// Disconnect abort stragglers after it. The handler captures it directly, so
	// there is no shared context field to race on.
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

	// Authenticate before trusting the body: validate the JWT and its serviceurl claim.
	if err := a.validateInbound(ctx, r.Header.Get("Authorization"), act.ServiceURL, act.ChannelID); err != nil {
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

	// Only user "message" Activities are dispatched; skip other types and drop
	// bot-authored messages (from.role == "bot") to avoid reply loops.
	if act.Type != "message" {
		return
	}
	if strings.EqualFold(act.From.Role, "bot") {
		return
	}

	a.recordConversation(act.Conversation.ID, act.ServiceURL, act.Recipient)

	msg := toMessage(&act, body)
	// Dispatch on the detached context, not runCtx: core cancels runCtx before
	// Disconnect drains this handler, so a reply on runCtx would fail "context
	// canceled" mid-drain. dispatchCtx drops runCtx's cancellation so an acked
	// message finishes within the drain window; Disconnect cancels it afterward to
	// bound a stuck reply. Increment before starting the goroutine so drainDispatch
	// sees the count (Shutdown waits for this handler to return).
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
	// Shutdown and drain get separate budgets: dispatch runs outside the handler
	// lifecycle, so a slow Shutdown must not consume the drain deadline and drop an
	// acked message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	// If the drain timed out, cancelDispatch below force-aborts the stragglers; log
	// that, since force-aborting an acked message is operationally significant.
	if n := a.inflight.Load(); n > 0 {
		log.Printf("teams: drain deadline reached; canceling %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv), so a concurrent Connect's live cancel is
	// not clobbered. Either way cancel this connection's own context after the drain
	// so a blocked handler cannot leak. CancelFunc is idempotent.
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

// drainDispatch waits (bounded by ctx) for in-flight dispatch to finish so an acked
// message is not dropped at shutdown. It polls an atomic counter, not a WaitGroup:
// handlers Shutdown may abandon could Add concurrently with Wait and risk a misuse
// panic.
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
// with FIFO eviction so a public endpoint cannot grow the map without limit.
// Eviction is by first-seen: replies happen in the same dispatch as the recording,
// so request/response bots always resolve. A much-later proactive send could find
// an evicted conversation, and Send then returns a clear error.
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
