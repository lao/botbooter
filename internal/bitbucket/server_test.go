package bitbucket

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

const testSecret = "topsecret"

// cloudPREvent / cloudPushEvent are Cloud PR-created and push deliveries for the
// callback tests; the comment payloads live in message_test.go.
const (
	cloudPREvent   = `{"actor":{"uuid":"{pr-actor}"},"repository":{"full_name":"myws/myrepo"},"pullrequest":{"id":42,"title":"x"}}`
	cloudPushEvent = `{"actor":{"uuid":"{pusher}"},"repository":{"full_name":"myws/myrepo"},"push":{"changes":[]}}`
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func discardDeps(disp chan *core.Message) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { disp <- m },
		Logger:   discardLogger(),
	}
}

// serveWebhook drives a.handleWebhook with a crafted signed request.
func serveWebhook(a *adapter, deps core.AdapterDeps, key string, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set(eventKeyHeader, key)
	if sig != "" {
		req.Header.Set(signatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	a.handleWebhook(context.Background(), rec, req, deps)
	return rec
}

func newCloudAdapter(t *testing.T) *adapter {
	t.Helper()
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", Email: "e@x", APIToken: "tok"})
	asserts.NoError(t, err, "new adapter")
	a.logger = discardLogger()
	return a
}

// waitMsg reads one dispatched message or fails.
func waitMsg(t *testing.T, disp chan *core.Message) *core.Message {
	t.Helper()
	select {
	case m := <-disp:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("expected a dispatch, got none")
		return nil
	}
}

// assertNoDispatch fails if anything is dispatched within a short window.
func assertNoDispatch(t *testing.T, disp chan *core.Message) {
	t.Helper()
	select {
	case m := <-disp:
		t.Fatalf("expected no dispatch, got %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSignatureRejects(t *testing.T) {
	body := []byte(cloudPRComment)
	valid := sign(testSecret, body)
	tests := []struct {
		name string
		sig  string
	}{
		{name: "WrongSecret", sig: sign("wrong", body)},
		{name: "AbsentHeader", sig: ""},
		{name: "WrongAlgoPrefix", sig: "sha1=" + valid[len("sha256="):]},
		{name: "NoEquals", sig: "sha256"},
		{name: "NonHex", sig: "sha256=zzzz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disp := make(chan *core.Message, 1)
			a := newCloudAdapter(t)
			rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, tc.sig)
			asserts.Equal(t, rec.Code, http.StatusUnauthorized, "bad signature is 401")
			assertNoDispatch(t, disp)
		})
	}
}

// A body mutated after signing fails the HMAC.
func TestSignatureBodyTampered(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a := newCloudAdapter(t)
	body := []byte(cloudPRComment)
	sig := sign(testSecret, body)
	rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", append(body, ' '), sig)
	asserts.Equal(t, rec.Code, http.StatusUnauthorized, "tampered body is 401")
	assertNoDispatch(t, disp)
}

func TestIngressCloud(t *testing.T) {
	t.Run("PullRequestComment", func(t *testing.T) {
		disp := make(chan *core.Message, 1)
		a := newCloudAdapter(t)
		body := []byte(cloudPRComment)
		rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "authentic delivery acked")
		m := waitMsg(t, disp)
		asserts.Equal(t, m.ChannelID, "myws/myrepo!42", "channel id")
		asserts.Equal(t, m.Content, "please deploy", "content")
	})

	t.Run("IssueComment", func(t *testing.T) {
		disp := make(chan *core.Message, 1)
		a := newCloudAdapter(t)
		body := []byte(cloudIssueComment)
		rec := serveWebhook(a, discardDeps(disp), "issue:comment_created", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		m := waitMsg(t, disp)
		asserts.Equal(t, m.ChannelID, "myws/myrepo#7", "issue channel id")
	})
}

func TestIngressDataCenter(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", AccessToken: "tok", BaseURL: "https://bb.example.com", Self: "someone-else"})
	asserts.NoError(t, err, "dc adapter")
	a.logger = discardLogger()
	body := []byte(serverPRComment)
	rec := serveWebhook(a, discardDeps(disp), "pr:comment:added", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "acked")
	m := waitMsg(t, disp)
	asserts.Equal(t, m.ChannelID, "PROJ/myrepo!42", "DC channel id")
	asserts.Equal(t, m.Content, "deploy please", "content")
}

// The bot's own comment is acked but not dispatched (the reply-loop guard).
func TestSelfAuthoredCommentDropped(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a := newCloudAdapter(t)
	a.self = "{actor-uuid}" // matches cloudPRComment's actor
	body := []byte(cloudPRComment)
	rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "self comment acked")
	assertNoDispatch(t, disp)
}

func TestAuthenticButDropped(t *testing.T) {
	tests := []struct {
		name string
		key  string
		body string
	}{
		{name: "UnknownKey", key: "repo:commit_status_created", body: cloudPRComment},
		{name: "UnparseableBody", key: "pullrequest:comment_created", body: `{ broken`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disp := make(chan *core.Message, 1)
			a := newCloudAdapter(t)
			body := []byte(tc.body)
			rec := serveWebhook(a, discardDeps(disp), tc.key, body, sign(testSecret, body))
			asserts.Equal(t, rec.Code, http.StatusOK, "authentic delivery acked")
			assertNoDispatch(t, disp)
		})
	}
}

