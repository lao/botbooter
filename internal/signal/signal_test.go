package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// validConfig returns a Config with every required field populated. BaseURL is
// overwritten by tests that talk to a fake container.
func validConfig() Config {
	return Config{BaseURL: "http://127.0.0.1:1", Number: "+15550001"}
}

// fakeAPI is a fake signal-cli-rest-api container: it upgrades
// /v1/receive/{number} to a WebSocket and records /v2/send requests.
type fakeAPI struct {
	ts       *httptest.Server
	wsConns  chan *websocket.Conn
	sendReqs chan map[string]any

	// sendStatus/sendBody shape the /v2/send response; zero values mean 201.
	sendStatus int
	sendBody   string
	// sendBlock, when non-nil, stalls /v2/send until closed (or the request
	// context dies).
	sendBlock chan struct{}
}

func startAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		wsConns:  make(chan *websocket.Conn, 4),
		sendReqs: make(chan map[string]any, 4),
	}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/receive/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.wsConns <- ws
	})
	mux.HandleFunc("/v2/send", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.sendReqs <- req
		if f.sendBlock != nil {
			select {
			case <-f.sendBlock:
			case <-r.Context().Done():
				return
			}
		}
		status := f.sendStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		body := f.sendBody
		if body == "" {
			body = `{"timestamp":"1700000003000"}`
		}
		_, _ = w.Write([]byte(body))
	})
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

// acceptWS waits for the adapter to dial the receive socket.
func acceptWS(t *testing.T, f *fakeAPI) *websocket.Conn {
	t.Helper()
	select {
	case ws := <-f.wsConns:
		t.Cleanup(func() { _ = ws.Close() })
		return ws
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for adapter to dial the receive socket")
		return nil
	}
}

// captureDeps returns AdapterDeps recording dispatches into *got and signaling
// done per dispatch, plus routing Done errors into loopErr when non-nil.
func captureDeps(got *[]*core.Message, done chan<- struct{}, loopErr chan<- error) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) {
			*got = append(*got, m)
			if done != nil {
				done <- struct{}{}
			}
		},
		Done: func(err error) {
			if loopErr != nil {
				loopErr <- err
			}
		},
		Disconnect: func() error { return nil },
	}
}

// connectAdapter points a at the fake container, connects, and returns the
// server side of the receive socket.
func connectAdapter(t *testing.T, a *adapter, deps core.AdapterDeps, f *fakeAPI) *websocket.Conn {
	t.Helper()
	reconfigure(t, a, f)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")
	t.Cleanup(func() { _ = a.Disconnect() })
	return acceptWS(t, f)
}

// reconfigure rebuilds a's derived state (wsURL, client) for the fake
// container's BaseURL, keeping the rest of its config. Only called before the
// adapter is connected, so the field copies race with nothing.
func reconfigure(t *testing.T, a *adapter, f *fakeAPI) {
	t.Helper()
	cfg := a.cfg
	cfg.BaseURL = f.ts.URL
	fresh, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter for fake container")
	a.cfg = fresh.cfg
	a.baseURL = fresh.baseURL
	a.wsURL = fresh.wsURL
	a.client = fresh.client
}

// writeEnvelope pushes one receive payload to the adapter over the socket.
func writeEnvelope(t *testing.T, ws *websocket.Conn, payload string) {
	t.Helper()
	asserts.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(payload)), "container write")
}

