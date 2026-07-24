package gitlab

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/lao/botbooter/internal/core"
)

// tokenHeader carries the webhook's configured "Secret token"; GitLab
// authenticates a delivery by sending it verbatim, so the adapter compares it
// against Config.Secret rather than verifying a body signature.
const tokenHeader = "X-Gitlab-Token" //nolint:gosec // G101: HTTP header name, not a credential

// handleWebhook authenticates, reads, filters, acks and dispatches one webhook
// delivery. The token is checked from the header BEFORE the body is read, so an
// unauthenticated request is rejected without buffering anything. The ack (200)
// is written before dispatch runs: GitLab retries slow deliveries and disables
// hooks that fail persistently, so dropped and invalid-but-authentic payloads
// are acked too.
func (a *adapter) handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	if !a.validToken(r) {
		// Log the rejection so an operator can diagnose a secret-token mismatch
		// (mirrors the Teams sibling's 401 log). The log names only the failure,
		// never the token.
		a.log().Warn("gitlab: inbound request rejected with 401", "reason", "X-Gitlab-Token mismatch")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Bound concurrent inbound processing before reading the (up to
	// a.maxRequestBytes) body, so an authenticated flood of large POSTs cannot
	// buffer unbounded memory. Held only across read+parse (this function); the
	// dispatch goroutine has its own bound (ackAndDispatch). a.readSem is
	// allocated once in newAdapter and never reassigned, so this direct read
	// races nothing.
	select {
	case a.readSem <- struct{}{}:
	default:
		a.log().Warn("gitlab: inbound concurrency limit reached; shedding with 503", "limit", maxConcurrentReads)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer func() { <-a.readSem }()

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch gogitlab.HookEventType(r) {
	case gogitlab.EventTypeNote, gogitlab.EventConfidentialNote:
		// GitLab delivers comments on confidential issues/MRs under a separate
		// "Confidential Note Hook" trigger; the payload is shaped identically and
		// client-go parses both through the same case, so route them together.
		a.handleNote(dispatchCtx, w, payload, deps)
	case gogitlab.EventTypeMergeRequest:
		a.handleMerge(dispatchCtx, w, payload)
	case gogitlab.EventTypePush:
		a.handlePush(dispatchCtx, w, payload)
	default:
		w.WriteHeader(http.StatusOK) // ping and other enabled triggers are not errors
	}
}

// validToken constant-time-compares the X-Gitlab-Token header against the
// configured secret. ConstantTimeCompare returns 0 on a length mismatch, so an
// empty or absent header is rejected without leaking the secret's length via
// timing.
func (a *adapter) validToken(r *http.Request) bool {
	got := r.Header.Get(tokenHeader)
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.Secret)) == 1
}

