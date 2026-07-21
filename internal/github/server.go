package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter/internal/core"
)

const signatureHeader = "X-Hub-Signature-256"

// handleWebhook authenticates, filters, acks and dispatches one webhook
// delivery. The ack (200) is written before dispatch runs: GitHub times out
// slow deliveries and disables hooks that fail persistently, so dropped and
// invalid-but-authentic payloads are acked too.
func (a *adapter) handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	// Read then verify as two steps with two distinct failure codes (the
	// sibling-adapter pattern): a body we cannot read is the client's 400; a
	// body that fails HMAC is a 403. The one-shot ValidatePayload cannot
	// distinguish the two.
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := gogithub.ValidateSignature(r.Header.Get(signatureHeader), payload, []byte(a.cfg.WebhookSecret)); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// GitHub's default webhook content type wraps the JSON in a payload= form
	// field; the signature above covers the raw form-encoded body, so unwrap
	// only after it verifies.
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(payload))
		if err != nil {
			a.log().Warn("github: discarding webhook with unparseable form body", "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		payload = []byte(form.Get("payload"))
	}

	switch gogithub.WebHookType(r) {
	case "issue_comment":
		a.handleIssueComment(dispatchCtx, w, payload, deps)
	case "pull_request":
		a.handlePullRequest(dispatchCtx, w, payload)
	case "push":
		a.handlePush(dispatchCtx, w, payload)
	default:
		w.WriteHeader(http.StatusOK) // ping and other subscribed events are not errors
	}
}