// awaitSend waits for the fake container to record one /v2/send request.
func awaitSend(t *testing.T, f *fakeAPI) map[string]any {
	t.Helper()
	select {
	case req := <-f.sendReqs:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a /v2/send request")
		return nil
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

const directReceive = `{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceUuid":"uuid-2","sourceName":"Ada","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hello bot"}}}`

const groupReceive = `{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000000456,"dataMessage":{"timestamp":1700000000456,"message":"hi group","groupInfo":{"groupId":"grp==","type":"DELIVER"}}}}`

const receiptReceive = `{"account":"+15550001","envelope":{"source":"+15550002","timestamp":1700000000789,"receiptMessage":{"when":1700000000789,"isDelivery":true}}}`

const ownReceive = `{"account":"+15550001","envelope":{"source":"+15550001","sourceNumber":"+15550001","timestamp":1700000001000,"dataMessage":{"timestamp":1700000001000,"message":"from myself"}}}`

const attachmentReceive = `{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000002000,"dataMessage":{"timestamp":1700000002000,"message":"a cat","attachments":[{"contentType":"image/jpeg","filename":"cat.jpg","id":"att-1","size":1234}]}}}`

func TestNew(t *testing.T) {
	bot, err := New(validConfig())

	asserts.NoError(t, err, "New with full config should succeed")
	asserts.NotNil(t, bot, "bot should be initialized")
	asserts.Equal(t, bot.BotType, core.SignalBotType, "bot type should be Signal")
	asserts.Equal(t, bot.BotType.String(), "signal", "bot type string should be signal")
}

func TestNew_MissingBaseURL(t *testing.T) {
	_, err := New(Config{Number: "+15550001"})
	asserts.ErrorIs(t, err, ErrMissingConfig, "New without BaseURL should fail")
	asserts.False(t, errors.Is(err, ErrInvalidConfig), "an empty field is not an unusable one")
}

func TestNew_MissingNumber(t *testing.T) {
	_, err := New(Config{BaseURL: "http://127.0.0.1:8080"})
	asserts.ErrorIs(t, err, ErrMissingConfig, "New without Number should fail")
	asserts.False(t, errors.Is(err, ErrInvalidConfig), "an empty field is not an unusable one")
}

// TestNew_RejectsUnusableBaseURL covers every BaseURL shape parseBaseURL
// rejects. The last three would otherwise break one channel silently: a query
// or fragment mis-builds every REST path while the receive socket still dials,
// and userinfo makes gorilla refuse the dial while REST sends keep working. A
// present-but-unusable BaseURL is ErrInvalidConfig, not ErrMissingConfig — the
// field is set, just wrong.
func TestNew_RejectsUnusableBaseURL(t *testing.T) {
	for _, tc := range []struct{ name, baseURL string }{
		{"Unparseable", "http://127.0.0.1:8080/\x7f"}, // DEL: url.Parse rejects control bytes
		{"BadScheme", "ftp://127.0.0.1:8080"},
		{"NoHost", "http:///v2"},
		{"Query", "http://127.0.0.1:8080/?token=abc"},
		{"ForcedEmptyQuery", "http://127.0.0.1:8080/?"},
		{"Fragment", "http://127.0.0.1:8080#frag"},
		{"Userinfo", "http://bot:pw@127.0.0.1:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: tc.baseURL, Number: "+15550001"})
			asserts.ErrorIs(t, err, ErrInvalidConfig, "New with an unusable BaseURL should fail")
			asserts.False(t, errors.Is(err, ErrMissingConfig), "a set-but-unusable field is not a missing one")
		})
	}
}

// TestNew_RejectsUnusableBaseURL_NoCredentialLeak pins that a BaseURL failing
// to parse does not echo itself back: url.Parse's *url.Error carries the raw
// URL unredacted, so wrapping it whole would print an embedded password.
func TestNew_RejectsUnusableBaseURL_NoCredentialLeak(t *testing.T) {
	_, err := New(Config{BaseURL: "http://bot:hunter2@127.0.0.1:1x", Number: "+15550001"})
	asserts.ErrorIs(t, err, ErrInvalidConfig, "an unparseable BaseURL should fail")
	asserts.False(t, strings.Contains(err.Error(), "hunter2"), "the error must not echo the credentials")
}

// TestNew_AcceptsUsableBaseURL guards against parseBaseURL tightening onto
// shapes an operator may legitimately configure. The bracketed IPv6 host is the
// one that matters most: receiveSocketURL appends to the base string, so losing
// the brackets would produce a ws URL that cannot dial.
func TestNew_AcceptsUsableBaseURL(t *testing.T) {
	for _, tc := range []struct{ name, baseURL string }{
		{"IPv6", "http://[::1]:8080"},
		{"IPv6WithPath", "http://[::1]:8080/api"},
		{"HTTPSPortPath", "https://signal.example:8443/api"},
		{"Localhost", "http://localhost:8080"},
		{"UppercaseScheme", "HTTP://127.0.0.1:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{BaseURL: tc.baseURL, Number: "+15550001"})
			asserts.NoError(t, err, "New with a usable BaseURL should succeed")
		})
	}
}

