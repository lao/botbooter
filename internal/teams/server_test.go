package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return testAddr("failing") }

type testAddr string

func (testAddr) Network() string  { return "test" }
func (a testAddr) String() string { return string(a) }

// channelActivityJSON builds an inbound message Activity carrying the given
// channelId (the field the endorsement check reads), for the endorsement tests.
func channelActivityJSON(channelID string) string {
	return activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1", channelID)
}

func captureDeps(got *[]*core.Message, done chan<- struct{}) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) {
			*got = append(*got, m)
			if done != nil {
				done <- struct{}{}
			}
		},
	}
}

func awaitDispatch(t *testing.T, done <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for dispatch %d of %d", i+1, n)
		}
	}
}

// activityJSONFromRole builds an inbound message Activity whose from account
// carries the given Bot Framework role ("user"/"bot"). Teams sets from.role to
// "bot" on messages authored by a bot (the bot's own echo or another bot in a
// shared channel), which is how such messages are identified and dropped.
func activityJSONFromRole(role, serviceURL, fromID, recipientID, convID string) string {
	act := map[string]any{
		"type":         "message",
		"id":           "act-1",
		"text":         "hi",
		"serviceUrl":   serviceURL,
		"timestamp":    "2026-06-30T12:00:00Z",
		"from":         map[string]string{"id": fromID, "name": "Ada", "role": role},
		"recipient":    map[string]string{"id": recipientID},
		"conversation": map[string]string{"id": convID},
	}
	b, _ := json.Marshal(act)
	return string(b)
}

// post drives handleMessages with a body and Authorization header, returning the
// recorder.
func post(a *adapter, deps core.AdapterDeps, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	a.handleMessages(context.Background(), w, r, deps)
	return w
}

func TestHandleMessages_DispatchesText(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	done := make(chan struct{}, 1)
	deps := captureDeps(&got, done)

	body := activityJSON("message", "hi there", allowedServiceURL, "user-1", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))

	w := post(a, deps, body, "Bearer "+token)
	asserts.Equal(t, w.Code, http.StatusOK, "valid request should be 200")

	awaitDispatch(t, done, 1)
	asserts.Equal(t, len(got), 1, "one message dispatched")
	asserts.Equal(t, got[0].Content, "hi there", "dispatched content")

	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, a.convs["conv-1"].serviceURL, allowedServiceURL, "conversation serviceUrl recorded")
	asserts.Equal(t, a.convs["conv-1"].bot.ID, "bot-1", "bot account recorded for replies")
}

// TestHandleMessages_DispatchesOnDetachedCtx guards the drain: core cancels the run
// context before Disconnect drains in-flight dispatch, so dispatch must ride the
// detached context passed in, not runCtx — otherwise a handler's reply would fail
// with "context canceled" mid-drain.
func TestHandleMessages_DispatchesOnDetachedCtx(t *testing.T) {
	a := testAdapter(t)
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{
		Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c },
	}

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	a.handleMessages(dispatchCtx, httptest.NewRecorder(), r, deps)

	select {
	case c := <-gotCtx:
		asserts.NoError(t, c.Err(), "dispatch ctx live before cancel")
		cancelDispatch()
		asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch rides the passed detached ctx")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestHandleMessages_RejectsMissingToken(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	w := post(a, deps, body, "")

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "no token should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

// TestHandleMessages_Routes401ToInjectedLogger proves the logger stored at
// Connect (here set directly) carries the rejection diagnostic, rather than
// always falling back to slog.Default.
func TestHandleMessages_Routes401ToInjectedLogger(t *testing.T) {
	a := testAdapter(t)
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	var got []*core.Message

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	w := post(a, captureDeps(&got, nil), body, "")

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "no token should be 401")
	asserts.True(t, strings.Contains(buf.String(), "rejected with 401"),
		"the injected logger receives the rejection diagnostic")
}

func TestHandleMessages_RejectsBadSignature(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	// Sign with the right key but claim the wrong audience.
	token := mintToken(t, testKID, validClaims("someone-else", allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "wrong audience should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsServiceURLClaimMismatch(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	// Token is otherwise valid but its serviceurl claim does not match the body.
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, "https://smba.trafficmanager.net/other/"))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusUnauthorized, "serviceurl mismatch should be 401")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsNonAllowlistedHost(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	const evil = "https://evil.example.com/"
	body := activityJSON("message", "hi", evil, "user-1", "bot-1", "conv-1")
	// Token validates (claim matches the body), but the host is not allowlisted.
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, evil))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusForbidden, "non-allowlisted serviceUrl should be 403")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_DropsBotMessage(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	// A real bot-authored Activity — the bot's own echo, or another bot in a
	// shared channel — has from != recipient (recipient is always this bot) but
	// carries from.role == "bot". That role is what marks it, per the Teams docs.
	body := activityJSONFromRole("bot", allowedServiceURL, "bot-2", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusOK, "still acked")
	// Dispatch is async, so assert on the synchronous side effect: a dropped
	// Activity returns before recordConversation, so no conversation is recorded.
	_, recorded := a.convs["conv-1"]
	asserts.False(t, recorded, "a bot-role message is dropped before dispatch")
	asserts.Equal(t, len(got), 0, "a bot-role message is not dispatched")
}

