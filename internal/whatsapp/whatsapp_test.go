package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// validConfig returns a Config with every required field populated.
func validConfig() Config {
	return Config{
		Token:         "tok",
		PhoneNumberID: "PNID",
		AppSecret:     "secret",
		VerifyToken:   "verify",
		Addr:          "127.0.0.1:0",
	}
}

// testAdapter builds an adapter directly (bypassing New) for handler tests,
// with Path defaulted so Connect can register the webhook route.
func testAdapter() *adapter {
	cfg := validConfig()
	cfg.Path = defaultPath
	return &adapter{cfg: cfg, baseURL: graphBaseURL, http: http.DefaultClient}
}

// sign returns the X-Hub-Signature-256 header value for body under secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// captureDeps returns AdapterDeps that append every dispatched message to *got.
// When done is non-nil it receives a signal after each dispatch, so tests can
// synchronize with the adapter's asynchronous (off-request-path) dispatch.
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

// awaitDispatch waits for n dispatch signals, failing the test on timeout.
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

const textWebhook = `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"phone_number_id":"PNID"},"contacts":[{"wa_id":"123","profile":{"name":"Ada"}}],"messages":[{"from":"123","id":"wamid.1","timestamp":"1","type":"text","text":{"body":"hello"}}]}}]}]}`

const imageWebhook = `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"phone_number_id":"PNID"},"messages":[{"from":"123","id":"wamid.2","type":"image","image":{"id":"MID","mime_type":"image/jpeg","caption":"a cat","sha256":"abc"}}]}}]}]}`

const statusWebhook = `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"phone_number_id":"PNID"},"statuses":[{"id":"wamid.1","status":"delivered","recipient_id":"123"}]}}]}]}`

// mixedBatchWebhook holds one valid text message followed by one whose "text"
// field is a string (not an object), which fails to unmarshal into inboundMessage.
const mixedBatchWebhook = `{"object":"whatsapp_business_account","entry":[{"id":"WABA","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"phone_number_id":"PNID"},"messages":[{"from":"123","id":"wamid.1","type":"text","text":{"body":"ok"}},{"from":"456","id":"wamid.2","type":"text","text":"oops"}]}}]}]}`

func TestNew(t *testing.T) {
	bot, err := New(validConfig())

	asserts.NoError(t, err, "New with full config should succeed")
	asserts.NotNil(t, bot, "bot should be initialized")
	asserts.Equal(t, bot.BotType, core.WhatsAppBotType, "bot type should be WhatsApp")
	asserts.Equal(t, bot.BotType.String(), "whatsapp", "bot type string should be whatsapp")
}

func TestNew_Defaults(t *testing.T) {
	a, err := newAdapter(validConfig())

	asserts.NoError(t, err, "newAdapter should succeed")
	asserts.Equal(t, a.cfg.Path, defaultPath, "Path should default")
	asserts.Equal(t, a.cfg.GraphVersion, defaultGraphVersion, "GraphVersion should default")
	asserts.True(t, a.http != nil, "HTTPClient should default")
}

func TestNew_NormalizesBareAddr(t *testing.T) {
	cases := map[string]string{
		"8080":           ":8080",
		":9090":          ":9090",
		"127.0.0.1:8080": "127.0.0.1:8080",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			cfg := validConfig()
			cfg.Addr = in
			a, err := newAdapter(cfg)
			asserts.NoError(t, err, "newAdapter should succeed")
			asserts.Equal(t, a.cfg.Addr, want, "Addr should be normalized")
		})
	}
}

func TestNew_MissingConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"token":       func(c *Config) { c.Token = "" },
		"phoneID":     func(c *Config) { c.PhoneNumberID = "" },
		"appSecret":   func(c *Config) { c.AppSecret = "" },
		"verifyToken": func(c *Config) { c.VerifyToken = "" },
		"addr":        func(c *Config) { c.Addr = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			_, err := New(cfg)
			asserts.ErrorIs(t, err, ErrMissingConfig, "missing field should error")
		})
	}
}

func TestValidateSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "secret"

	asserts.True(t, validateSignature(secret, sign(secret, body), body), "valid signature should pass")
	asserts.False(t, validateSignature(secret, sign("other", body), body), "wrong secret should fail")
	asserts.False(t, validateSignature(secret, sign(secret, []byte("tampered")), body), "tampered body should fail")
	asserts.False(t, validateSignature(secret, "deadbeef", body), "missing prefix should fail")
	asserts.False(t, validateSignature(secret, signaturePrefix+"zz", body), "non-hex should fail")
	asserts.False(t, validateSignature("", sign("", body), body), "empty secret should fail")
}

func TestHandleVerify(t *testing.T) {
	a := testAdapter()

	t.Run("Match", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/webhook?hub.mode=subscribe&hub.verify_token=verify&hub.challenge=42", nil)
		w := httptest.NewRecorder()

		a.handleVerify(w, r)

		asserts.Equal(t, w.Code, http.StatusOK, "matching token should be 200")
		asserts.Equal(t, w.Body.String(), "42", "challenge should be echoed")
	})

	t.Run("WrongToken", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/webhook?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=42", nil)
		w := httptest.NewRecorder()

		a.handleVerify(w, r)

		asserts.Equal(t, w.Code, http.StatusForbidden, "wrong token should be 403")
	})

	t.Run("WrongMode", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/webhook?hub.mode=unsubscribe&hub.verify_token=verify&hub.challenge=42", nil)
		w := httptest.NewRecorder()

		a.handleVerify(w, r)

		asserts.Equal(t, w.Code, http.StatusForbidden, "a valid token with a non-subscribe mode should be 403")
	})
}

func TestHandleWebhook_DispatchesText(t *testing.T) {
	a := testAdapter()
	var got []*core.Message
	body := []byte(textWebhook)

	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(textWebhook))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	w := httptest.NewRecorder()
	done := make(chan struct{}, 1)

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, done))
	awaitDispatch(t, done, 1)

	asserts.Equal(t, w.Code, http.StatusOK, "authentic request should be 200")
	asserts.Equal(t, len(got), 1, "one message should be dispatched")
	asserts.Equal(t, got[0].UserID, "123", "UserID should be the sender")
	asserts.Equal(t, got[0].ChannelID, "123", "ChannelID should be the sender (reply target)")
	asserts.Equal(t, got[0].Content, "hello", "Content should be the text body")
	asserts.Equal(t, got[0].ID, "wamid.1", "ID should be the wamid")
	asserts.Equal(t, got[0].AuthorName, "Ada", "AuthorName should come from the contacts profile")
	asserts.True(t, got[0].Timestamp.Equal(time.Unix(1, 0).UTC()), "Timestamp should be parsed from the webhook")
	wm, ok := RawMessage(got[0])
	asserts.True(t, ok, "Raw should carry the parsed WhatsAppMessage")
	asserts.Equal(t, wm.Type, "text", "message type should be text")
}

// TestHandleWebhook_DispatchesOnDetachedCtx guards the drain: the handler must
// dispatch on exactly the detached context passed in (derived in Connect from the
// run context via WithoutCancel), not the run context — otherwise a handler's reply
// would fail with "context canceled" mid-drain.
func TestHandleWebhook_DispatchesOnDetachedCtx(t *testing.T) {
	a := testAdapter()
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{
		Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c },
	}

	body := []byte(textWebhook)
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(textWebhook))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	a.handleWebhook(dispatchCtx, httptest.NewRecorder(), r, deps)

	select {
	case c := <-gotCtx:
		asserts.NoError(t, c.Err(), "dispatch ctx live before cancel")
		cancelDispatch()
		asserts.ErrorIs(t, c.Err(), context.Canceled, "dispatch rides the passed detached ctx")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestHandleWebhook_BadSignature(t *testing.T) {
	a := testAdapter()
	var got []*core.Message

	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(textWebhook))
	r.Header.Set(signatureHeader, sign("wrong-secret", []byte(textWebhook)))
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusForbidden, "bad signature should be 403")
	asserts.Equal(t, len(got), 0, "no message should be dispatched")
}

