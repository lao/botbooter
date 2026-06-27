package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// recordDeps builds AdapterDeps whose Dispatch records the last message and a
// call count, so handler tests can assert what (if anything) was dispatched.
func recordDeps() (core.AdapterDeps, *int, **core.Message) {
	var (
		count int
		last  *core.Message
	)
	deps := core.AdapterDeps{Dispatch: func(_ context.Context, m *core.Message) {
		count++
		last = m
	}}
	return deps, &count, &last
}

func TestNew_BotType(t *testing.T) {
	bot := New(Config{})

	asserts.Equal(t, bot.BotType, core.WebhookBotType, "bot type")
}

func TestHandler_PostDispatchesAndAcks(t *testing.T) {
	a := newAdapter(Config{})
	deps, count, last := recordDeps()
	h := a.handler(context.Background(), deps)

	body := `{"id":"1","user_id":"u","channel_id":"c","content":"hi"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))

	asserts.Equal(t, rr.Code, http.StatusOK, "status")
	asserts.Equal(t, *count, 1, "one message dispatched")
	asserts.Equal(t, (*last).ChannelID, "c", "channel id")
	asserts.Equal(t, (*last).Content, "hi", "content")
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	a := newAdapter(Config{})
	deps, count, _ := recordDeps()
	h := a.handler(context.Background(), deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	asserts.Equal(t, rr.Code, http.StatusMethodNotAllowed, "status")
	asserts.Equal(t, rr.Header().Get("Allow"), http.MethodPost, "Allow header")
	asserts.Equal(t, *count, 0, "nothing dispatched")
}

func TestHandler_BadJSON(t *testing.T) {
	a := newAdapter(Config{})
	deps, count, _ := recordDeps()
	h := a.handler(context.Background(), deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json")))

	asserts.Equal(t, rr.Code, http.StatusBadRequest, "status")
	asserts.Equal(t, *count, 0, "nothing dispatched")
}

func TestHandler_NilMessageAcksWithoutDispatch(t *testing.T) {
	a := newAdapter(Config{Decoder: func(*http.Request) (*core.Message, error) {
		return nil, nil
	}})
	deps, count, _ := recordDeps()
	h := a.handler(context.Background(), deps)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))

	asserts.Equal(t, rr.Code, http.StatusOK, "status")
	asserts.Equal(t, *count, 0, "nil message dispatches nothing")
}

func TestSend(t *testing.T) {
	err := newAdapter(Config{}).Send(context.Background(), "c", "hi")
	asserts.ErrorIs(t, err, ErrNoSender, "Send without a sender")

	var gotChannel, gotText string
	withSender := newAdapter(Config{Sender: func(_ context.Context, channelID, text string) error {
		gotChannel, gotText = channelID, text
		return nil
	}})
	asserts.NoError(t, withSender.Send(context.Background(), "c", "hi"), "Send with a sender")
	asserts.Equal(t, gotChannel, "c", "sender channel")
	asserts.Equal(t, gotText, "hi", "sender text")
}

func TestRawPayload(t *testing.T) {
	p := &Payload{ChannelID: "c"}

	got, ok := RawPayload(&core.Message{Raw: p})
	asserts.True(t, ok, "RawPayload recovers the default payload")
	asserts.True(t, got == p, "RawPayload returns the same pointer")

	_, ok = RawPayload(&core.Message{})
	asserts.False(t, ok, "RawPayload reports false when Raw is unset")
}

func TestAttachments(t *testing.T) {
	atts, err := newAdapter(Config{}).Attachments(&core.Message{})
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 0, "webhook payload has no attachments")
}

// End-to-end: serve on a real loopback listener, POST a message, confirm a
// handler runs, then shut down cleanly via context cancellation.
func TestConnect_ServesAndShutsDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "listen")

	dispatched := make(chan *core.Message, 1)
	bot := New(Config{Listener: ln})
	asserts.NoError(t, bot.HandleFunc(".*", func(_ context.Context, _ *core.Bot, m *core.Message) {
		dispatched <- m
	}), "handler")

	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, bot.Connect(ctx), "connect")

	url := "http://" + ln.Addr().String() + "/"
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"channel_id":"c","content":"hi"}`))
	asserts.NoError(t, err, "post")
	_ = resp.Body.Close()
	asserts.Equal(t, resp.StatusCode, http.StatusOK, "status")

	select {
	case m := <-dispatched:
		asserts.Equal(t, m.Content, "hi", "dispatched content")
	case <-time.After(2 * time.Second):
		t.Fatal("message was not dispatched")
	}

	cancel()
	asserts.NoError(t, bot.Disconnect(), "disconnect")
}

func TestConnect_BindErrorSurfaces(t *testing.T) {
	// Hold a port so a second bind to the same address is guaranteed to fail,
	// regardless of whether the test runs as root.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "hold a port")
	defer func() { _ = held.Close() }()

	bot := New(Config{Addr: held.Addr().String()})

	asserts.Error(t, bot.Connect(context.Background()), "Connect should surface the bind error")
}
