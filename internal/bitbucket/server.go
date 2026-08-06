package bitbucket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lao/botbooter/internal/core"
)

const (
	// signatureHeader carries the HMAC-SHA256 of the raw body as "sha256=<hex>".
	// This is the one thing Cloud and Data Center share, so verification is
	// flavor-independent.
	signatureHeader = "X-Hub-Signature" //nolint:gosec // G101: HTTP header name, not a credential

	// eventKeyHeader names the event, e.g. "pullrequest:comment_created" (Cloud) or
	// "pr:comment:added" (Data Center).
	eventKeyHeader = "X-Event-Key"

	signaturePrefix = "sha256="
)

// handleWebhook reads, authenticates, routes, acks and dispatches one delivery.
// Unlike GitLab (a plain token header, checkable before the body), Bitbucket
// signs the body, so the body is read BEFORE auth (GitHub's posture) — the
// readSem and the body cap bound what an unauthenticated flood can buffer.
//
// Every authentic delivery is acked 200, even when dropped, unreadable or shed
// under load: Bitbucket times a delivery out at 10s and retries a failure twice
// with no backoff, so a slow or 5xx response turns into duplicate deliveries. A
// bad signature is the only 401 — a wrong secret should be visible, and its body
// is not authentic so there is nothing to double-deliver.
func (a *adapter) handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	// Bound concurrent inbound processing before buffering the (up to
	// a.maxRequestBytes) body. a.readSem is allocated once in newAdapter and never
	// reassigned, so this direct read races nothing.
	select {
	case a.readSem <- struct{}{}:
	default:
		a.log().Warn("bitbucket: inbound concurrency limit reached; acking and dropping delivery", "limit", maxConcurrentReads)
		w.WriteHeader(http.StatusOK)
		return
	}
	defer func() { <-a.readSem }()

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.maxRequestBytes))
	if err != nil {
		// Over the cap or truncated. Acked: the payload is unusable and unverifiable
		// either way, and a non-200 would trade this delivery for a retry storm.
		a.log().Warn("bitbucket: discarding webhook with unreadable body",
			"error", err, "cap", a.maxRequestBytes, "read", len(payload), "content_length", r.ContentLength)
		w.WriteHeader(http.StatusOK)
		return
	}

	if !a.validSignature(r, payload) {
		a.log().Warn("bitbucket: inbound request rejected with 401", "reason", "X-Hub-Signature mismatch")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	key := r.Header.Get(eventKeyHeader)
	switch a.fl.category(key) {
	case catComment:
		a.handleComment(dispatchCtx, w, key, payload, deps)
	case catPullRequest:
		a.handlePullRequest(dispatchCtx, w, key, payload)
	case catPush:
		a.handlePush(dispatchCtx, w, key, payload)
	default:
		w.WriteHeader(http.StatusOK) // ping, comment edits and other unhandled keys are not errors
	}
}