func TestHandleWebhook_StatusOnlyIgnored(t *testing.T) {
	a := testAdapter()
	var got []*core.Message
	body := []byte(statusWebhook)

	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(statusWebhook))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusOK, "status callback should be acked 200")
	asserts.Equal(t, len(got), 0, "status callback should dispatch nothing")
}

func TestHandleWebhook_OversizedBody(t *testing.T) {
	a := testAdapter()
	var got []*core.Message

	big := strings.Repeat("a", maxRequestBytes+1)
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(big))
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusBadRequest, "an oversized body should be 400")
	asserts.Equal(t, len(got), 0, "an oversized body dispatches nothing")
}

func TestHandleWebhook_UnparseableBody(t *testing.T) {
	a := testAdapter()
	var got []*core.Message
	body := []byte("not json{")

	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	w := httptest.NewRecorder()

	a.handleWebhook(context.Background(), w, r, captureDeps(&got, nil))

	asserts.Equal(t, w.Code, http.StatusOK, "an authentic but unparseable body is still acked 200")
	asserts.Equal(t, len(got), 0, "an unparseable body dispatches nothing")
}

func TestParseWebhook_Image(t *testing.T) {
	messages := parseWebhook([]byte(imageWebhook))

	asserts.Equal(t, len(messages), 1, "one message expected")
	m := messages[0]
	asserts.Equal(t, m.Type, "image", "type should be image")
	asserts.Equal(t, m.Text, "a cat", "caption should become the text")
	asserts.NotNil(t, m.Media, "media should be set")
	asserts.Equal(t, m.Media.ID, "MID", "media id")
	asserts.Equal(t, m.Media.MimeType, "image/jpeg", "media mime type")
}

func TestParseWebhook_SkipsUnparseable(t *testing.T) {
	messages := parseWebhook([]byte(mixedBatchWebhook))

	asserts.Equal(t, len(messages), 1, "the valid message survives; the bad one is skipped")
	asserts.Equal(t, messages[0].From, "123", "the surviving message is the valid one")
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"unix seconds", "1", time.Unix(1, 0).UTC()},
		{"large value", "1700000000", time.Unix(1700000000, 0).UTC()},
		{"zero unix is epoch not zero-time", "0", time.Unix(0, 0).UTC()},
		{"empty", "", time.Time{}},
		{"non-numeric", "oops", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asserts.True(t, parseTimestamp(c.in).Equal(c.want), "parseTimestamp should match")
		})
	}
}

func TestSend(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var payload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter()
	a.baseURL = srv.URL
	a.http = srv.Client()
	a.cfg.GraphVersion = "v23.0"
	a.cfg.PhoneNumberID = "PNID"
	a.cfg.Token = "tok"

	err := a.Send(context.Background(), "123", "hi there", core.SendOptions{})

	asserts.NoError(t, err, "Send should succeed on 200")
	asserts.Equal(t, gotMethod, http.MethodPost, "Send should POST")
	asserts.Equal(t, gotPath, "/v23.0/PNID/messages", "Send should target the messages endpoint")
	asserts.Equal(t, gotAuth, "Bearer tok", "Send should set the bearer token")
	asserts.Equal(t, payload["messaging_product"], "whatsapp", "payload messaging_product (Meta rejects without it)")
	asserts.Equal(t, payload["recipient_type"], "individual", "payload recipient_type")
	asserts.Equal(t, payload["type"], "text", "payload type")
	asserts.Equal(t, payload["to"], "123", "payload recipient")
	text, _ := payload["text"].(map[string]any)
	asserts.Equal(t, text["body"], "hi there", "payload text body")
}