func TestNew_Defaults(t *testing.T) {
	a, err := newAdapter(validConfig())
	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.True(t, a.cfg.DialTimeout > 0, "DialTimeout should default")
	asserts.True(t, a.cfg.SendTimeout > 0, "SendTimeout should default")
	asserts.NotNil(t, a.client, "HTTPClient should default")
}

func TestNew_DerivesReceiveSocketURL(t *testing.T) {
	a, err := newAdapter(Config{BaseURL: "https://signal.example/api/", Number: "+15550001"})
	asserts.NoError(t, err, "newAdapter")
	asserts.Equal(t, a.wsURL, "wss://signal.example/api/v1/receive/+15550001",
		"https becomes wss, trailing slash trimmed ('+' is valid in a path)")
	asserts.Equal(t, a.cfg.BaseURL, "https://signal.example/api", "BaseURL trailing slash trimmed")
	asserts.Equal(t, a.baseURL, "https://signal.example/api",
		"the REST base is the normalized parse, not cfg.BaseURL — it is what /v2/send is appended to")
}

func TestConnect_DialError(t *testing.T) {
	cfg := validConfig()
	cfg.DialTimeout = 200 * time.Millisecond
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter")

	err = a.Connect(context.Background(), core.AdapterDeps{})
	asserts.Error(t, err, "Connect to a dead address should fail")
}

func TestReceive_DispatchesDirectMessage(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, directReceive)
	awaitDispatch(t, done, 1)

	m := got[0]
	asserts.Equal(t, m.UserID, "+15550002", "UserID should be the sender number")
	asserts.Equal(t, m.AuthorName, "Ada", "AuthorName should be the source name")
	asserts.Equal(t, m.ChannelID, "+15550002", "direct ChannelID should be the sender")
	asserts.Equal(t, m.Content, "hello bot", "Content should be the message text")
	asserts.Equal(t, m.ID, "1700000000123", "ID should be the envelope timestamp")
	asserts.Equal(t, m.Timestamp.UnixMilli(), int64(1700000000123), "Timestamp should parse from millis")

	sm, ok := RawMessage(m)
	asserts.True(t, ok, "Raw should carry a signal Message")
	asserts.Equal(t, sm.Source, "+15550002", "raw Source")
	asserts.Equal(t, sm.Text, "hello bot", "raw Text")
}

func TestReceive_DispatchesGroupMessage(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, groupReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].ChannelID, "group:grp==", "group ChannelID should be prefixed")
	asserts.Equal(t, got[0].UserID, "+15550002", "UserID should stay the individual sender")
	sm, _ := RawMessage(got[0])
	asserts.Equal(t, sm.GroupID, "grp==", "raw GroupID")
}

func TestReceive_SkipsNonDataAndOwnMessages(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	// A receipt (no dataMessage), the bot's own message, then a real one. Only
	// the real one dispatches — and its arrival proves the loop survived the
	// first two.
	writeEnvelope(t, ws, receiptReceive)
	writeEnvelope(t, ws, ownReceive)
	writeEnvelope(t, ws, directReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "only the real inbound message should dispatch")
	asserts.Equal(t, got[0].Content, "hello bot", "the surviving message is the real one")
}

func TestReceive_SkipsMalformedPayload(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, `{not json`)
	writeEnvelope(t, ws, `"not an object"`)
	writeEnvelope(t, ws, directReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "malformed payloads should be skipped, not fatal")
}

