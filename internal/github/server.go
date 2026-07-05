package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
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

	if gogithub.WebHookType(r) != "issue_comment" {
		w.WriteHeader(http.StatusOK) // ping and other subscribed events are not errors
		return
	}
	parsed, err := gogithub.ParseWebHook("issue_comment", payload)
	if err != nil {
		log.Printf("github: discarding webhook with unparseable body: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	event, ok := parsed.(*gogithub.IssueCommentEvent)
	if !ok || event.GetAction() != "created" || a.isSelfOrBot(event) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	// Dispatch on the detached context: core cancels runCtx *before*
	// Disconnect's drain waits for this handler, so a reply threaded onto
	// runCtx would fail mid-drain. The increment lands before Shutdown
	// returns, so drainDispatch always observes it.
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		deps.Dispatch(dispatchCtx, toMessage(event))
	}()
}

// isSelfOrBot reports whether the comment author is any GitHub App bot (covers
// this bot in App mode and silences other bots wholesale, like the Slack and
// Discord adapters) or this bot's own account (the check that matters in PAT
// mode, where its comments arrive as a plain User).
func (a *adapter) isSelfOrBot(event *gogithub.IssueCommentEvent) bool {
	user := event.GetComment().GetUser()
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

	// A bot that cannot recognize itself is a reply-loop hazard: fail loudly
	// at startup, in either auth mode, rather than silently at dispatch.
	selfID, selfLogin, err := a.resolveSelf(ctx)
	if err != nil {
		detachedCancel()
		return err
	}

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
	a.selfID, a.selfLogin = selfID, selfLogin
	a.srv = srv
	a.boundAddr = ln.Addr().String()
	a.detachedCancel = detachedCancel
	a.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Done(err)
		}
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

// resolveSelf resolves the bot's own account for loop prevention. PAT mode is
// one call; App mode cannot call GET /user with an installation token, so it
// asks GET /app (App JWT) for the slug, then resolves "<slug>[bot]" to an id.
func (a *adapter) resolveSelf(ctx context.Context) (int64, string, error) {
	if a.cfg.Token != "" {
		user, _, err := a.client.Users.Get(ctx, "")
		if err != nil {
			return 0, "", fmt.Errorf("github: resolve self identity: %w", err)
		}
		return user.GetID(), user.GetLogin(), nil
	}

	atr, err := ghinstallation.NewAppsTransport(a.baseTransport, a.cfg.AppID, a.cfg.PrivateKey)
	if err != nil {
		return 0, "", fmt.Errorf("github: build app transport: %w", err)
	}
	opts := []gogithub.ClientOptionsFunc{gogithub.WithHTTPClient(
		&http.Client{Transport: atr, Timeout: a.cfg.HTTPClient.Timeout},
	)}
	if a.baseURL != "" { // test hook: point the one-shot client at a fake API
		opts = append(opts, gogithub.WithURLs(gogithub.Ptr(a.baseURL+"/"), gogithub.Ptr(a.baseURL+"/")))
	}
	appClient, err := gogithub.NewClient(opts...)
	if err != nil {
		return 0, "", fmt.Errorf("github: build app client: %w", err)
	}

	app, _, err := appClient.Apps.Get(ctx, "")
	if err != nil {
		return 0, "", fmt.Errorf("github: resolve app slug: %w", err)
	}
	user, _, err := a.client.Users.Get(ctx, app.GetSlug()+"[bot]")
	if err != nil {
		return 0, "", fmt.Errorf("github: resolve bot user %s[bot]: %w", app.GetSlug(), err)
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
		log.Printf("github: drain deadline reached; canceling %d in-flight dispatch(es)", n)
		drainErr = fmt.Errorf("github: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared fields only if a reconnect has not installed a newer
	// connection (identity-compare on srv). Either way, cancel THIS
	// connection's detached context after the drain so a stuck handler cannot
	// leak past shutdown. selfID/selfLogin persist: they are re-resolved on
	// the next Connect and harmless while no server is up.
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