func TestSend_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"outside the 24 hour window"}}`)
	}))
	defer srv.Close()

	a := testAdapter()
	a.baseURL = srv.URL
	a.http = srv.Client()

	err := a.Send(context.Background(), "123", "hi", core.SendOptions{})

	asserts.Error(t, err, "non-2xx response should error")
	asserts.True(t, strings.Contains(err.Error(), "24 hour"), "error should carry the response body")
}

func TestSend_Threading(t *testing.T) {
	newSrv := func(payload *map[string]any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(payload)
			w.WriteHeader(http.StatusOK)
		}))
	}
	newAdapter := func(srv *httptest.Server) *adapter {
		a := testAdapter()
		a.baseURL = srv.URL
		a.http = srv.Client()
		return a
	}

	t.Run("InReplyToQuotesMessageID", func(t *testing.T) {
		var payload map[string]any
		srv := newSrv(&payload)
		defer srv.Close()

		err := newAdapter(srv).Send(context.Background(), "123", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "123", ID: "wamid.X"}})

		asserts.NoError(t, err, "Send should succeed on 200")
		ctx, _ := payload["context"].(map[string]any)
		asserts.Equal(t, ctx["message_id"], "wamid.X", "context.message_id quotes the message")
	})

	t.Run("WithThreadIDQuotesRawID", func(t *testing.T) {
		var payload map[string]any
		srv := newSrv(&payload)
		defer srv.Close()

		err := newAdapter(srv).Send(context.Background(), "123", "hi", core.SendOptions{ThreadID: "wamid.RAW"})

		asserts.NoError(t, err, "Send")
		ctx, _ := payload["context"].(map[string]any)
		asserts.Equal(t, ctx["message_id"], "wamid.RAW", "context.message_id quotes the raw ThreadID")
	})

	t.Run("NoAnchorOmitsContext", func(t *testing.T) {
		var payload map[string]any
		srv := newSrv(&payload)
		defer srv.Close()

		err := newAdapter(srv).Send(context.Background(), "123", "hi", core.SendOptions{})

		asserts.NoError(t, err, "Send with no anchor")
		_, hasContext := payload["context"]
		asserts.True(t, !hasContext, "no context key when there is no anchor")
	})

	t.Run("ThreadIDWinsOverReplyTo", func(t *testing.T) {
		var payload map[string]any
		srv := newSrv(&payload)
		defer srv.Close()

		err := newAdapter(srv).Send(context.Background(), "123", "hi",
			core.SendOptions{ThreadID: "wamid.RAW", ReplyTo: &core.Message{ChannelID: "123", ID: "wamid.X"}})

		asserts.NoError(t, err, "Send")
		ctx, _ := payload["context"].(map[string]any)
		asserts.Equal(t, ctx["message_id"], "wamid.RAW", "explicit ThreadID wins over ReplyTo.ID")
	})
}

// The adapter must satisfy the optional AttachmentResolver so the unified
// (*core.Bot).ResolveAttachmentURL routes to it rather than the default
// passthrough (guards the pointer-receiver method-set trap).
var _ core.AttachmentResolver = (*adapter)(nil)

func TestResolveAttachmentURL(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"url":"https://lookaside.fbsbx.com/media/blob","mime_type":"image/jpeg"}`)
	}))
	defer srv.Close()

	a := testAdapter()
	a.baseURL = srv.URL
	a.http = srv.Client()
	a.cfg.GraphVersion = "v23.0"
	a.cfg.Token = "tok"

	// Resolve through the unified entry point to prove the adapter is routed to.
	b := core.New(core.WhatsAppBotType, a)
	url, err := b.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: &Media{ID: "MID", MimeType: "image/jpeg"}})

	asserts.NoError(t, err, "resolve should succeed on 200")
	asserts.Equal(t, url, "https://lookaside.fbsbx.com/media/blob", "returns the Cloud API media url")
	asserts.Equal(t, gotMethod, http.MethodGet, "resolve should GET")
	asserts.Equal(t, gotPath, "/v23.0/MID", "resolve should target /{graph-version}/{media-id}")
	asserts.Equal(t, gotAuth, "Bearer tok", "resolve should set the bearer token")
}