// TestHandleMessages_SaturatedDispatchReturns503 proves the concurrency bound:
// once the dispatch semaphore is full, a further Activity is shed with 503
// (which the platform retries) rather than acked and dropped. The semaphore is
// forced to a single slot so one blocked dispatch saturates it.
func TestHandleMessages_SaturatedDispatchReturns503(t *testing.T) {
	a := testAdapter(t)
	a.dispatchSem = make(chan struct{}, 1) // force a single dispatch slot

	release := make(chan struct{})
	defer close(release)
	dispatched := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {
			dispatched <- struct{}{}
			<-release // hold the only slot
		},
	}

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	auth := "Bearer " + mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))

	// First request acquires the only slot, is acked, and blocks in dispatch.
	w1 := post(a, deps, body, auth)
	asserts.Equal(t, w1.Code, http.StatusOK, "first request acquires the slot and is acked")
	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never started")
	}

	// Second request finds the semaphore full and is shed with 503, not acked.
	w2 := post(a, deps, body, auth)
	asserts.Equal(t, w2.Code, http.StatusServiceUnavailable, "saturated dispatch sheds load with 503")
}

func TestHandleMessages_IgnoresNonMessage(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	deps := captureDeps(&got, nil)

	body := activityJSON("conversationUpdate", "", allowedServiceURL, "user-1", "bot-1", "conv-1")
	token := mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL))
	w := post(a, deps, body, "Bearer "+token)

	asserts.Equal(t, w.Code, http.StatusOK, "still acked")
	asserts.Equal(t, len(got), 0, "non-message activity not dispatched")
}

