package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
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
		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"id": 777, "login": "bot-account"}`))
		case r.URL.Path == "/app":
			_, _ = w.Write([]byte(`{"id": 7, "slug": "my-app"}`))
		case strings.Contains(r.URL.Path, "/users/my-app"):
			_, _ = w.Write([]byte(`{"id": 555, "login": "my-app[bot]"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
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
	selfID, boundAddr := a.selfID, a.boundAddr
	a.mu.Unlock()
	asserts.Equal(t, selfID, int64(777), "PAT self-identity resolved via /user")
	asserts.True(t, boundAddr != "", "listener bound, addr recoverable")
}

func TestConnect_SelfIdentityFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := newAdapter(patConfig())
	asserts.NoError(t, err, "new adapter")
	a.client = testClient(t, srv)

	err = a.Connect(context.Background(), core.AdapterDeps{})

	asserts.Error(t, err, "a bot that cannot recognize itself must not start")
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.True(t, a.srv == nil, "no server installed on failure")
}

func TestConnect_AppModeResolvesBotUser(t *testing.T) {
	srv := selfIdentityServer(t)
	defer srv.Close()
	a, err := newAdapter(appConfig(t))
	asserts.NoError(t, err, "new App adapter")
	a.client = testClient(t, srv)
	a.baseURL = srv.URL // one-shot App-JWT client also hits the fake

	asserts.NoError(t, a.Connect(context.Background(), core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	}), "connect in App mode")
	defer func() { _ = a.Disconnect() }()

	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, a.selfID, int64(555), "App self-identity resolved via /app then /users/{slug}[bot]")
}

func TestDisconnect_IdempotentAndClears(t *testing.T) {
	a, srv, cancel := connectedAdapter(t, core.AdapterDeps{
		Done: func(error) {}, Disconnect: func() error { return nil },
	})
	defer srv.Close()
	defer cancel()

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
			a.selfID, a.selfLogin = 777, "bot-account"
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