func TestResolveAttachmentURL_NoMediaID(t *testing.T) {
	a := testAdapter()
	a.http = nil // a no-op resolve must short-circuit before any HTTP call

	cases := map[string]any{
		"WrongExtraData": "not-media",
		"NilMedia":       (*Media)(nil),
		"EmptyID":        &Media{ID: ""},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			url, err := a.ResolveAttachmentURL(context.Background(), core.Attachment{ExtraData: extra})
			asserts.NoError(t, err, "a missing media id is not an error")
			asserts.Equal(t, url, "", "a missing media id yields no URL")
		})
	}
}

func TestResolveAttachmentURL_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid access token"}}`)
	}))
	defer srv.Close()

	a := testAdapter()
	a.baseURL = srv.URL
	a.http = srv.Client()

	url, err := a.ResolveAttachmentURL(context.Background(), core.Attachment{ExtraData: &Media{ID: "MID"}})

	asserts.Error(t, err, "a non-2xx response should error")
	asserts.Equal(t, url, "", "no URL is returned on error")
	asserts.True(t, strings.Contains(err.Error(), "invalid access token"), "error carries the response body")
}

func TestAttachments(t *testing.T) {
	a := testAdapter()

	t.Run("NilData", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{})
		asserts.NoError(t, err, "nil data should not error")
		asserts.Equal(t, len(got), 0, "nil data yields no attachments")
	})

	t.Run("TypedNilRaw", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{Raw: (*Message)(nil)})
		asserts.NoError(t, err, "typed-nil Raw should not panic or error")
		asserts.Equal(t, len(got), 0, "typed-nil Raw yields no attachments")
	})

	t.Run("NoMedia", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{Raw: &Message{Type: "text"}})
		asserts.NoError(t, err, "text message should not error")
		asserts.Equal(t, len(got), 0, "text message yields no attachments")
		asserts.True(t, got != nil, "no-media yields a non-nil empty slice, mirroring sibling adapters")
	})

	t.Run("Image", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{Raw: &Message{
			Type:  "image",
			Media: &Media{ID: "MID", MimeType: "image/png"},
		}})
		asserts.NoError(t, err, "image message should not error")
		asserts.Equal(t, len(got), 1, "image yields one attachment")
		asserts.True(t, got[0].IsImage, "image attachment should be flagged as image")
		asserts.Equal(t, got[0].URL, "", "URL is empty for Cloud API media")
		media, ok := got[0].ExtraData.(*Media)
		asserts.True(t, ok, "ExtraData should carry the *Media")
		asserts.Equal(t, media.ID, "MID", "ExtraData media id")
		asserts.Equal(t, media.MimeType, "image/png", "ExtraData media mime type")
	})
}

func TestConnectDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := testAdapter()
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

	a := testAdapter() // cfg.Addr is 127.0.0.1:0
	b := core.New(core.WhatsAppBotType, a)
	deps := core.AdapterDeps{Done: func(error) {}, Disconnect: func() error { return nil }}

	asserts.Equal(t, Addr(b), "", "no bound address before Connect")
	asserts.NoError(t, a.Connect(ctx, deps), "Connect")

	_, port, err := net.SplitHostPort(Addr(b))
	asserts.NoError(t, err, "Addr returns host:port after Connect")
	asserts.True(t, port != "" && port != "0", "OS-assigned port is resolved from :0")

	asserts.NoError(t, a.Disconnect(), "Disconnect")
	asserts.Equal(t, Addr(b), "", "bound address cleared after Disconnect")
}