// sourceOnlyReceive has no sourceNumber, so the adapter must fall back to the
// envelope's source field for the sender identity.
const sourceOnlyReceive = `{"account":"+15550001","envelope":{"source":"uuid-9","sourceName":"Bob","timestamp":1700000003000,"dataMessage":{"timestamp":1700000003000,"message":"via uuid"}}}`

func TestReceive_FallsBackToSource(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, sourceOnlyReceive)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, got[0].UserID, "uuid-9", "UserID should fall back to envelope source")
}

// TestReceive_SkipsEnvelopeWithNoSender pins that an envelope carrying none of
// the three source forms is dropped: dispatching it would hand a handler an
// empty UserID and ChannelID, and a reply would POST an empty recipient.
func TestReceive_SkipsEnvelopeWithNoSender(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, `{"envelope":{"timestamp":1700000000000,"dataMessage":{"message":"anonymous"}}}`)
	writeEnvelope(t, ws, `{"envelope":{"sourceNumber":"+15550002","timestamp":1700000001000,"dataMessage":{"message":"real"}}}`)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "only the identified message should dispatch")
	asserts.Equal(t, got[0].Content, "real", "the identified message should be the one dispatched")
}

// TestReceive_SkipsOwnMessageByAccount covers the self-drop against the
// container's own normalized number, which can be formatted differently from
// the operator-supplied Config.Number.
func TestReceive_SkipsOwnMessageByAccount(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, `{"account":"+15559999","envelope":{"sourceNumber":"+15559999","timestamp":1700000000000,"dataMessage":{"message":"from myself"}}}`)
	writeEnvelope(t, ws, `{"account":"+15559999","envelope":{"sourceNumber":"+15550002","timestamp":1700000001000,"dataMessage":{"message":"from someone else"}}}`)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 1, "the bot's own message should be dropped")
	asserts.Equal(t, got[0].Content, "from someone else", "only the other party's message should dispatch")
}

func TestSend_Direct(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	asserts.NoError(t, a.Send(context.Background(), "+15550002", "hi there", core.SendOptions{}), "Send should succeed on 201")

	req := awaitSend(t, f)
	asserts.Equal(t, req["number"], "+15550001", "number field")
	asserts.Equal(t, req["message"], "hi there", "message field")
	recipients := req["recipients"].([]any)
	asserts.Equal(t, len(recipients), 1, "one recipient")
	asserts.Equal(t, recipients[0], "+15550002", "recipient number")
}

func TestSend_Group(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	asserts.NoError(t, a.Send(context.Background(), "group:grp==", "hi group", core.SendOptions{}), "group Send should succeed")

	req := awaitSend(t, f)
	recipients := req["recipients"].([]any)
	// Asserted as a literal, not recomputed with Send's own formula, so the
	// wire convention is visible and a change to it fails loudly. A group id is
	// base64-encoded twice on purpose: signal-cli reports groupId as base64 of
	// the raw 32-byte id, and signal-cli-rest-api's public form base64s that
	// string again ("group." + base64("grp==") == "group.Z3JwPT0=", which its
	// /v2/send handler decodes once to recover signal-cli's own "grp==").
	var wantRecipient any = "group.Z3JwPT0="
	asserts.Equal(t, recipients[0], wantRecipient, "group recipient should be the base64 REST form")
	asserts.Equal(t, wantRecipient, any(restGroupPrefix+base64.StdEncoding.EncodeToString([]byte("grp=="))), "literal must match the documented encoding")
}

// TestSend_NeedsNoReceiveSocket pins the REST independence: Send works before
// Connect and after Disconnect, since it never touches the receive socket.
func TestSend_NeedsNoReceiveSocket(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	asserts.NoError(t, a.Send(context.Background(), "+15550002", "hi", core.SendOptions{}), "Send before Connect should work")
	awaitSend(t, f)
}

func TestSend_ErrorResponse(t *testing.T) {
	f := startAPI(t)
	f.sendStatus = http.StatusBadRequest
	f.sendBody = `{"error":"Unregistered user"}`
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	err := a.Send(context.Background(), "+15550002", "hi", core.SendOptions{})
	asserts.Error(t, err, "Send should surface a REST error")
	asserts.True(t, strings.Contains(err.Error(), "Unregistered user"), "error should carry the container's message: "+err.Error())
}

