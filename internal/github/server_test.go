package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

const (
	prCommentCreated = `{
  "action": "created",
  "issue": {"number": 7, "pull_request": {"url": "https://api.github.com/repos/lao/botbooter/pulls/7"}},
  "comment": {"id": 2, "body": "/retest", "created_at": "2026-07-03T11:00:00Z",
    "user": {"id": 99, "login": "reviewer", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"},
  "sender": {"id": 99, "login": "reviewer", "type": "User"}
}`
	commentEdited = `{
  "action": "edited",
  "issue": {"number": 42},
  "comment": {"id": 3, "body": "edited", "user": {"id": 99, "login": "reviewer", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	botAuthoredComment = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {"id": 4, "body": "I am an app", "user": {"id": 555, "login": "some-app[bot]", "type": "Bot"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	// PAT-shape self comment: type User, id matching the adapter's selfID.
	selfAuthoredComment = `{
  "action": "created",
  "issue": {"number": 42},
  "comment": {"id": 5, "body": "my own reply", "user": {"id": 777, "login": "bot-account", "type": "User"}},
  "repository": {"full_name": "lao/botbooter"}
}`
	pingEvent = `{"zen": "Design for failure.", "hook_id": 1}`
)

// failingListener makes Serve return err immediately, exercising the serve
// error filter without a real socket (mirrors the Teams adapter's test).
type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return testAddr("failing") }

type testAddr string

func (testAddr) Network() string  { return "test" }
func (a testAddr) String() string { return string(a) }

// sign returns the X-Hub-Signature-256 header value for body under secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookRequest builds a signed issue_comment POST for the handler tests.
func webhookRequest(secret, event, payload string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sign(secret, []byte(payload)))
	return r
}

func captureDeps(got *[]*core.Message, done chan struct{}) core.AdapterDeps {
	return core.AdapterDeps{Dispatch: func(_ context.Context, m *core.Message) {
		*got = append(*got, m)
		if done != nil {
			done <- struct{}{}
		}
	}}
}

func awaitDispatch(t *testing.T, done chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for dispatch")
		}
	}
}

func TestHandleWebhook_DispatchesComment(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", "issue_comment", issueCommentCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].ChannelID, "lao/botbooter#42", "channel id")
	asserts.Equal(t, got[0].Content, "/deploy staging", "content")
	raw, ok := RawEvent(got[0])
	asserts.True(t, ok, "raw event present")
	asserts.False(t, raw.Event.GetIssue().IsPullRequest(), "plain issue comment")
}

// GitHub's default webhook content type is application/x-www-form-urlencoded,
// which wraps the JSON in a payload= form field; the signature covers the raw
// form-encoded body. The adapter must accept both content types.
func TestHandleWebhook_FormEncodedPayload(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)
	w := httptest.NewRecorder()

	body := "payload=" + url.QueryEscape(issueCommentCreated)
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(body)))

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic form-encoded request should be 200")
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].Content, "/deploy staging", "content")
}

func TestHandleWebhook_PRComment(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "issue_comment", prCommentCreated), captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].ChannelID, "lao/botbooter#7", "PR channel id")
	raw, _ := RawEvent(got[0])
	asserts.True(t, raw.Event.GetIssue().IsPullRequest(), "PR comment detectable via raw event")
}

// The handler must dispatch on exactly the detached context passed in, not the
// request context — otherwise a reply would fail with "context canceled"
// mid-drain (same guard as the WhatsApp adapter).
func TestHandleWebhook_DispatchesOnDetachedCtx(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()
	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}

	a.handleWebhook(dispatchCtx, httptest.NewRecorder(), webhookRequest("hook-secret", "issue_comment", issueCommentCreated), deps)

	select {
	case c := <-gotCtx:
		asserts.NoError(t, c.Err(), "dispatch ctx live before cancel")
		cancelDispatch()
		asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch rides the passed detached ctx")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestHandleWebhook_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		request  func() *http.Request
		wantCode int
	}{
		{"BadSignature", func() *http.Request {
			r := webhookRequest("hook-secret", "issue_comment", issueCommentCreated)
			r.Header.Set("X-Hub-Signature-256", sign("wrong-secret", []byte(issueCommentCreated)))
			return r
		}, http.StatusForbidden},
		{"MissingSignature", func() *http.Request {
			r := webhookRequest("hook-secret", "issue_comment", issueCommentCreated)
			r.Header.Del("X-Hub-Signature-256")
			return r
		}, http.StatusForbidden},
		{"OversizedBody", func() *http.Request {
			big := strings.Repeat("a", maxRequestBytes+1)
			r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(big))
			r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(big)))
			return r
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(patConfig())
			asserts.NoError(t, err, "new adapter")
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, tc.request(), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, tc.wantCode, "status")
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

func selfIdentityServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"id": 777, "login": "bot-account"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// waitForSelfID polls until the serve goroutine has resolved the adapter's
// self identity; resolution is asynchronous by design (Connect is non-blocking).
func waitForSelfID(t *testing.T, a *adapter, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		id := a.selfID
		a.mu.Unlock()
		if id == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("self identity not resolved to %d in time", want)
}

func connectedAdapter(t *testing.T, deps core.AdapterDeps) (*adapter, *httptest.Server, context.CancelFunc) {
	t.Helper()
	srv := selfIdentityServer(t)
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	return a, srv, cancel
}

// GitHub webhooks are always POST; anything else is rejected before the
// signature check ever runs.
func TestConnect_NonPostMethodRejected(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	resp, err := http.Get("http://" + addr + "/webhook")
	asserts.NoError(t, err, "GET webhook")
	_ = resp.Body.Close()
	asserts.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed, "non-POST is 405")
}

func TestConnect_ListenError(t *testing.T) {
	// Occupy a port so Connect's Listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "occupy port")
	defer func() { _ = ln.Close() }()

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.cfg.Addr = ln.Addr().String()

	err = a.Connect(context.Background(), core.AdapterDeps{})
	asserts.Error(t, err, "Connect must fail when the address is taken")
}

func TestConnect_ResolvesSelfAndBinds(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	boundAddr := a.boundAddr
	a.mu.Unlock()
	asserts.True(t, boundAddr != "", "listener bound, addr recoverable")
	waitForSelfID(t, a, 777) // resolved asynchronously via /user before serving
}

// A bot that cannot recognize itself is a reply-loop hazard: an identity
// failure must surface fatally via deps.Done (Connect itself stays
// non-blocking, so it cannot return the error synchronously).
func TestConnect_SelfIdentityFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	failed := make(chan error, 1)
	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done:       func(err error) { failed <- err },
		Disconnect: func() error { return nil },
	}), "Connect is non-blocking; the failure arrives via Done")
	defer func() { _ = a.Disconnect() }()

	select {
	case err := <-failed:
		asserts.Error(t, err, "a bot that cannot recognize itself must not serve")
	case <-time.After(2 * time.Second):
		t.Fatal("identity failure never surfaced via deps.Done")
	}

	// Serve never runs on this path, so the ln.Close in the serve goroutine is
	// the only thing that releases the listener — pin that it actually did.
	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections after identity failure")
	}
}

// Connect must return before the self-identity round-trip completes: core
// holds the bot lock across adapter.Connect and documents it as non-blocking,
// so a slow GitHub API must not stall Connect or a concurrent Disconnect.
func TestConnect_DoesNotBlockOnSelfResolution(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold /user until the test releases it
		_, _ = w.Write([]byte(`{"id": 777, "login": "bot-account"}`))
	}))
	defer srv.Close()
	defer unblock()
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	connected := make(chan error, 1)
	go func() {
		connected <- a.Connect(context.Background(), core.AdapterDeps{
			Done: func(error) {}, Disconnect: func() error { return nil },
		})
	}()
	select {
	case err := <-connected:
		asserts.NoError(t, err, "connect")
	case <-time.After(time.Second):
		t.Fatal("Connect blocked on the self-identity round-trip")
	}
	defer func() { _ = a.Disconnect() }()

	unblock()
	waitForSelfID(t, a, 777)
}

// App mode needs no self-identity lookup: every App-authored comment arrives
// with user type "Bot" and is dropped wholesale, so Connect must bind and
// serve without a single GitHub API round-trip. All outbound API traffic is
// rerouted to a counting server so a regression to calling resolveSelf (or any
// other Connect-path API call) trips the zero-requests assertion.
func TestConnect_AppModeSkipsSelfResolution(t *testing.T) {
	var apiCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := appConfig(t)
	cfg.HTTPClient = &http.Client{Transport: rewriteTransport{host: srv.Listener.Addr().String()}}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new App adapter")

	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	}), "connect in App mode")
	defer func() { _ = a.Disconnect() }()

	// Resolution would be asynchronous; give a regressed lookup a moment to fire.
	time.Sleep(50 * time.Millisecond)

	a.mu.Lock()
	selfID, boundAddr := a.selfID, a.boundAddr
	a.mu.Unlock()
	asserts.Equal(t, selfID, int64(0), "App mode relies on the Bot-type filter, not selfID")
	asserts.True(t, boundAddr != "", "listener bound")
	asserts.Equal(t, apiCalls.Load(), int64(0), "App-mode Connect must make no GitHub API round-trip")
}

// A non-default cfg.Path must actually route: the webhook lands on the custom
// route and the default route no longer exists.
func TestConnect_CustomPathRoutes(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()

	cfg := patConfig()
	cfg.Path = "/hooks/gh"
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	var got []*core.Message
	dispatched := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := captureDeps(&got, dispatched)
	deps.Done = func(error) {}
	deps.Disconnect = func() error { return nil }
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	post := func(path string) int {
		r, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(issueCommentCreated))
		asserts.NoError(t, err, "build request")
		r.Header.Set("X-GitHub-Event", "issue_comment")
		r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(issueCommentCreated)))
		resp, err := http.DefaultClient.Do(r)
		asserts.NoError(t, err, "deliver webhook")
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	asserts.Equal(t, post("/hooks/gh"), http.StatusOK, "custom path serves the webhook")
	awaitDispatch(t, dispatched, 1)
	asserts.Equal(t, len(got), 1, "delivery on the custom path dispatched")
	asserts.Equal(t, post("/webhook"), http.StatusNotFound, "default path is not registered")
}

func TestDisconnect_IdempotentAndClears(t *testing.T) {
	// A clean Disconnect must never report through deps.Done: Serve's
	// ErrServerClosed is the expected result of Shutdown, and serve's filter
	// (server.go) is what keeps it out of the done channel.
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done:       func(err error) { t.Errorf("deps.Done called on clean Disconnect: %v", err) },
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	waitForSelfID(t, a, 777) // settle async resolution before tearing down
	asserts.NoError(t, a.Disconnect(), "first disconnect")
	a.mu.Lock()
	cleared := a.srv == nil && a.boundAddr == "" && a.detachedCancel == nil
	stillSelf := a.selfID == 777
	a.mu.Unlock()
	asserts.True(t, cleared, "server state cleared")
	asserts.True(t, stillSelf, "self identity persists across Disconnect")
	asserts.NoError(t, a.Disconnect(), "second disconnect is a no-op")
}

func TestDisconnect_NeverConnected(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	asserts.NoError(t, a.Disconnect(), "disconnect before connect is nil")
}

func TestConnect_CtxCancelTriggersDisconnect(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { disconnected <- struct{}{}; return nil },
	})
	defer srv.Close()
	defer func() { _ = a.Disconnect() }()

	cancel()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not call deps.Disconnect on ctx cancel")
	}
}

// A stale watcher from a superseded connection must not tear down its
// replacement: connect, disconnect, reconnect, then cancel the FIRST ctx and
// assert deps.Disconnect is not called for the second connection.
func TestConnect_StaleWatcherIgnoresReplacedServer(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()
	disconnects := make(chan struct{}, 2)
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { disconnects <- struct{}{}; return nil },
	}

	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	asserts.NoError(t, a.Connect(ctx1, deps), "first connect")
	asserts.NoError(t, a.Disconnect(), "disconnect first connection")

	asserts.NoError(t, a.Connect(context.Background(), deps), "reconnect")
	defer func() { _ = a.Disconnect() }()

	cancel1()

	select {
	case <-disconnects:
		t.Fatal("stale watcher tore down the replacement connection")
	case <-time.After(200 * time.Millisecond):
		// No Disconnect call: the stale watcher saw a.srv != its srv and bailed.
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.True(t, a.srv != nil, "second connection still installed")
}

// Disconnect must cancel the connection's detached dispatch context after the
// drain, so a stuck handler cannot leak past shutdown.
func TestDisconnect_CancelsDetachedCtx(t *testing.T) {
	gotCtx := make(chan context.Context, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch:   func(c context.Context, _ *core.Message) { gotCtx <- c },
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	// Drive one real delivery through the bound server so dispatch runs on the
	// connection's detached context.
	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	r, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(issueCommentCreated))
	asserts.NoError(t, err, "build request")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(issueCommentCreated)))
	resp, err := http.DefaultClient.Do(r)
	asserts.NoError(t, err, "deliver webhook")
	_ = resp.Body.Close()

	var dispatchCtx context.Context
	select {
	case dispatchCtx = <-gotCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
	asserts.NoError(t, dispatchCtx.Err(), "detached ctx live while connected")

	asserts.NoError(t, a.Disconnect(), "disconnect")
	asserts.ErrorIs(t, dispatchCtx.Err(), context.Canceled, "detached ctx canceled by Disconnect")
}

// Disconnect must cancel the detached context only AFTER the drain window: the
// whole design ("an acked message is processed rather than dropped") collapses
// if the cancel lands first, because an in-flight handler would see a dead ctx
// mid-reply. A dispatch that is still running when Disconnect starts must
// observe a live context through to completion.
func TestDisconnect_DrainsBeforeCancelingDetachedCtx(t *testing.T) {
	entered := make(chan struct{}, 1)
	ctxErrAtEnd := make(chan error, 1)
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Dispatch: func(c context.Context, _ *core.Message) {
			entered <- struct{}{}
			time.Sleep(100 * time.Millisecond) // still in flight when Disconnect runs
			ctxErrAtEnd <- c.Err()
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

	// Drive one real delivery through the bound server so dispatch runs on the
	// connection's detached context.
	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	r, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(issueCommentCreated))
	asserts.NoError(t, err, "build request")
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(issueCommentCreated)))
	resp, err := http.DefaultClient.Do(r)
	asserts.NoError(t, err, "deliver webhook")
	_ = resp.Body.Close()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.NoError(t, a.Disconnect(), "disconnect during in-flight dispatch")

	select {
	case dispatchErr := <-ctxErrAtEnd:
		asserts.NoError(t, dispatchErr, "in-flight dispatch kept a live detached ctx until it finished (drain before cancel)")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to finish")
	}
}

// A signed but unparseable form-encoded body must be acked (the delivery is
// authentic — a 4xx would make GitHub mark the hook as failing) and dropped.
func TestHandleWebhook_UnparseableFormBody(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	var got []*core.Message
	w := httptest.NewRecorder()

	body := "payload=%zz" // invalid URL escape: ParseQuery fails
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", "issue_comment")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Hub-Signature-256", sign("hook-secret", []byte(body)))

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusOK, "authentic-but-unparseable form body still acks 200")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestServe_ReportsUnexpectedError(t *testing.T) {
	want := errors.New("accept failed")
	done := make(chan error, 1)

	serve(&http.Server{}, failingListener{err: want}, func(err error) {
		done <- err
	})

	select {
	case got := <-done:
		asserts.ErrorIs(t, got, want, "unexpected serve error should be reported")
	default:
		t.Fatal("serve returned without reporting the listener error")
	}
}

func TestAttachments_AlwaysNil(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	atts, err := a.Attachments(&core.Message{})
	asserts.NoError(t, err, "no error")
	asserts.True(t, atts == nil, "v1 has no attachments")
}

func TestHandleWebhook_AckedButDropped(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		prep    func(a *adapter)
	}{
		{"PingEvent", "ping", pingEvent, nil},
		{"EditedAction", "issue_comment", commentEdited, nil},
		{"BotAuthor", "issue_comment", botAuthoredComment, nil},
		{"SelfAuthor", "issue_comment", selfAuthoredComment, func(a *adapter) {
			a.mu.Lock()
			a.selfID = 777
			a.mu.Unlock()
		}},
		{"UnknownEvent", "workflow_run", `{"action": "completed"}`, nil},
		{"MalformedJSON", "issue_comment", `not json{`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := newAdapter(patConfig())
			asserts.NoError(t, err, "new adapter")
			if tc.prep != nil {
				tc.prep(a)
			}
			var got []*core.Message
			w := httptest.NewRecorder()

			a.handleWebhook(context.Background(), w, webhookRequest("hook-secret", tc.event, tc.payload), captureDeps(&got, nil))

			asserts.Equal(t, w.Code, http.StatusOK, "dropped events still ack 200")
			// Dispatch is async; a dropped event never increments inflight, so
			// give a stray dispatch a moment to appear before asserting.
			time.Sleep(20 * time.Millisecond)
			asserts.Equal(t, len(got), 0, "nothing dispatched")
		})
	}
}

func TestDrainDispatch_ContextCancel(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.inflight.Add(1) // never decremented
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.drainDispatch(ctx) // must return promptly on a canceled ctx
	asserts.Equal(t, a.inflight.Load(), int64(1), "drain returns on ctx cancel without waiting")
}

func TestDrainDispatch_WaitsForInflight(t *testing.T) {
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.inflight.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		a.inflight.Add(-1)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.drainDispatch(ctx)

	asserts.Equal(t, a.inflight.Load(), int64(0), "drain should wait until in-flight dispatch reaches zero")
}

// TestDisconnect_DrainTimeoutReturnsError guards that a drain which cannot finish
// within its deadline surfaces an error rather than masquerading as a clean
// shutdown: Disconnect force-cancels the straggler and must report it. The drain
// budget is a hardcoded 5s, so this takes ~5s of real time and is env-gated to
// keep the default suite fast (mirrors the slow-shutdown drain timing test).
func TestDisconnect_DrainTimeoutReturnsError(t *testing.T) {
	if os.Getenv("BOTBOOTER_GITHUB_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_GITHUB_DRAIN_TIMING_TEST to run the ~5s drain-timeout test")
	}
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	// Unstarted server: Shutdown returns immediately, so only the drain can time out.
	a.mu.Lock()
	a.srv = &http.Server{}
	a.detachedCancel = func() {}
	a.mu.Unlock()

	release := make(chan struct{})
	defer close(release)
	dispatched := make(chan struct{}, 1)
	deps := core.AdapterDeps{Dispatch: func(context.Context, *core.Message) {
		dispatched <- struct{}{}
		<-release // block past the 5s drain deadline
	}}

	a.handleWebhook(context.Background(), httptest.NewRecorder(), webhookRequest("hook-secret", "issue_comment", issueCommentCreated), deps)

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.Error(t, a.Disconnect(), "drain timeout should surface an error, not a clean nil")
}

// TestDisconnect_SlowShutdownDoesNotStarveDrain is the end-to-end regression
// for the two-budget Disconnect: a slow client keeps a connection active so
// srv.Shutdown burns its full deadline, and the drain must still get its own
// deadline to let an already-acked in-flight dispatch finish. With one shared
// context (the bug) drain would inherit ~0s and abandon the goroutine. It takes
// ~6s of real time, so it is gated behind an env var to keep the default suite
// hermetic and fast.
func TestDisconnect_SlowShutdownDoesNotStarveDrain(t *testing.T) {
	if os.Getenv("BOTBOOTER_GITHUB_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_GITHUB_DRAIN_TIMING_TEST to run the ~6s slow-shutdown drain timing test")
	}

	// Bind a concrete free port so raw client connections can target it; Connect
	// does not expose its listener address before this override.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "probe listen")
	addr := probe.Addr().String()
	asserts.NoError(t, probe.Close(), "probe close")

	identity := selfIdentityServer(t)
	defer identity.Close()
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, identity)
	a.cfg.Addr = addr

	// The acked dispatch outlives Shutdown's 5s deadline: if drain shared that
	// deadline it would get ~0s and never wait for this goroutine.
	dispatchDone := make(chan struct{})
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { return nil },
		Dispatch: func(context.Context, *core.Message) {
			time.Sleep(6 * time.Second)
			close(dispatchDone)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")

	// 1) A valid webhook: the handler acks 200 and spawns the in-flight dispatch.
	body := []byte(issueCommentCreated)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", bytes.NewReader(body))
	asserts.NoError(t, err, "build webhook request")
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", sign("hook-secret", body))
	resp, err := http.DefaultClient.Do(req)
	asserts.NoError(t, err, "post webhook")
	_ = resp.Body.Close()
	asserts.Equal(t, resp.StatusCode, http.StatusOK, "webhook acked")

	// Wait until the dispatch goroutine has registered as in-flight.
	deadline := time.Now().Add(time.Second)
	for a.inflight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	asserts.Equal(t, a.inflight.Load(), int64(1), "dispatch is in-flight")

	// 2) A slow client promises a body it never sends, so the handler blocks
	//    reading it and the connection stays active through Shutdown.
	slow, err := net.Dial("tcp", addr)
	asserts.NoError(t, err, "slow dial")
	defer func() { _ = slow.Close() }()
	_, err = slow.Write([]byte("POST /webhook HTTP/1.1\r\nHost: x\r\n" +
		"X-Hub-Signature-256: bad\r\nContent-Length: 1000000\r\n\r\n"))
	asserts.NoError(t, err, "slow write")
	time.Sleep(100 * time.Millisecond) // let the server mark the connection active

	// 3) Disconnect: Shutdown burns ~5s on the slow client; drain must still get
	//    its own budget so the acked dispatch (6s) finishes.
	start := time.Now()
	err = a.Disconnect()
	elapsed := time.Since(start)
	// Shutdown times out waiting on the slow client, so a deadline error here is
	// expected; the point of the test is that drain still saved the dispatch.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected Disconnect error: %v", err)
	}

	asserts.Equal(t, a.inflight.Load(), int64(0),
		"an acked in-flight dispatch must survive a slow shutdown (separate drain budget)")

	// Guard the harness itself: if Shutdown returned fast the slow-client hold
	// failed and the scenario never exercised the shared-budget path.
	if elapsed < 4*time.Second {
		t.Fatalf("expected Shutdown to burn ~5s on the slow client, elapsed=%s", elapsed)
	}

	select {
	case <-dispatchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch goroutine never completed")
	}
}