func TestHandleMessages_AcceptsEndorsedChannel(t *testing.T) {
	a := testAdapter(t, "msteams")
	var got []*core.Message
	done := make(chan struct{}, 1)
	deps := captureDeps(&got, done)

	body := channelActivityJSON("msteams")
	w := post(a, deps, body, "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	asserts.Equal(t, w.Code, http.StatusOK, "activity on an endorsed channel is accepted")
	awaitDispatch(t, done, 1)
	asserts.Equal(t, len(got), 1, "dispatched")
}

func TestHandleMessages_RejectsUnendorsedChannel(t *testing.T) {
	a := testAdapter(t, "msteams")
	var got []*core.Message
	deps := captureDeps(&got, nil)

	// Key endorsed for msteams only; an Activity claiming another channel must fail
	// even though its signature/aud/iss/serviceurl are all valid.
	body := channelActivityJSON("directline")
	w := post(a, deps, body, "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	asserts.Equal(t, w.Code, http.StatusUnauthorized, "channel not in the key's endorsements is rejected")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_RejectsBlankChannelWhenEndorsed(t *testing.T) {
	a := testAdapter(t, "msteams")
	var got []*core.Message
	deps := captureDeps(&got, nil)

	// A blank channelId must not skip an endorsed key's channel check.
	body := channelActivityJSON("")
	w := post(a, deps, body, "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	asserts.Equal(t, w.Code, http.StatusUnauthorized, "blank channelId rejected for an endorsed key")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_UnendorsedKeyExemptFromChannelCheck(t *testing.T) {
	// A key with no endorsements (Emulator/Skill) authenticates regardless of
	// channelId — the exemption the auth tests without a channelId rely on.
	a := testAdapter(t)
	var got []*core.Message
	done := make(chan struct{}, 1)
	deps := captureDeps(&got, done)

	body := channelActivityJSON("")
	w := post(a, deps, body, "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	asserts.Equal(t, w.Code, http.StatusOK, "unendorsed key is exempt from the channel check")
	awaitDispatch(t, done, 1)
	asserts.Equal(t, len(got), 1, "dispatched")
}

func TestHandleMessages_BadJSON(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	w := post(a, captureDeps(&got, nil), "{not json", "")
	asserts.Equal(t, w.Code, http.StatusBadRequest, "unparseable body should be 400")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestHandleMessages_OversizedBody(t *testing.T) {
	a := testAdapter(t)
	var got []*core.Message
	big := strings.Repeat("a", maxRequestBytes+16)
	w := post(a, captureDeps(&got, nil), big, "")
	asserts.Equal(t, w.Code, http.StatusBadRequest, "oversized body should be 400")
	asserts.Equal(t, len(got), 0, "nothing dispatched")
}

func TestConnectDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}

	asserts.NoError(t, a.Connect(ctx, deps), "Connect should bind and start")
	asserts.NoError(t, a.Disconnect(), "Disconnect should shut down cleanly")
	asserts.NoError(t, a.Disconnect(), "Disconnect should be idempotent")
}

func TestAddr_ExposesBoundListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := newAdapter(validConfig()) // cfg.Addr is 127.0.0.1:0
	asserts.NoError(t, err, "newAdapter")
	b := core.New(core.TeamsBotType, a)
	deps := core.AdapterDeps{Done: func(error) {}, Disconnect: func() error { return nil }}

	asserts.Equal(t, Addr(b), "", "no bound address before Connect")
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")

	_, port, err := net.SplitHostPort(Addr(b))
	asserts.NoError(t, err, "Addr returns host:port after Connect")
	asserts.True(t, port != "" && port != "0", "OS-assigned port is resolved from :0")

	asserts.NoError(t, a.Disconnect(), "Disconnect")
	asserts.Equal(t, Addr(b), "", "bound address cleared after Disconnect")
}

// TestDisconnect_SlowShutdownDoesNotStarveDrain mirrors the whatsapp sibling: a
// slow client makes srv.Shutdown burn its full budget, and the test proves the
// in-flight dispatch drain still gets its own budget and finishes an acked
// message. ~6s of real time, so it is env-gated like the whatsapp version.
func TestDisconnect_SlowShutdownDoesNotStarveDrain(t *testing.T) {
	if os.Getenv("BOTBOOTER_TEAMS_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_TEAMS_DRAIN_TIMING_TEST to run the ~6s slow-shutdown drain timing test")
	}

	a := testAdapter(t) // cfg.Addr is 127.0.0.1:0; wires a JWKS server for validation

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

	a.mu.Lock()
	addr := a.boundAddr
	a.mu.Unlock()
	asserts.True(t, addr != "", "listener address exposed after Connect")

	// 1) A valid activity: the handler acks 200 and spawns the in-flight dispatch.
	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+a.cfg.Path, strings.NewReader(body))
	asserts.NoError(t, err, "build request")
	req.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	resp, err := http.DefaultClient.Do(req)
	asserts.NoError(t, err, "post activity")
	_ = resp.Body.Close()
	asserts.Equal(t, resp.StatusCode, http.StatusOK, "activity acked")

	deadline := time.Now().Add(time.Second)
	for a.inflight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	asserts.Equal(t, a.inflight.Load(), int64(1), "dispatch is in-flight")

	// 2) A slow client promises a body it never sends, holding a connection active
	//    through Shutdown so Shutdown burns its full budget.
	slow, err := net.Dial("tcp", addr)
	asserts.NoError(t, err, "slow dial")
	defer func() { _ = slow.Close() }()
	_, err = slow.Write([]byte("POST " + a.cfg.Path + " HTTP/1.1\r\nHost: x\r\n" +
		"Content-Length: 1000000\r\n\r\n"))
	asserts.NoError(t, err, "slow write")
	time.Sleep(100 * time.Millisecond)

	// 3) Disconnect: Shutdown burns ~5s on the slow client; drain must still save
	//    the acked 6s dispatch.
	start := time.Now()
	err = a.Disconnect()
	elapsed := time.Since(start)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected Disconnect error: %v", err)
	}

	asserts.Equal(t, a.inflight.Load(), int64(0),
		"an acked in-flight dispatch must survive a slow shutdown (separate drain budget)")
	if elapsed < 4*time.Second {
		t.Fatalf("expected Shutdown to burn ~5s on the slow client, elapsed=%s", elapsed)
	}

	select {
	case <-dispatchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch goroutine never completed")
	}
}

// TestDisconnect_CancelsDispatchContext guards the leak fix: Disconnect must cancel
// the connection's detached dispatch context after the drain, so a handler blocked
// on ctx.Done() cannot leak past shutdown. The connection's cancel is wired by hand
// so the test controls the exact context the handler dispatches on.
func TestDisconnect_CancelsDispatchContext(t *testing.T) {
	a := testAdapter(t)
	dispatchCtx, cancelDispatch := context.WithCancel(context.WithoutCancel(context.Background()))
	// srv must be non-nil so Disconnect runs its teardown rather than early-returning;
	// an unstarted server's Shutdown returns immediately.
	a.mu.Lock()
	a.srv = &http.Server{}
	a.detachedCancel = cancelDispatch
	a.mu.Unlock()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}
	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	a.handleMessages(dispatchCtx, httptest.NewRecorder(), r, deps)

	var c context.Context
	select {
	case c = <-gotCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
	asserts.NoError(t, c.Err(), "dispatch ctx live before Disconnect")
	asserts.NoError(t, a.Disconnect(), "Disconnect")
	asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch ctx canceled after drain")
}

