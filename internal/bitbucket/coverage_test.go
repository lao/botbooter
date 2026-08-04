package bitbucket

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// DC PR and push deliveries for the callback tests.
const (
	serverPREvent   = `{"actor":{"slug":"dev"},"pullRequest":{"id":9,"toRef":{"repository":{"slug":"r","project":{"key":"P"}}}}}`
	serverPushEvent = `{"actor":{"slug":"dev"},"repository":{"slug":"r","project":{"key":"P"}},"changes":[]}`
)

func newDCAdapter(t *testing.T, cfg Config) *adapter {
	t.Helper()
	cfg.Secret, cfg.Addr, cfg.BaseURL, cfg.Self = testSecret, "127.0.0.1:0", "https://bb.example.com", "botuser"
	cfg.AccessToken = "tok"
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "dc adapter")
	a.logger = discardLogger()
	return a
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDCCallbacks(t *testing.T) {
	t.Run("PullRequest", func(t *testing.T) {
		got := make(chan *PullRequestEvent, 1)
		a := newDCAdapter(t, Config{OnPullRequest: func(_ context.Context, ev *PullRequestEvent) { got <- ev }})
		body := []byte(serverPREvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pr:opened", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		select {
		case ev := <-got:
			asserts.NotNil(t, ev.Server, "server PR union set")
		case <-time.After(2 * time.Second):
			t.Fatal("DC PR callback not invoked")
		}
	})

	t.Run("Push", func(t *testing.T) {
		got := make(chan *PushEvent, 1)
		a := newDCAdapter(t, Config{OnPush: func(_ context.Context, ev *PushEvent) { got <- ev }})
		body := []byte(serverPushEvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "repo:refs_changed", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "acked")
		select {
		case ev := <-got:
			asserts.NotNil(t, ev.Server, "server push union set")
		case <-time.After(2 * time.Second):
			t.Fatal("DC push callback not invoked")
		}
	})
}

func TestPushNilAndUnparseable(t *testing.T) {
	t.Run("NilCallback", func(t *testing.T) {
		a := newCloudAdapter(t) // no OnPush
		body := []byte(cloudPushEvent)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "repo:push", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "nil push callback acked and dropped")
	})

	t.Run("Unparseable", func(t *testing.T) {
		a := newCloudAdapter(t)
		a.cfg.OnPush = func(context.Context, *PushEvent) {}
		body := []byte(`{ broken`)
		rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "repo:push", body, sign(testSecret, body))
		asserts.Equal(t, rec.Code, http.StatusOK, "unparseable push acked")
	})
}

func TestPullRequestUnparseable(t *testing.T) {
	a := newCloudAdapter(t)
	a.cfg.OnPullRequest = func(context.Context, *PullRequestEvent) {}
	body := []byte(`{ broken`)
	rec := serveWebhook(a, core.AdapterDeps{Logger: a.logger}, "pullrequest:created", body, sign(testSecret, body))
	asserts.Equal(t, rec.Code, http.StatusOK, "unparseable PR acked")
}

// resolveSelf (Cloud) reads the account UUID from GET /2.0/user.
func TestResolveSelfCloud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uuid":"{resolved-me}","username":"bot"}`))
	}))
	defer srv.Close()
	a := cloudTestAdapter(t, srv.URL)
	self, err := a.fl.resolveSelf(context.Background())
	asserts.NoError(t, err, "resolve self")
	asserts.Equal(t, self, "{resolved-me}", "self uuid")
}

// A cancelled context aborts the Cloud whoami probe.
func TestResolveSelfCloudCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Block until the connection is torn down (at srv.Close) rather than on a
		// fixed sleep, so the orphaned probe request exits promptly.
		<-r.Context().Done()
	}))
	defer srv.Close()
	a := cloudTestAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.fl.resolveSelf(ctx)
	asserts.Error(t, err, "cancelled probe errors")
}

// resolveSelf (Data Center) has no whoami and returns empty.
func TestResolveSelfDataCenter(t *testing.T) {
	self, err := (&serverFlavor{}).resolveSelf(context.Background())
	asserts.NoError(t, err, "dc resolve self")
	asserts.Equal(t, self, "", "dc has no whoami")
}

func TestParentID(t *testing.T) {
	asserts.Equal(t, parentID(core.SendOptions{}), int64(0), "zero options → no parent")
	asserts.Equal(t, parentID(core.SendOptions{ThreadID: "1001"}), int64(1001), "thread id wins")
	asserts.Equal(t, parentID(core.SendOptions{ThreadID: "nope"}), int64(0), "non-numeric → 0")
	asserts.Equal(t, parentID(core.SendOptions{ReplyTo: &core.Message{ID: "77"}}), int64(77), "reply-to id used")
	asserts.Equal(t, parentID(core.SendOptions{ThreadID: "5", ReplyTo: &core.Message{ID: "9"}}), int64(5), "thread id beats reply-to")
}

func TestTimestampFallbacks(t *testing.T) {
	asserts.Equal(t, parseRFC3339("not a time").IsZero(), true, "bad RFC3339 → zero")
	asserts.Equal(t, millisToTime(0).IsZero(), true, "zero millis → zero")
}