// handleNote routes a Note Hook. client-go resolves the noteable type into a
// distinct event, so issue and merge-request comments dispatch as messages while
// commit and snippet comments (no reply target in v1) are acked and dropped. A
// payload that cannot be parsed, or an unexpected noteable type, is acked too —
// the delivery is authentic, and a 4xx would make GitLab mark the hook failing.
func (a *adapter) handleNote(dispatchCtx context.Context, w http.ResponseWriter, payload []byte, deps core.AdapterDeps) {
	parsed, err := gogitlab.ParseWebhook(gogitlab.EventTypeNote, payload)
	if err != nil {
		a.log().Warn("gitlab: discarding note webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	switch event := parsed.(type) {
	case *gogitlab.IssueCommentEvent:
		oa := event.ObjectAttributes
		if a.dropComment(oa.System, oa.Action, oa.AuthorID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		a.ackAndDispatch(w, func() { deps.Dispatch(dispatchCtx, toMessageFromIssueComment(event)) })
	case *gogitlab.MergeCommentEvent:
		oa := event.ObjectAttributes
		if a.dropComment(oa.System, oa.Action, oa.AuthorID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		a.ackAndDispatch(w, func() { deps.Dispatch(dispatchCtx, toMessageFromMergeComment(event)) })
	default:
		w.WriteHeader(http.StatusOK) // commit/snippet comment: no reply target in v1
	}
}

// dropComment reports whether a note must be ignored: a system note (an
// automated change like a label or title edit — never a real message), an edit
// rather than a new note (only the "create" action is a fresh message), or one
// the bot itself authored (the reply-loop guard, by author id since GitLab note
// webhooks carry no reliable bot flag).
func (a *adapter) dropComment(system bool, action gogitlab.CommentEventAction, authorID int64) bool {
	return system || action != gogitlab.CommentEventActionCreate || a.isSelfUser(authorID)
}

// handleMerge routes a Merge Request Hook to cfg.OnMergeRequest. Parsing happens
// only when a callback is set, so a nil callback costs the same as an unknown
// event. Only actions that create or change the MR's reviewable content pass,
// and an MR the bot itself authored is dropped (the comment path's reply-loop
// filter, applied to the MR author).
func (a *adapter) handleMerge(dispatchCtx context.Context, w http.ResponseWriter, payload []byte) {
	callback := a.cfg.OnMergeRequest
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	parsed, err := gogitlab.ParseWebhook(gogitlab.EventTypeMergeRequest, payload)
	if err != nil {
		a.log().Warn("gitlab: discarding merge-request webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogitlab.MergeEvent)
	if !ok || !reviewableAction(event.ObjectAttributes.Action) || a.isSelfUser(event.ObjectAttributes.AuthorID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	a.ackAndDispatch(w, func() { callback(dispatchCtx, event) })
}

// reviewableAction reports whether a Merge Request action creates or changes the
// MR's reviewable content. "update" fires when the source branch gets new
// commits (among other edits); the callback receives the full event and can
// narrow further (e.g. on ObjectAttributes.OldRev for a code push).
func reviewableAction(action string) bool {
	return action == "open" || action == "reopen" || action == "update"
}

// handlePush routes a Push Hook to cfg.OnPush, unfiltered — ref filtering is the
// callback's job.
func (a *adapter) handlePush(dispatchCtx context.Context, w http.ResponseWriter, payload []byte) {
	callback := a.cfg.OnPush
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	parsed, err := gogitlab.ParseWebhook(gogitlab.EventTypePush, payload)
	if err != nil {
		a.log().Warn("gitlab: discarding push webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogitlab.PushEvent)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	a.ackAndDispatch(w, func() { callback(dispatchCtx, event) })
}

// ackAndDispatch bounds concurrent webhook dispatch with a counting semaphore
// (maxConcurrentDispatch). It attempts a non-blocking acquire BEFORE acking: on
// success it acks 200, runs f on a tracked dispatch goroutine, and releases the
// slot when f returns; on saturation it answers 503 so GitLab retries the
// delivery later instead of the adapter spawning unbounded goroutines.
func (a *adapter) ackAndDispatch(w http.ResponseWriter, f func()) {
	// Snapshot this connection's semaphore once: Connect installs a fresh one per
	// connection, and the acquire below and the release in the dispatch goroutine
	// must target the SAME channel even if a later reconnect swaps a.sem.
	a.mu.Lock()
	sem := a.sem
	a.mu.Unlock()
	select {
	case sem <- struct{}{}:
	default:
		a.log().Warn("gitlab: dispatch concurrency limit reached; shedding with 503", "limit", maxConcurrentDispatch)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	a.dispatchAsync(func() {
		defer func() { <-sem }()
		f()
	})
}

// dispatchAsync runs f on a dispatch goroutine tracked by the inflight counter.
// Callers pass work bound to the detached context: core cancels runCtx *before*
// Disconnect's drain waits for the handler, so work threaded onto runCtx would
// fail mid-drain. The increment lands before Shutdown returns, so drainDispatch
// always observes it.
func (a *adapter) dispatchAsync(f func()) {
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		f()
	}()
}

// isSelfUser reports whether id is the bot's own resolved account, the reply-loop
// guard. Zero before self-identity resolves, so it never drops a real user.
func (a *adapter) isSelfUser(id int64) bool {
	a.mu.Lock()
	self := a.selfID
	a.mu.Unlock()
	return self != 0 && id == self
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the shutdown drain, and
	// WithCancel lets Disconnect abort stragglers after it.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		// GitLab webhooks are always POST; there is no GET handshake.
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
	// Fresh per-connection dispatch semaphore: a slot a hung handler never
	// releases dies with this connection instead of leaking across reconnects.
	a.sem = make(chan struct{}, maxConcurrentDispatch)
	a.mu.Unlock()

	go func() {
		// Self-identity resolves here, off the Connect path: adapter.Connect is
		// non-blocking by contract (core holds the bot lock across it), so it must
		// not wait on a GitLab round-trip. Resolving before Serve means no request
		// is handled before the bot can recognize its own notes. A bot that cannot
		// recognize itself is a reply-loop hazard, so failure is fatal via
		// deps.Done rather than silent at dispatch.
		selfID, selfUsername, err := a.resolveSelf(ctx)
		if err != nil {
			_ = ln.Close()        // Serve never runs, so nothing else closes it
			if ctx.Err() == nil { // a canceled startup is a shutdown, not a failure
				deps.Done(err)
			}
			return
		}
		a.mu.Lock()
		a.selfID = selfID
		a.selfUsername = selfUsername
		a.mu.Unlock()
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
// ErrServerClosed of a clean Shutdown — to done. Split out so the error filter is
// unit-testable with a failing listener (mirrors the sibling adapters).
func serve(srv *http.Server, ln net.Listener, done func(error)) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		done(err)
	}
}

// resolveSelf resolves the bot's own account (id and username) for reply-loop
// prevention. GitLab note webhooks carry no reliable bot flag, so the bot's own
// notes are recognized by author id; the token's user is that account for a
// personal, project or group access token alike.
func (a *adapter) resolveSelf(ctx context.Context) (int64, string, error) {
	user, _, err := a.client.Users.CurrentUser(gogitlab.WithContext(ctx))
	if err != nil {
		return 0, "", fmt.Errorf("gitlab: resolve self identity: %w", err)
	}
	return user.ID, user.Username, nil
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	cancelDispatch := a.detachedCancel
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	// Shutdown and drain each get their own budget: dispatch goroutines run
	// outside the HTTP handler lifecycle, so a slow Shutdown must not consume the
	// drain deadline and drop an already-acked in-flight message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	var drainErr error
	if n := a.inflight.Load(); n > 0 {
		a.log().Warn("gitlab: drain deadline reached; canceling in-flight dispatches", "inflight", n)
		drainErr = fmt.Errorf("gitlab: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv). Either way, cancel THIS connection's
	// detached context after the drain so a stuck handler cannot leak past
	// shutdown. selfID persists: it is re-resolved on the next Connect and
	// harmless while no server is up.
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

// Attachments implements core.Adapter. GitLab notes carry markdown, not an
// upload channel worth modeling; v1 has no attachment support.
func (a *adapter) Attachments(_ *core.Message) ([]core.Attachment, error) {
	return nil, nil
}