// TestDisconnect_DrainTimeoutReturnsError guards that a drain which cannot finish
// within its deadline surfaces an error rather than masquerading as a clean
// shutdown: Disconnect force-cancels the straggler and must report it. The drain
// budget is a hardcoded 5s, so this takes ~5s of real time and is env-gated to
// keep the default suite fast (mirrors the whatsapp slow-drain timing test).
func TestDisconnect_DrainTimeoutReturnsError(t *testing.T) {
	if os.Getenv("BOTBOOTER_TEAMS_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_TEAMS_DRAIN_TIMING_TEST to run the ~5s drain-timeout test")
	}
	a := testAdapter(t)
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

	body := activityJSON("message", "hi", allowedServiceURL, "user-1", "bot-1", "conv-1")
	r := httptest.NewRequest(http.MethodPost, a.cfg.Path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+mintToken(t, testKID, validClaims(a.cfg.AppID, allowedServiceURL)))
	a.handleMessages(context.Background(), httptest.NewRecorder(), r, deps)

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.Error(t, a.Disconnect(), "drain timeout should surface an error, not a clean nil")
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
		t.Fatal("unexpected serve error was not reported")
	}
}

func TestConnect_StaleWatcherIgnoresReplacedServer(t *testing.T) {
	a := testAdapter(t)
	called := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { called <- struct{}{}; return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())

	asserts.NoError(t, a.Connect(ctx, deps), "Connect should bind")

	// Simulate a reconnect that installed a different server, then close the one
	// this Connect started so it does not leak.
	a.mu.Lock()
	old := a.srv
	a.srv = &http.Server{}
	a.mu.Unlock()
	defer func() { _ = old.Close() }()

	cancel() // wake the now-stale watcher

	select {
	case <-called:
		t.Fatal("a stale watcher must not drive Disconnect on a replaced server")
	case <-time.After(200 * time.Millisecond):
		// Expected: the guard saw a.srv != its own server and skipped.
	}
}

func TestDisconnect_NeverConnected(t *testing.T) {
	a := testAdapter(t)
	asserts.NoError(t, a.Disconnect(), "Disconnect before Connect should be safe")
}

func TestConnect_BadAddr(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.cfg.Addr = "127.0.0.1:999999" // port out of range
	err = a.Connect(context.Background(), core.AdapterDeps{
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	})
	asserts.Error(t, err, "binding an invalid address should fail Connect")
}

func TestConnect_RoutesWebhookMethods(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "reserve loopback address")
	addr := ln.Addr().String()
	asserts.NoError(t, ln.Close(), "release loopback address")

	cfg := validConfig()
	cfg.Addr = addr
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := core.AdapterDeps{Done: func(error) {}, Disconnect: a.Disconnect}
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	defer func() { asserts.NoError(t, a.Disconnect(), "Disconnect") }()

	resp, err := http.Get("http://" + addr + a.cfg.Path)
	asserts.NoError(t, err, "GET webhook")
	asserts.Equal(t, resp.StatusCode, http.StatusMethodNotAllowed, "webhook should require POST")
	_ = resp.Body.Close()

	resp, err = http.Post("http://"+addr+a.cfg.Path, "application/json", strings.NewReader("{"))
	asserts.NoError(t, err, "POST webhook")
	defer func() { _ = resp.Body.Close() }()
	asserts.Equal(t, resp.StatusCode, http.StatusBadRequest, "POST should reach message handler")
}

func TestConnect_ContextCancelDisconnects(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	ctx, cancel := context.WithCancel(context.Background())
	disc := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Done: func(error) {},
		Disconnect: func() error {
			disc <- struct{}{}
			return a.Disconnect()
		},
	}
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	cancel()
	select {
	case <-disc:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel did not trigger Disconnect")
	}
}

func TestDrainDispatch_ContextCancel(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	a.inflight.Add(1) // never decremented
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.drainDispatch(ctx) // must return promptly on a canceled ctx
	asserts.Equal(t, a.inflight.Load(), int64(1), "drain returns on ctx cancel without waiting")
}

func TestDrainDispatch_WaitsForInflight(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
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

func TestRecordConversation_BoundedEviction(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")
	// Insert one over the cap; the oldest must be evicted.
	for i := 0; i <= maxConversations; i++ {
		a.recordConversation("c"+strconv.Itoa(i), allowedServiceURL, channelAccount{})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	asserts.Equal(t, len(a.convs), maxConversations, "map capped at maxConversations")
	_, firstPresent := a.convs["c0"]
	asserts.False(t, firstPresent, "oldest entry evicted")
}

func TestRecordConversation_IgnoresIncompleteMapping(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter")

	a.recordConversation("", allowedServiceURL, channelAccount{})
	a.recordConversation("conv-1", "", channelAccount{})

	asserts.Equal(t, len(a.convs), 0, "incomplete mappings should not be recorded")
}