func TestSend_ErrorResponseNonJSON(t *testing.T) {
	f := startAPI(t)
	f.sendStatus = http.StatusInternalServerError
	f.sendBody = `boom`
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	err := a.Send(context.Background(), "+15550002", "hi", core.SendOptions{})
	asserts.Error(t, err, "Send should surface a REST error")
	asserts.True(t, strings.Contains(err.Error(), "boom"), "error should fall back to the raw body: "+err.Error())
}

// TestSend_DoesNotFollowRedirect pins the default client's redirect policy. Go
// replays a POST body on 307/308 (a bytes.Reader body populates GetBody), so
// following one would deliver the message, its recipient and the bot's own
// number to whatever host the container named, and the final 200 would make
// Send report success.
func TestSend_DoesNotFollowRedirect(t *testing.T) {
	var relayed atomic.Int64
	relay := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		relayed.Add(1)
	}))
	t.Cleanup(relay.Close)
	container := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, relay.URL+"/v2/send", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(container.Close)

	a, err := newAdapter(Config{BaseURL: container.URL, Number: "+15550001"})
	asserts.NoError(t, err, "newAdapter")

	err = a.Send(context.Background(), "+15550002", "secret", core.SendOptions{})
	asserts.Error(t, err, "a redirected send should fail instead of following")
	asserts.Equal(t, relayed.Load(), int64(0), "the send body must not be replayed at the redirect target")
}

func TestSend_ContextCanceled(t *testing.T) {
	f := startAPI(t)
	f.sendBlock = make(chan struct{})
	t.Cleanup(func() { close(f.sendBlock) })
	a, _ := newAdapter(validConfig())
	reconfigure(t, a, f)

	ctx, cancel := context.WithCancel(context.Background())
	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(ctx, "+15550002", "hi", core.SendOptions{}) }()

	awaitSend(t, f) // request arrived; never answer it
	cancel()
	select {
	case err := <-sendErr:
		asserts.ErrorIs(t, err, context.Canceled, "Send should honor ctx cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not honor cancellation")
	}
}

// TestSend_TimesOutWithoutResponse pins the independent round-trip bound:
// even with a background (never-canceled) context, a request the container
// accepts but never answers must fail after SendTimeout instead of blocking
// the caller forever.
func TestSend_TimesOutWithoutResponse(t *testing.T) {
	f := startAPI(t)
	f.sendBlock = make(chan struct{})
	t.Cleanup(func() { close(f.sendBlock) })
	cfg := validConfig()
	cfg.SendTimeout = 100 * time.Millisecond
	a, err := newAdapter(cfg)
	asserts.NoError(t, err, "newAdapter")
	reconfigure(t, a, f)

	sendErr := make(chan error, 1)
	go func() { sendErr <- a.Send(context.Background(), "+15550002", "hi", core.SendOptions{}) }()

	awaitSend(t, f) // request arrived; never answer it
	select {
	case err := <-sendErr:
		asserts.ErrorIs(t, err, context.DeadlineExceeded, "Send should time out on a silent container")
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not time out")
	}
}

func TestSend_RequestError(t *testing.T) {
	a, _ := newAdapter(validConfig()) // BaseURL points at a dead address
	err := a.Send(context.Background(), "+15550002", "hi", core.SendOptions{})
	asserts.Error(t, err, "Send against a dead container should fail")
	asserts.True(t, strings.Contains(err.Error(), "send request failed"), "error should name the request failure: "+err.Error())
}

func TestReadLoop_RemoteCloseReportsDone(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	loopErr := make(chan error, 1)
	ws := connectAdapter(t, a, captureDeps(nil, nil, loopErr), f)

	_ = ws.Close()

	select {
	case err := <-loopErr:
		asserts.Error(t, err, "an unexpected remote close should surface via Done")
	case <-time.After(2 * time.Second):
		t.Fatal("Done was never called after remote close")
	}
}