// validSignature constant-time-compares the X-Hub-Signature HMAC-SHA256 of the
// raw body against one computed with the configured secret. A missing, malformed
// (wrong prefix, non-hex) or wrong signature all return false. hmac.Equal is
// constant-time over equal-length inputs, so a wrong signature of the right
// length cannot be recovered byte by byte from response timing.
func (a *adapter) validSignature(r *http.Request, body []byte) bool {
	sig := r.Header.Get(signatureHeader)
	if !strings.HasPrefix(sig, signaturePrefix) {
		return false
	}
	want, err := hex.DecodeString(sig[len(signaturePrefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.Secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// handleComment maps a comment delivery and dispatches it, dropping the bot's own
// comments (the reply-loop guard, keyed on the actor identity carried as the
// message UserID). An unparseable body is authentic-but-unusable, so it is acked
// and warned rather than rejected.
func (a *adapter) handleComment(dispatchCtx context.Context, w http.ResponseWriter, key string, payload []byte, deps core.AdapterDeps) {
	msg, ok := a.fl.parseComment(key, payload)
	if !ok {
		a.log().Warn("bitbucket: discarding comment webhook with unparseable body", "event", key)
		w.WriteHeader(http.StatusOK)
		return
	}
	if a.isSelf(msg.UserID) {
		w.WriteHeader(http.StatusOK) // the bot's own comment: reply-loop guard
		return
	}
	a.ackAndDispatch(w, func() { deps.Dispatch(dispatchCtx, msg) })
}

// handlePullRequest routes a reviewable PR delivery to cfg.OnPullRequest. Parsing
// happens only when a callback is set, so a nil callback costs the same as an
// unhandled key. A delivery the bot itself triggered is dropped (the comment
// path's reply-loop filter, applied to the event's actor).
func (a *adapter) handlePullRequest(dispatchCtx context.Context, w http.ResponseWriter, key string, payload []byte) {
	callback := a.cfg.OnPullRequest
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	event, authorID, ok := a.fl.parsePullRequest(key, payload)
	if !ok {
		a.log().Warn("bitbucket: discarding pull-request webhook with unparseable body", "event", key)
		w.WriteHeader(http.StatusOK)
		return
	}
	if a.isSelf(authorID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	a.ackAndDispatch(w, func() { callback(dispatchCtx, event) })
}

// handlePush routes a push delivery to cfg.OnPush, unfiltered — ref filtering is
// the callback's job.
func (a *adapter) handlePush(dispatchCtx context.Context, w http.ResponseWriter, key string, payload []byte) {
	callback := a.cfg.OnPush
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := a.fl.parsePush(key, payload)
	if !ok {
		a.log().Warn("bitbucket: discarding push webhook with unparseable body", "event", key)
		w.WriteHeader(http.StatusOK)
		return
	}
	a.ackAndDispatch(w, func() { callback(dispatchCtx, event) })
}

// ackAndDispatch bounds concurrent dispatch with a counting semaphore. It attempts
// a non-blocking acquire BEFORE acking: on success it acks 200 and runs f on a
// tracked goroutine; on saturation it acks 200 without dispatching rather than
// spawning unbounded goroutines. Shedding with 5xx would not save the delivery —
// Bitbucket does not re-deliver a success — and would trigger its retry storm.
func (a *adapter) ackAndDispatch(w http.ResponseWriter, f func()) {
	// Snapshot this connection's semaphore once: Connect installs a fresh one per
	// connection, and the acquire here and the release in the goroutine must target
	// the SAME channel even if a reconnect swaps a.sem.
	a.mu.Lock()
	sem := a.sem
	a.mu.Unlock()
	select {
	case sem <- struct{}{}:
	default:
		a.log().Warn("bitbucket: dispatch concurrency limit reached; acking and dropping delivery", "limit", maxConcurrentDispatch)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	a.dispatchAsync(func() {
		defer func() { <-sem }()
		f()
	})
}

// dispatchAsync runs f on a dispatch goroutine tracked by the inflight counter.
// Callers pass work bound to the detached context so an acked reply finishes
// during the shutdown drain; the increment lands before Shutdown returns, so
// drainDispatch always observes it.
func (a *adapter) dispatchAsync(f func()) {
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		f()
	}()
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the shutdown drain, and
	// WithCancel lets Disconnect abort stragglers after it.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		// Bitbucket webhooks are always POST; there is no GET handshake.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.handleWebhook(detachedCtx, w, r, deps)
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
	// Fresh per-connection dispatch semaphore: a slot a hung handler never releases
	// dies with this connection instead of leaking across reconnects.
	a.sem = make(chan struct{}, maxConcurrentDispatch)
	a.mu.Unlock()

	go func() {
		// Self-identity resolves here, off the non-blocking Connect path. On Cloud in
		// API-token mode Config.Self is empty and resolved via whoami; a bot that
		// cannot recognize its own comments is a reply-loop hazard, so failure is
		// fatal via deps.Done. In access-token mode and on Data Center Config.Self is
		// required and already set, so no probe runs. Resolving before Serve means no
		// request is handled before the bot can recognize itself.
		if a.cfg.Self == "" {
			probeCtx, cancel := context.WithTimeout(ctx, a.selfResolveBudget)
			self, resolveErr := a.fl.resolveSelf(probeCtx)
			cancel()
			// An empty identity with no error (e.g. whoami returns a blank uuid)
			// is as dangerous as a failure: isSelf would never match, silently
			// disabling the reply-loop guard. Treat it as fatal, like GitLab does.
			if resolveErr == nil && self == "" {
				resolveErr = fmt.Errorf("%w: whoami returned an empty identity", ErrMissingConfig)
			}
			if resolveErr != nil {
				_ = ln.Close()        // Serve never runs, so nothing else closes it
				if ctx.Err() == nil { // a canceled startup is a shutdown, not a failure
					deps.Done(resolveErr)
				}
				return
			}
			a.mu.Lock()
			a.self = self
			a.mu.Unlock()
		}
		serve(srv, ln, deps.Done)
	}()

	// Tear down when the run context is canceled; identity-compare so a stale
	// watcher from a superseded connection never tears down its replacement.
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

// serve runs srv on ln and reports any unexpected termination — anything but the
// ErrServerClosed of a clean Shutdown — to done.
func serve(srv *http.Server, ln net.Listener, done func(error)) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		done(err)
	}
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	cancelDispatch := a.detachedCancel
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	// Shutdown and drain each get their own budget: dispatch goroutines run outside
	// the HTTP handler lifecycle, so a slow Shutdown must not consume the drain
	// deadline and drop an already-acked in-flight message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), a.drainBudget)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	var drainErr error
	if n := a.inflight.Load(); n > 0 {
		a.log().Warn("bitbucket: drain deadline reached; canceling in-flight dispatches", "inflight", n)
		drainErr = fmt.Errorf("bitbucket: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv). Either way, cancel THIS connection's
	// detached context after the drain so a stuck handler cannot leak past
	// shutdown. self persists: it is re-resolved on the next Connect and harmless
	// while no server is up.
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

// drainDispatch waits, bounded by ctx, for in-flight dispatch goroutines so an
// acked message is processed rather than dropped at shutdown. It polls an atomic
// counter rather than a WaitGroup: an Add racing Wait would risk a misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