// TestDisconnect_CancelsDispatchContext guards the leak fix: Disconnect must cancel
// the connection's detached dispatch context after the drain, so a handler blocked
// on ctx.Done() cannot leak past shutdown. The connection's cancel is wired by hand
// so the test controls the exact context the handler dispatches on.
func TestDisconnect_CancelsDispatchContext(t *testing.T) {
	a := testAdapter()
	dispatchCtx, cancelDispatch := context.WithCancel(context.WithoutCancel(context.Background()))
	// srv must be non-nil so Disconnect runs its teardown rather than early-returning;
	// an unstarted server's Shutdown returns immediately.
	a.mu.Lock()
	a.srv = &http.Server{}
	a.detachedCancel = cancelDispatch
	a.mu.Unlock()

	gotCtx := make(chan context.Context, 1)
	deps := core.AdapterDeps{Dispatch: func(c context.Context, _ *core.Message) { gotCtx <- c }}
	body := []byte(textWebhook)
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(textWebhook))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	a.handleWebhook(dispatchCtx, httptest.NewRecorder(), r, deps)

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
// keep the default suite fast (mirrors the slow-shutdown drain timing test).
func TestDisconnect_DrainTimeoutReturnsError(t *testing.T) {
	if os.Getenv("BOTBOOTER_WHATSAPP_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_WHATSAPP_DRAIN_TIMING_TEST to run the ~5s drain-timeout test")
	}
	a := testAdapter()
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

	body := []byte(textWebhook)
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(textWebhook))
	r.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
	a.handleWebhook(context.Background(), httptest.NewRecorder(), r, deps)

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	asserts.Error(t, a.Disconnect(), "drain timeout should surface an error, not a clean nil")
}

func TestDrainDispatch_ContextCancel(t *testing.T) {
	a := testAdapter()
	a.inflight.Add(1) // never decremented
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.drainDispatch(ctx) // must return promptly on a canceled ctx
	asserts.Equal(t, a.inflight.Load(), int64(1), "drain returns on ctx cancel without waiting")
}

func TestDrainDispatch_WaitsForInflight(t *testing.T) {
	a := testAdapter()
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

func TestConnect_StaleWatcherIgnoresReplacedServer(t *testing.T) {
	a := testAdapter()
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

func TestConnect_BindError(t *testing.T) {
	a := testAdapter()
	a.cfg.Addr = "127.0.0.1:99999" // port out of range

	err := a.Connect(context.Background(), core.AdapterDeps{})

	asserts.Error(t, err, "an invalid bind address should fail synchronously")
}

func TestDisconnect_NeverConnected(t *testing.T) {
	a := testAdapter()
	asserts.NoError(t, a.Disconnect(), "Disconnect before Connect should be safe")
}

// TestDisconnect_SlowShutdownDoesNotStarveDrain is the end-to-end regression
// for the two-budget Disconnect: a slow client keeps a connection active so
// srv.Shutdown burns its full deadline, and the drain must still get its own
// deadline to let an already-acked in-flight dispatch finish. With one shared
// context (the bug) drain would inherit ~0s and abandon the goroutine. It takes
// ~6s of real time, so it is gated behind an env var to keep the default suite
// hermetic and fast.
func TestDisconnect_SlowShutdownDoesNotStarveDrain(t *testing.T) {
	if os.Getenv("BOTBOOTER_WHATSAPP_DRAIN_TIMING_TEST") == "" {
		t.Skip("set BOTBOOTER_WHATSAPP_DRAIN_TIMING_TEST to run the ~6s slow-shutdown drain timing test")
	}

	// Bind a concrete free port so raw client connections can target it; Connect
	// does not expose its listener address.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	asserts.NoError(t, err, "probe listen")
	addr := probe.Addr().String()
	asserts.NoError(t, probe.Close(), "probe close")

	a := testAdapter()
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
	body := []byte(textWebhook)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+defaultPath, bytes.NewReader(body))
	asserts.NoError(t, err, "build webhook request")
	req.Header.Set(signatureHeader, sign(a.cfg.AppSecret, body))
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
	_, err = slow.Write([]byte("POST " + defaultPath + " HTTP/1.1\r\nHost: x\r\n" +
		signatureHeader + ": bad\r\nContent-Length: 1000000\r\n\r\n"))
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
