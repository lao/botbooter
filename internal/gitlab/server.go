package gitlab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
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
// is written before dispatch runs, and every authentic delivery is acked even
// when it is dropped, unreadable or shed under load, because GitLab never
// re-delivers a failed webhook and counts consecutive failures — a 4xx, a 5xx, a
// timeout and any other HTTP error alike — toward auto-disabling the hook, first
// temporarily and then, if they keep coming, permanently (the current thresholds
// are in _docs/platforms.md, not repeated here — they are GitLab's, and they
// move). A non-200 would therefore lose the delivery *and* suppress later ones,
// so 401 on a bad token is the only failure worth reporting — a wrong secret
// should stop the deliveries.
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
		a.log().Warn("gitlab: inbound concurrency limit reached; acking and dropping delivery", "limit", maxConcurrentReads)
		w.WriteHeader(http.StatusOK)
		return
	}
	defer func() { <-a.readSem }()

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.maxRequestBytes))
	if err != nil {
		// The body is over a.maxRequestBytes or the delivery was truncated. Both
		// are acked: the payload is unusable either way, and a 4xx would trade one
		// lost delivery for the bot's whole ingress (see handleWebhook's doc).
		// Log the sizes alongside the cap: read == cap means the body was over it,
		// read < cap means the delivery was cut short, and content_length (-1 when
		// the delivery is chunked) names how big it claimed to be.
		a.log().Warn("gitlab: discarding webhook with unreadable body",
			"error", err, "cap", a.maxRequestBytes, "read", len(payload), "content_length", r.ContentLength)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch gogitlab.HookEventType(r) {
	case gogitlab.EventTypeNote:
		a.handleNote(dispatchCtx, w, payload, deps, false)
	case gogitlab.EventConfidentialNote:
		// GitLab routes a note to the separate "Confidential Note Hook" trigger
		// when the note is internal OR its noteable is confidential, so this
		// trigger carries both comments on confidential issues (dispatched) and
		// internal notes on visible ones (dropped — see handleNote). The payload is
		// shaped identically and client-go parses both event types through the same
		// case, so the same handler routes it.
		a.handleNote(dispatchCtx, w, payload, deps, true)
	case gogitlab.EventTypeMergeRequest:
		a.handleMerge(dispatchCtx, w, payload)
	case gogitlab.EventTypePush:
		a.handlePush(dispatchCtx, w, payload)
	default:
		w.WriteHeader(http.StatusOK) // ping and other enabled triggers are not errors
	}
}

// validToken constant-time-compares the X-Gitlab-Token header against the
// configured secret, so a wrong header of the right length cannot be recovered
// byte by byte from response timing. ConstantTimeCompare short-circuits to 0 on
// a length mismatch — that comparison is not constant-time, so the secret's
// length remains observable; only its bytes are protected. An empty or absent
// header takes that path and is rejected.
func (a *adapter) validToken(r *http.Request) bool {
	got := r.Header.Get(tokenHeader)
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.Secret)) == 1
}