func TestDisconnect_CleanNoDone(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	loopErr := make(chan error, 1)
	connectAdapter(t, a, captureDeps(nil, nil, loopErr), f)

	asserts.NoError(t, a.Disconnect(), "clean Disconnect should succeed")

	select {
	case err := <-loopErr:
		t.Fatalf("Done should not fire on intentional Disconnect, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	asserts.NoError(t, a.Disconnect(), "Disconnect when already disconnected is a no-op")
}

// TestReconnect_AfterDisconnect pins that a Signal bot is reusable: unlike the
// whatsmeow adapter (whose store closes on Disconnect), nothing here is
// single-run. It also exercises the per-connection state — the second Connect
// installs a fresh wsConn, and the first connection's teardown must not have
// left anything behind that swallows the second one's messages.
func TestReconnect_AfterDisconnect(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	loopErr := make(chan error, 2)
	deps := captureDeps(&got, done, loopErr)

	ws := connectAdapter(t, a, deps, f)
	writeEnvelope(t, ws, directReceive)
	awaitDispatch(t, done, 1)
	asserts.NoError(t, a.Disconnect(), "first Disconnect")

	// Second run on the same adapter.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	asserts.NoError(t, a.Connect(ctx, deps), "second Connect should succeed on the same adapter")
	t.Cleanup(func() { _ = a.Disconnect() })
	ws2 := acceptWS(t, f)
	asserts.True(t, ws2 != ws, "the second Connect should dial a new socket")

	writeEnvelope(t, ws2, `{"envelope":{"sourceNumber":"+15550002","timestamp":1700000009000,"dataMessage":{"message":"after reconnect"}}}`)
	awaitDispatch(t, done, 1)

	asserts.Equal(t, len(got), 2, "both runs should dispatch")
	asserts.Equal(t, got[1].Content, "after reconnect", "the second run's message should arrive")
	asserts.NoError(t, a.Disconnect(), "second Disconnect")

	select {
	case err := <-loopErr:
		t.Fatalf("Done should not fire on intentional Disconnect, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDisconnect_DrainsInflightDispatch(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, _ *core.Message) {
			entered <- struct{}{}
			<-release
			finished <- struct{}{}
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}
	ws := connectAdapter(t, a, deps, f)

	writeEnvelope(t, ws, directReceive)
	<-entered

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	asserts.NoError(t, a.Disconnect(), "Disconnect should wait for the in-flight dispatch")

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Disconnect returned without draining the in-flight dispatch")
	}
}

// TestDisconnect_DrainTimeout covers the drain-deadline branch: a handler
// stuck past the (test-shrunk) drain budget makes Disconnect return an error
// instead of blocking forever.
func TestDisconnect_DrainTimeout(t *testing.T) {
	old := drainTimeout
	drainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainTimeout = old })

	f := startAPI(t)
	a, _ := newAdapter(validConfig())

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, _ *core.Message) {
			entered <- struct{}{}
			<-release
		},
		Done:       func(error) {},
		Disconnect: func() error { return nil },
	}
	ws := connectAdapter(t, a, deps, f)
	t.Cleanup(func() { close(release) })

	writeEnvelope(t, ws, directReceive)
	<-entered

	err := a.Disconnect()
	asserts.Error(t, err, "Disconnect should surface a drain timeout")
	asserts.True(t, strings.Contains(err.Error(), "drain timed out"), "error should name the drain timeout: "+err.Error())
}

// TestDisconnect_WaitsForReadLoopExit pins the drain's happens-before edge:
// Disconnect must not return until the read loop has stopped consuming
// messages, since only then is the inflight counter complete.
func TestDisconnect_WaitsForReadLoopExit(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	connectAdapter(t, a, captureDeps(nil, nil, nil), f)

	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	asserts.NoError(t, a.Disconnect(), "Disconnect should succeed")

	select {
	case <-c.loopDone:
	default:
		t.Fatal("Disconnect returned before the read loop exited")
	}
}

