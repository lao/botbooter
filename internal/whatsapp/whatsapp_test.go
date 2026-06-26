package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	asserts.NotNil(t, got[0].WhatsAppData, "WhatsAppData should be set")
	asserts.Equal(t, got[0].WhatsAppData.Type, "text", "message type should be text")
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

	err := a.Send(context.Background(), "123", "hi there")

	asserts.NoError(t, err, "Send should succeed on 200")
	asserts.Equal(t, gotMethod, http.MethodPost, "Send should POST")
	asserts.Equal(t, gotPath, "/v23.0/PNID/messages", "Send should target the messages endpoint")
	asserts.Equal(t, gotAuth, "Bearer tok", "Send should set the bearer token")
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

	err := a.Send(context.Background(), "123", "hi")

	asserts.Error(t, err, "non-2xx response should error")
	asserts.True(t, strings.Contains(err.Error(), "24 hour"), "error should carry the response body")
}

func TestAttachments(t *testing.T) {
	a := testAdapter()

	t.Run("NilData", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{})
		asserts.NoError(t, err, "nil data should not error")
		asserts.Equal(t, len(got), 0, "nil data yields no attachments")
	})

	t.Run("NoMedia", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{WhatsAppData: &core.WhatsAppMessage{Type: "text"}})
		asserts.NoError(t, err, "text message should not error")
		asserts.Equal(t, len(got), 0, "text message yields no attachments")
	})

	t.Run("Image", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{WhatsAppData: &core.WhatsAppMessage{
			Type:  "image",
			Media: &core.WhatsAppMedia{ID: "MID", MimeType: "image/png"},
		}})
		asserts.NoError(t, err, "image message should not error")
		asserts.Equal(t, len(got), 1, "image yields one attachment")
		asserts.True(t, got[0].IsImage, "image attachment should be flagged as image")
		asserts.Equal(t, got[0].URL, "", "URL is empty for Cloud API media")
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