// handleNote routes a Note Hook. client-go resolves the noteable type into a
// distinct event, so issue and merge-request comments dispatch as messages while
// commit and snippet comments (no reply target in v1) are acked and dropped. A
// payload that cannot be parsed, or an unexpected noteable type, is acked too —
// the delivery is authentic, and a 4xx would make GitLab mark the hook failing.
//
// confidentialTrigger reports that the delivery arrived on the Confidential Note
// Hook. GitLab picks that trigger when the note is internal *or* its noteable is
// confidential, and Send can only create a plain note, so answering an internal
// note would publish the internal thread. Internal notes are therefore dropped by
// two filters: the note's own internal flag (side-parsed by internalNote — GitLab
// sends it, client-go's typed events do not expose it), and the noteable's
// confidentiality as the fallback for a payload that omits the flag, where a
// confidential-trigger note on a non-confidential noteable is internal by
// elimination. A note on a confidential issue is dispatched — the reply inherits
// the issue's restricted audience.
func (a *adapter) handleNote(dispatchCtx context.Context, w http.ResponseWriter, payload []byte, deps core.AdapterDeps, confidentialTrigger bool) {
	parsed, err := gogitlab.ParseWebhook(gogitlab.EventTypeNote, payload)
	if err != nil {
		a.log().Warn("gitlab: discarding note webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	flagged := internalNote(payload)
	switch event := parsed.(type) {
	case *gogitlab.IssueCommentEvent:
		oa := event.ObjectAttributes
		if a.dropComment(oa.System, flagged || (confidentialTrigger && !event.Issue.Confidential), oa.Action, oa.AuthorID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		a.ackAndDispatch(w, func() { deps.Dispatch(dispatchCtx, toMessageFromIssueComment(event)) })
	case *gogitlab.MergeCommentEvent:
		oa := event.ObjectAttributes
		// A merge request has no confidentiality of its own, so the confidential
		// trigger can only mean the note itself is internal.
		if a.dropComment(oa.System, flagged || confidentialTrigger, oa.Action, oa.AuthorID) {
			w.WriteHeader(http.StatusOK)
			return
		}
		a.ackAndDispatch(w, func() { deps.Dispatch(dispatchCtx, toMessageFromMergeComment(event)) })
	default:
		w.WriteHeader(http.StatusOK) // commit/snippet comment: no reply target in v1
	}
}

// internalNote reports the note's own object_attributes.internal flag. GitLab
// ships it in every note delivery (it is in the hook builder's safe-attribute
// list), but client-go's typed note events do not expose the field, so it is
// side-parsed off the raw payload — which handleNote has already parsed once, so
// a failure here means the flag is simply not there.
//
// An absent or unparseable flag reads false and leaves handleNote's noteable
// discriminator in charge, so an instance whose payload predates the field keeps
// exactly the behaviour it had before the flag was read.
func internalNote(payload []byte) bool {
	var note struct {
		ObjectAttributes struct {
			Internal bool `json:"internal"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(payload, &note); err != nil {
		return false
	}
	return note.ObjectAttributes.Internal
}

// dropComment reports whether a note must be ignored: a system note (an
// automated change like a label or title edit — never a real message), an
// internal note the reply path cannot answer in kind (see handleNote), an
// explicit edit (an action other than "create"), or one the bot itself authored
// (the reply-loop guard, by author id since GitLab note webhooks carry no
// reliable bot flag).
//
// object_attributes.action was only added to note deliveries in GitLab 16.11, so
// an older or self-hosted instance (which Config.BaseURL explicitly targets)
// sends new-note hooks with an empty action. An empty action is therefore
// treated as a create — dropping only a *known* non-create action — so the core
// comment path is not silently blackholed on those instances. The system, self
// and internal filters still apply regardless.
func (a *adapter) dropComment(system, internal bool, action gogitlab.CommentEventAction, authorID int64) bool {
	return system || internal || (action != "" && action != gogitlab.CommentEventActionCreate) || a.isSelfUser(authorID)
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
// slot when f returns; on saturation it logs and acks 200 without dispatching,
// rather than spawning unbounded goroutines. Shedding with 503 would not save the
// delivery — GitLab does not re-deliver it — and would push the hook toward
// being auto-disabled, taking later deliveries with it.
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
		a.log().Warn("gitlab: dispatch concurrency limit reached; acking and dropping delivery", "limit", maxConcurrentDispatch)
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
		//
		// The probe carries its own selfResolveBudget bound derived from ctx, so an
		// unresponsive endpoint fails fast via Done instead of leaving the listener
		// bound with Serve never started. cancel runs before serve (not deferred) so
		// the timer is released rather than held for the server's lifetime.
		probeCtx, cancel := context.WithTimeout(ctx, a.selfResolveBudget)
		selfID, err := a.resolveSelf(probeCtx)
		cancel()
		if err != nil {
			_ = ln.Close()        // Serve never runs, so nothing else closes it
			if ctx.Err() == nil { // a canceled startup is a shutdown, not a failure
				deps.Done(err)
			}
			return
		}
		a.mu.Lock()
		a.selfID = selfID
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

// resolveSelf resolves the id of the bot's own account for reply-loop
// prevention. GitLab note webhooks carry no reliable bot flag, so the bot's own
// notes are recognized by author id; the token's user is that account for a
// personal, project or group access token alike.
func (a *adapter) resolveSelf(ctx context.Context) (int64, error) {
	user, _, err := a.client.Users.CurrentUser(gogitlab.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("gitlab: resolve self identity: %w", err)
	}
	return user.ID, nil
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

	drainCtx, drainCancel := context.WithTimeout(context.Background(), a.drainBudget)
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