func TestAttachmentsNone(t *testing.T) {
	atts, err := newCloudAdapter(t).Attachments(&core.Message{})
	asserts.NoError(t, err, "attachments")
	asserts.True(t, atts == nil, "no attachments")
}

// Addr and CloudClient return zero values for a non-Bitbucket bot.
func TestAccessorsForeignBot(t *testing.T) {
	asserts.Equal(t, Addr(&core.Bot{}), "", "Addr on foreign bot")
	asserts.True(t, CloudClient(&core.Bot{}) == nil, "CloudClient on foreign bot")
}

// serve reports an unexpected Serve error (not the clean-shutdown ErrServerClosed).
func TestServeReportsError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "listen")
	_ = ln.Close() // Serve on a closed listener fails immediately
	done := make(chan error, 1)
	serve(&http.Server{}, ln, func(e error) { done <- e })
	select {
	case e := <-done:
		asserts.Error(t, e, "serve reports the listener error")
	case <-time.After(time.Second):
		t.Fatal("serve did not report")
	}
}

func TestAuthFunc(t *testing.T) {
	bearer := authFunc(Config{AccessToken: "secret"}, true)
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	bearer(req)
	asserts.Equal(t, req.Header.Get("Authorization"), "Bearer secret", "bearer header")

	basic := authFunc(Config{Email: "e@x", APIToken: "pw"}, false)
	req2, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	basic(req2)
	u, p, ok := req2.BasicAuth()
	asserts.True(t, ok && u == "e@x" && p == "pw", "basic auth header")
}

func TestValidSegmentEdges(t *testing.T) {
	asserts.False(t, validSegment("."), "single dot rejected")
	asserts.False(t, validSegment("a b"), "space rejected")
	asserts.True(t, validSegment("a.b-c_d1"), "allowed set accepted")
	asserts.True(t, validSegment("~john"), "DC personal-repo project key accepted")
	asserts.False(t, validSegment("a/b"), "path separator rejected")
}

func TestParseErrorBranches(t *testing.T) {
	bad := []byte(`{ broken`)
	_, ok := (&cloudFlavor{}).parseComment("issue:comment_created", bad)
	asserts.False(t, ok, "cloud issue unparseable")
	_, _, ok = (&cloudFlavor{}).parsePullRequest("", bad)
	asserts.False(t, ok, "cloud PR unparseable")
	_, ok = (&cloudFlavor{}).parsePush("", bad)
	asserts.False(t, ok, "cloud push unparseable")
	_, ok = (&serverFlavor{}).parseComment("", bad)
	asserts.False(t, ok, "server comment unparseable")
	_, _, ok = (&serverFlavor{}).parsePullRequest("", bad)
	asserts.False(t, ok, "server PR unparseable")
	_, ok = (&serverFlavor{}).parsePush("", bad)
	asserts.False(t, ok, "server push unparseable")
}

func TestSendCloudErrors(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError)
	a := cloudTestAdapter(t, srv.URL)
	asserts.Error(t, a.Send(context.Background(), "ws/repo!1", "x", core.SendOptions{}), "cloud PR API error")
	asserts.Error(t, a.Send(context.Background(), "ws/repo#1", "x", core.SendOptions{}), "cloud issue API error")
}

func TestLogFallback(t *testing.T) {
	asserts.NotNil(t, (&adapter{}).log(), "log falls back to slog.Default")
}

func TestDisconnectNotConnected(t *testing.T) {
	asserts.NoError(t, newCloudAdapter(t).Disconnect(), "disconnect when never connected is a no-op")
}

func TestConnectListenError(t *testing.T) {
	a := newCloudAdapter(t)
	a.cfg.Addr = "bad-host-no-port"
	err := a.Connect(context.Background(), core.AdapterDeps{Logger: a.logger})
	asserts.Error(t, err, "listen error surfaces from Connect")
}

// Addr reports the bound address once connected.
func TestAddrConnected(t *testing.T) {
	bot, err := New(Config{Email: "e@x", APIToken: "tok", Secret: testSecret, Addr: "127.0.0.1:0", Self: "me"})
	asserts.NoError(t, err, "new bot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, bot.Connect(ctx), "connect")
	defer func() { _ = bot.Disconnect() }()
	asserts.True(t, Addr(bot) != "", "Addr reports the bound address")
}

// Cancelling the run context tears the server down via the ctx-watch goroutine.
func TestContextCancelTeardown(t *testing.T) {
	a, err := newAdapter(Config{Secret: testSecret, Addr: "127.0.0.1:0", AccessToken: "tok", Self: "me"})
	asserts.NoError(t, err, "adapter")
	a.logger = discardLogger()
	deps := core.AdapterDeps{
		Dispatch:   func(context.Context, *core.Message) {},
		Disconnect: func() error { return a.Disconnect() },
		Logger:     a.logger,
	}
	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "connect")
	cancel()
	// The ctx-watch goroutine calls Disconnect, which clears boundAddr.
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		gone := a.srv == nil
		a.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("context cancel did not tear the server down")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