func TestAttachments(t *testing.T) {
	f := startAPI(t)
	a, _ := newAdapter(validConfig())
	var got []*core.Message
	done := make(chan struct{}, 4)
	ws := connectAdapter(t, a, captureDeps(&got, done, nil), f)

	writeEnvelope(t, ws, attachmentReceive)
	awaitDispatch(t, done, 1)

	atts, err := a.Attachments(got[0])
	asserts.NoError(t, err, "Attachments should succeed")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.True(t, atts[0].IsImage, "image/jpeg should be flagged as image")
	asserts.Equal(t, atts[0].URL, f.ts.URL+"/v1/attachments/att-1", "URL should point at the container's attachment endpoint")
	sa, ok := atts[0].ExtraData.(*Attachment)
	asserts.True(t, ok, "ExtraData should carry the *Attachment")
	asserts.Equal(t, sa.ID, "att-1", "attachment id")
	asserts.Equal(t, sa.ContentType, "image/jpeg", "attachment content type")
	asserts.Equal(t, sa.Filename, "cat.jpg", "attachment filename")
	asserts.Equal(t, sa.Size, int64(1234), "attachment size")
}

// TestAttachments_EscapesID pins that an attachment id is escaped as one path
// segment. The id is sender-influenced, so a "../" in it must not walk the URL
// onto a different container endpoint.
func TestAttachments_EscapesID(t *testing.T) {
	a, _ := newAdapter(Config{BaseURL: "http://127.0.0.1:8080", Number: "+15550001"})
	m := &core.Message{Raw: &Message{Attachments: []Attachment{{ID: "../../v1/register/+15550002"}}}}

	atts, err := a.Attachments(m)
	asserts.NoError(t, err, "Attachments should succeed")
	asserts.Equal(t, atts[0].URL, "http://127.0.0.1:8080/v1/attachments/..%2F..%2Fv1%2Fregister%2F+15550002",
		"a traversal id must stay inside the attachments path segment")
}

func TestAttachments_NonSignalMessage(t *testing.T) {
	a, _ := newAdapter(validConfig())
	atts, err := a.Attachments(&core.Message{Raw: "not signal"})
	asserts.NoError(t, err, "foreign message should not error")
	asserts.True(t, atts == nil, "foreign message should yield nil attachments")
}

func TestRawMessage_NotSignal(t *testing.T) {
	_, ok := RawMessage(&core.Message{Raw: 42})
	asserts.False(t, ok, "RawMessage should report non-signal Raw")
}

// TestBot_EndToEnd drives the full core.Bot lifecycle against the fake
// container: connect, receive a message, reply from the handler, then shut
// down cleanly.
func TestBot_EndToEnd(t *testing.T) {
	f := startAPI(t)
	cfg := validConfig()
	cfg.BaseURL = f.ts.URL
	bot, err := New(cfg)
	asserts.NoError(t, err, "New")

	replied := make(chan struct{})
	// HandleFunc records a bad pattern rather than returning it; this one is
	// valid, and Run below would surface a registration error if it were not.
	bot.HandleFunc("^ping$", func(ctx context.Context, b *core.Bot, m *core.Message) {
		asserts.NoError(t, b.SendMessageContext(ctx, m.ChannelID, "pong"), "reply send")
		close(replied)
	})

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- bot.Run(ctx) }()

	ws := acceptWS(t, f)
	writeEnvelope(t, ws, `{"account":"+15550001","envelope":{"source":"+15550002","sourceNumber":"+15550002","sourceName":"Ada","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"ping"}}}`)

	req := awaitSend(t, f)
	asserts.Equal(t, req["message"], "pong", "handler reply should reach the container")
	recipients := req["recipients"].([]any)
	asserts.Equal(t, recipients[0], "+15550002", "reply should target the sender")

	select {
	case <-replied:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never completed its reply")
	}

	cancel()
	select {
	case err := <-runErr:
		asserts.NoError(t, err, "Run should return nil on a clean ctx cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