func (a *adapter) handleIssueComment(dispatchCtx context.Context, w http.ResponseWriter, payload []byte, deps core.AdapterDeps) {
	parsed, err := gogithub.ParseWebHook("issue_comment", payload)
	if err != nil {
		a.log().Warn("github: discarding webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogithub.IssueCommentEvent)
	if !ok || event.GetAction() != "created" || a.isSelfOrBot(event) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	a.dispatchAsync(func() { deps.Dispatch(dispatchCtx, toMessage(event)) })
}

// handlePullRequest routes pull_request deliveries to cfg.OnPullRequest.
// Parsing happens only when a callback is set, so a nil callback costs the
// same as an unknown event type. Only actions that create or change the PR's
// reviewable content pass, and the comment path's reply-loop filter applies
// to the PR author.
func (a *adapter) handlePullRequest(dispatchCtx context.Context, w http.ResponseWriter, payload []byte) {
	callback := a.cfg.OnPullRequest
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	parsed, err := gogithub.ParseWebHook("pull_request", payload)
	if err != nil {
		a.log().Warn("github: discarding webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogithub.PullRequestEvent)
	if !ok || !reviewableAction(event.GetAction()) || a.isSelfOrBotUser(event.GetPullRequest().GetUser()) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	a.dispatchAsync(func() { callback(dispatchCtx, event) })
}

// reviewableAction reports whether a pull_request action creates or changes
// the PR's reviewable content ("synchronize" fires when the head branch gets
// new commits).
func reviewableAction(action string) bool {
	return action == "opened" || action == "reopened" || action == "synchronize"
}

// handlePush routes push deliveries to cfg.OnPush, unfiltered — ref filtering
// is the callback's job.
func (a *adapter) handlePush(dispatchCtx context.Context, w http.ResponseWriter, payload []byte) {
	callback := a.cfg.OnPush
	if callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	parsed, err := gogithub.ParseWebHook("push", payload)
	if err != nil {
		a.log().Warn("github: discarding webhook with unparseable body", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogithub.PushEvent)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	a.dispatchAsync(func() { callback(dispatchCtx, event) })
}

// dispatchAsync runs f on a dispatch goroutine tracked by the inflight
// counter. Callers pass work bound to the detached context: core cancels
// runCtx *before* Disconnect's drain waits for the handler, so work threaded
// onto runCtx would fail mid-drain. The increment lands before Shutdown
// returns, so drainDispatch always observes it.
func (a *adapter) dispatchAsync(f func()) {
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		f()
	}()
}

// isSelfOrBot reports whether the comment author is any GitHub App bot (covers
// this bot in App mode and silences other bots wholesale, like the Slack and
// Discord adapters) or this bot's own account (the check that matters in PAT
// mode, where its comments arrive as a plain User).
func (a *adapter) isSelfOrBot(event *gogithub.IssueCommentEvent) bool {
	return a.isSelfOrBotUser(event.GetComment().GetUser())
}

// isSelfOrBotUser is the user-level check behind isSelfOrBot, shared with the
// reaction poller so both ingress paths drop the same authors.
func (a *adapter) isSelfOrBotUser(user *gogithub.User) bool {
	if user.GetType() == "Bot" {
		return true
	}
	a.mu.Lock()
	selfID := a.selfID
	a.mu.Unlock()
	return selfID != 0 && user.GetID() == selfID
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the shutdown drain, and
	// WithCancel lets Disconnect abort stragglers after it.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))

	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		// GitHub webhooks are always POST; there is no GET handshake.
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
	a.mu.Unlock()

	// Snapshot the reaction cutoff now, not when the poller goroutine starts:
	// the poller waits behind self-identity resolution (a GitHub round-trip),
	// and the documented contract is "only reactions added while connected".
	reactionCutoff := time.Now()

	go func() {
		// Self-identity resolves here, off the Connect path: adapter.Connect
		// is non-blocking by contract (core holds the bot lock across it), so
		// it must not wait on a GitHub round-trip. Resolving before Serve
		// means no request is handled before the bot can recognize itself. A
		// bot that cannot recognize itself is a reply-loop hazard, so failure
		// is fatal via deps.Done rather than silent at dispatch. Only PAT
		// mode needs the lookup: App comments arrive as type "Bot" and
		// isSelfOrBot drops them wholesale.
		if a.cfg.Token != "" {
			selfID, selfLogin, err := a.resolveSelf(ctx)
			if err != nil {
				_ = ln.Close()        // Serve never runs, so nothing else closes it
				if ctx.Err() == nil { // a canceled startup is a shutdown, not a failure
					deps.Done(err)
				}
				return
			}
			a.mu.Lock()
			a.selfID = selfID
			a.selfLogin = selfLogin
			a.mu.Unlock()
		}
		// The reaction poller starts after self-identity resolves so its very
		// first cycle can filter the bot's own reactions, and only when repos
		// are configured AND an OnReaction handler is registered — polling
		// costs API requests every cycle, so a bot nobody is listening on
		// must not pay for it. It stops with ctx like the teardown watcher below;
		// its dispatches ride detachedCtx + inflight, so Disconnect's drain
		// covers reaction handlers like webhook dispatch (see pollReactions
		// for the one caveat).
		if a.pollingConfigured() && deps.HasReactionHandlers {
			go a.pollReactions(ctx, detachedCtx, deps, reactionCutoff)
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

// serve runs srv on ln and reports any unexpected termination — anything but
// the ErrServerClosed of a clean Shutdown — to done. Split out so the error
// filter is unit-testable with a failing listener (mirrors the Teams adapter).
func serve(srv *http.Server, ln net.Listener, done func(error)) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		done(err)
	}
}

// resolveSelf resolves the bot's own account (ID and login) for reply-loop
// prevention in PAT mode, where its comments arrive as a plain User; the
// login also routes wildcard repo discovery of the bot's own owner through
// the authenticated-user listing. App mode needs no lookup: its comments
// carry user type "Bot", which isSelfOrBot drops without ever consulting
// selfID, and its discovery lists the installation's repos.
func (a *adapter) resolveSelf(ctx context.Context) (int64, string, error) {
	user, _, err := a.client.Users.Get(ctx, "")
	if err != nil {
		return 0, "", fmt.Errorf("github: resolve self identity: %w", err)
	}
	return user.GetID(), user.GetLogin(), nil
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
	// outside the HTTP handler lifecycle, so a slow Shutdown must not consume
	// the drain deadline and drop an already-acked in-flight message.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutCancel()
	err := srv.Shutdown(shutCtx)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	a.drainDispatch(drainCtx)

	var drainErr error
	if n := a.inflight.Load(); n > 0 {
		a.log().Warn("github: drain deadline reached; canceling in-flight dispatches", "inflight", n)
		drainErr = fmt.Errorf("github: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv). Either way, cancel THIS
	// connection's detached context after the drain so a stuck handler cannot
	// leak past shutdown. selfID persists: it is re-resolved on the next
	// PAT-mode Connect and harmless while no server is up.
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
// acked message is processed rather than dropped at shutdown. It polls an
// atomic counter rather than a WaitGroup: an Add racing Wait would risk a
// misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Attachments implements core.Adapter. GitHub issue comments carry markdown,
// not an upload channel worth modeling; v1 has no attachment support.
func (a *adapter) Attachments(_ *core.Message) ([]core.Attachment, error) {
	return nil, nil
}