// A body over the cap is acked and dropped without dispatch.
func TestBodyOverCap(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a := newCloudAdapter(t)
	a.maxRequestBytes = 10
	body := []byte(cloudPRComment)
	rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "over-cap delivery acked")
	assertNoDispatch(t, disp)
}

func TestReadSemaphoreSaturated(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a := newCloudAdapter(t)
	a.readSem = make(chan struct{}, 1)
	a.readSem <- struct{}{} // saturate
	body := []byte(cloudPRComment)
	rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "shed delivery acked")
	assertNoDispatch(t, disp)
}

func TestDispatchSemaphoreSaturated(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a := newCloudAdapter(t)
	a.sem = make(chan struct{}, 1)
	a.sem <- struct{}{} // saturate
	body := []byte(cloudPRComment)
	rec := serveWebhook(a, discardDeps(disp), "pullrequest:comment_created", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "shed delivery acked")
	assertNoDispatch(t, disp)
}

// --- Callbacks -------------------------------------------------------------

func TestOnPullRequestCallback(t *testing.T) {
	t.Run("Invoked", func(t *testing.T) {
		got := make(chan *PullRequestEvent, 1)
		cfg := Config{Secret: testSecret, Addr: "127.0.0.1:0", Email: "e", APIToken: "t",
			OnPullRequest: func(_ context.Context, ev *PullRequestEvent) { got <- ev }}
		a, err := newAdapter(cfg)
		asserts.NoError(t, err, "adapter")
		a.logger = discardLogger()
		body := []byte(cloudPREvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pullrequest:created", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		select {
		case ev := <-got:
			asserts.NotNil(t, ev.Cloud, "cloud PR union set")
		case <-time.After(2 * time.Second):
			t.Fatal("callback not invoked")
		}
	})

	t.Run("NilCallbackDropped", func(t *testing.T) {
		a := newCloudAdapter(t) // no OnPullRequest
		body := []byte(cloudPREvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pullrequest:created", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "nil callback acked and dropped")
	})

	t.Run("SelfAuthoredDropped", func(t *testing.T) {
		got := make(chan *PullRequestEvent, 1)
		cfg := Config{Secret: testSecret, Addr: "127.0.0.1:0", Email: "e", APIToken: "t",
			OnPullRequest: func(_ context.Context, ev *PullRequestEvent) { got <- ev }}
		a, err := newAdapter(cfg)
		asserts.NoError(t, err, "adapter")
		a.logger = discardLogger()
		a.self = "{pr-actor}" // matches cloudPREvent author
		body := []byte(cloudPREvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pullrequest:created", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		select {
		case <-got:
			t.Fatal("self-authored PR should be dropped")
		case <-time.After(50 * time.Millisecond):
		}
	})

	t.Run("NonReviewableActionDropped", func(t *testing.T) {
		got := make(chan *PullRequestEvent, 1)
		cfg := Config{Secret: testSecret, Addr: "127.0.0.1:0", Email: "e", APIToken: "t",
			OnPullRequest: func(_ context.Context, ev *PullRequestEvent) { got <- ev }}
		a, err := newAdapter(cfg)
		asserts.NoError(t, err, "adapter")
		a.logger = discardLogger()
		body := []byte(cloudPREvent)
		// pullrequest:approved is not a reviewable-content change → catUnknown.
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pullrequest:approved", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		select {
		case <-got:
			t.Fatal("non-reviewable PR action should be dropped")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

func TestOnPushCallback(t *testing.T) {
	got := make(chan *PushEvent, 1)
	cfg := Config{Secret: testSecret, Addr: "127.0.0.1:0", Email: "e", APIToken: "t",
		OnPush: func(_ context.Context, ev *PushEvent) { got <- ev }}
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "adapter")
	a.logger = discardLogger()
	body := []byte(cloudPushEvent)
	rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "repo:push", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "acked")
	select {
	case ev := <-got:
		asserts.NotNil(t, ev.Cloud, "cloud push union set")
	case <-time.After(2 * time.Second):
		t.Fatal("push callback not invoked")
	}
}

// --- Lifecycle -------------------------------------------------------------

// stubFlavor drives the Connect-path self-resolve tests without a network.
type stubFlavor struct {
	self  string
	err   error
	delay time.Duration
}

func (*stubFlavor) category(string) eventCategory                     { return catUnknown }
func (*stubFlavor) parseComment(string, []byte) (*core.Message, bool) { return nil, false }
func (*stubFlavor) parsePullRequest(string, []byte) (*PullRequestEvent, string, bool) {
	return nil, "", false
}
func (*stubFlavor) parsePush(string, []byte) (*PushEvent, bool)                     { return nil, false }
func (*stubFlavor) postComment(context.Context, commentTarget, string, int64) error { return nil }
func (s *stubFlavor) resolveSelf(ctx context.Context) (string, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.self, s.err
}

func TestConnectServesAndDispatches(t *testing.T) {
	disp := make(chan *core.Message, 1)
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", AccessToken: "tok", Self: "someone-else"})
	asserts.NoError(t, err, "adapter")
	deps := discardDeps(disp)
	deps.Disconnect = func() error { return a.Disconnect() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "connect")

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	asserts.True(t, addr != "", "Addr recovered after :0")

	body := []byte(cloudPRComment)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", bytes.NewReader(body))
	req.Header.Set(eventKeyHeader, "pullrequest:comment_created")
	req.Header.Set(signatureHeader, sign(testSecret, body))
	resp, err := http.DefaultClient.Do(req)
	asserts.NoError(t, err, "POST")
	asserts.Equal(t, resp.StatusCode, http.StatusOK, "acked")
	_ = resp.Body.Close()
	waitMsg(t, disp)

	// Non-POST is 405.
	getResp, err := http.Get("http://" + addr + "/webhook")
	asserts.NoError(t, err, "GET")
	asserts.Equal(t, getResp.StatusCode, http.StatusMethodNotAllowed, "non-POST is 405")
	_ = getResp.Body.Close()

	asserts.NoError(t, a.Disconnect(), "disconnect drains cleanly")
}

// A whoami failure at Connect is fatal via deps.Done, and the listener is closed
// rather than left bound.
func TestConnectWhoamiFailureFatal(t *testing.T) {
	a := newCloudAdapter(t)
	a.fl = &stubFlavor{err: errors.New("boom")}
	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(err error) { done <- err },
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	asserts.NoError(t, a.Connect(context.Background(), deps), "connect returns (probe is async)")
	select {
	case err := <-done:
		asserts.Error(t, err, "whoami failure reported via Done")
	case <-time.After(2 * time.Second):
		t.Fatal("Done not called on whoami failure")
	}
}

// A whoami that succeeds but returns an empty identity is as fatal as an error:
// an empty self silently disables the reply-loop guard, so the bot must not serve.
func TestConnectWhoamiEmptyIdentityFatal(t *testing.T) {
	a := newCloudAdapter(t)
	a.fl = &stubFlavor{} // self == "", err == nil
	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(err error) { done <- err },
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	asserts.NoError(t, a.Connect(context.Background(), deps), "connect returns (probe is async)")
	select {
	case err := <-done:
		asserts.ErrorIs(t, err, ErrMissingConfig, "empty identity reported via Done")
	case <-time.After(2 * time.Second):
		t.Fatal("Done not called on empty identity")
	}
}

// A hanging whoami is bounded by selfResolveBudget, so Connect does not leave the
// listener bound forever.
func TestConnectWhoamiBudget(t *testing.T) {
	a := newCloudAdapter(t)
	a.fl = &stubFlavor{delay: time.Second}
	a.selfResolveBudget = 20 * time.Millisecond
	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Done:       func(err error) { done <- err },
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	asserts.NoError(t, a.Connect(context.Background(), deps), "connect")
	select {
	case err := <-done:
		asserts.Error(t, err, "budget-exceeded whoami reported")
	case <-time.After(time.Second):
		t.Fatal("budget did not fire")
	}
}

// Disconnect reports a drain timeout when an in-flight dispatch outlives the
// drain budget.
func TestDisconnectDrainDeadline(t *testing.T) {
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", AccessToken: "tok", Self: "someone-else"})
	asserts.NoError(t, err, "adapter")
	a.logger = discardLogger()
	a.drainBudget = 10 * time.Millisecond

	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) { time.Sleep(300 * time.Millisecond) },
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "connect")

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	body := []byte(cloudPRComment)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", bytes.NewReader(body))
	req.Header.Set(eventKeyHeader, "pullrequest:comment_created")
	req.Header.Set(signatureHeader, sign(testSecret, body))
	resp, err := http.DefaultClient.Do(req)
	asserts.NoError(t, err, "POST")
	_ = resp.Body.Close()

	err = a.Disconnect()
	asserts.Error(t, err, "drain deadline reached with in-flight dispatch")
}

// A reconnect installs fresh per-connection state and serves again.
func TestReconnect(t *testing.T) {
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", AccessToken: "tok", Self: "someone-else"})
	asserts.NoError(t, err, "adapter")
	a.logger = discardLogger()
	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "first connect")
	asserts.NoError(t, a.Disconnect(), "first disconnect")
	asserts.NoError(t, a.Connect(ctx, deps), "reconnect")
	asserts.NoError(t, a.Disconnect(), "second disconnect")
}
