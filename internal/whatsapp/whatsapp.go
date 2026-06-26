// Package whatsapp is the WhatsApp adapter for botbooter. It receives messages
// from the Meta WhatsApp Business Cloud API over an inbound webhook and sends
// replies back through the Cloud API. It implements core.Adapter.
//
// Unlike the dial-out adapters (Slack, Discord), the Cloud API delivers inbound
// messages as HTTP webhook callbacks, so this adapter runs its own HTTP server:
// Connect binds a listener and serves until the run context is canceled, and
// Disconnect shuts the server down. Bind a local Addr, put a TLS-terminating
// reverse proxy in front, and register the public HTTPS URL in Meta's webhook
// settings.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lao/botbooter/internal/core"
)

const (
	defaultPath         = "/webhook"
	defaultGraphVersion = "v23.0"
	graphBaseURL        = "https://graph.facebook.com"
	signatureHeader     = "X-Hub-Signature-256"
	signaturePrefix     = "sha256="
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("whatsapp: missing required config field")

// Config configures a WhatsApp Cloud API bot.
type Config struct {
	// Token is the Cloud API access token sent as a Bearer credential on
	// outbound calls. Prefer a long-lived system-user token; short-lived user
	// tokens expire in ~24h, after which Send fails.
	Token string
	// PhoneNumberID identifies the sending number and forms part of the send
	// URL.
	PhoneNumberID string
	// AppSecret is the Meta app secret used to verify the X-Hub-Signature-256
	// HMAC on inbound webhook requests. Required: without it the endpoint would
	// accept spoofed payloads.
	AppSecret string
	// VerifyToken is echoed during Meta's GET webhook-verification handshake.
	VerifyToken string
	// Addr is the local TCP address the webhook server binds, e.g. ":8080".
	Addr string
	// Path is the webhook route. Defaults to "/webhook".
	Path string
	// GraphVersion is the Graph API version. Defaults to "v23.0".
	GraphVersion string
	// HTTPClient is used for outbound Cloud API calls. Defaults to
	// http.DefaultClient.
	HTTPClient *http.Client
}

// adapter is the WhatsApp Cloud API implementation of core.Adapter.
type adapter struct {
	cfg     Config
	baseURL string // Cloud API host; overridable in tests.
	http    *http.Client

	mu  sync.Mutex
	srv *http.Server
}

// New creates a WhatsApp bot backed by the Meta Cloud API. It returns
// ErrMissingConfig if a required credential is absent, and otherwise applies
// defaults for Path, GraphVersion and HTTPClient. The webhook server is not
// started until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.WhatsAppBotType, a), nil
}

// newAdapter validates cfg, applies defaults for the optional fields, and builds
// the adapter.
func newAdapter(cfg Config) (*adapter, error) {
	if cfg.Token == "" || cfg.PhoneNumberID == "" || cfg.AppSecret == "" || cfg.VerifyToken == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: Token, PhoneNumberID, AppSecret, VerifyToken and Addr are required", ErrMissingConfig)
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	if cfg.GraphVersion == "" {
		cfg.GraphVersion = defaultGraphVersion
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &adapter{cfg: cfg, baseURL: graphBaseURL, http: httpClient}, nil
}

// Connect starts the webhook HTTP server in the background and returns once the
// listener is bound (so a port conflict surfaces here rather than asynchronously).
// The server runs until ctx is canceled, at which point the run loop tears it
// down via Disconnect.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	mux := http.NewServeMux()
	mux.HandleFunc(a.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.handleVerify(w, r)
		case http.MethodPost:
			a.handleWebhook(ctx, w, r, deps)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	a.mu.Lock()
	a.srv = srv
	a.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Done(err)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = deps.Disconnect()
	}()

	return nil
}

// handleVerify answers Meta's webhook-verification handshake: if the verify
// token matches, it echoes hub.challenge; otherwise it returns 403.
func (a *adapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == a.cfg.VerifyToken {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, q.Get("hub.challenge"))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

// handleWebhook verifies the request signature, parses inbound messages and
// dispatches each one. It acknowledges an authentic request with 200 so Meta
// does not retry; an unauthenticated request gets 403.
func (a *adapter) handleWebhook(ctx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !validateSignature(a.cfg.AppSecret, r.Header.Get(signatureHeader), body) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	messages, err := parseWebhook(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, m := range messages {
		deps.Dispatch(ctx, &core.Message{
			UserID:       m.From,
			ChannelID:    m.From,
			Content:      m.Text,
			WhatsAppData: m,
		})
	}
	w.WriteHeader(http.StatusOK)
}

// Disconnect shuts the webhook server down. It is safe to call when the server
// was never started and is idempotent.
func (a *adapter) Disconnect() error {
	a.mu.Lock()
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(context.Background())
}

// Send delivers text to channelID (a WhatsApp wa_id / phone number) via the
// Cloud API. A non-2xx response — including the common out-of-24h-window or
// missing-template rejection — is returned as an error carrying the response
// body.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                channelID,
		"type":              "text",
		"text":              map[string]any{"preview_url": false, "body": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/%s/%s/messages", a.baseURL, a.cfg.GraphVersion, a.cfg.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("whatsapp: send failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// Attachments returns the media attached to the message's WhatsApp event. The
// Cloud API delivers media by ID rather than URL, so Attachment.URL is empty and
// ExtraData carries the *core.WhatsAppMedia; resolve the bytes with GET /{ID}
// (using your access token) to obtain a short-lived download URL.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	if m.WhatsAppData == nil || m.WhatsAppData.Media == nil {
		return nil, nil
	}
	media := m.WhatsAppData.Media
	return []core.Attachment{{
		IsImage:   strings.HasPrefix(media.MimeType, "image"),
		URL:       "",
		ExtraData: media,
	}}, nil
}

// validateSignature reports whether header is a valid X-Hub-Signature-256 for
// body under secret. The header has the form "sha256=<hex>"; the comparison is
// constant-time.
func validateSignature(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// webhookEnvelope mirrors the parts of the Cloud API webhook payload we use.
// Status callbacks (delivery/read receipts) carry no messages[], so they yield
// no dispatched messages — which is also why the bot never sees its own
// outbound messages echoed back.
type webhookEnvelope struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Messages []json.RawMessage `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// inboundMessage mirrors a single object in value.messages[].
type inboundMessage struct {
	From string `json:"from"`
	ID   string `json:"id"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Image    *mediaObject `json:"image"`
	Document *mediaObject `json:"document"`
	Video    *mediaObject `json:"video"`
	Audio    *mediaObject `json:"audio"`
	Sticker  *mediaObject `json:"sticker"`
}

// mediaObject mirrors a media field (image/document/...) on an inbound message.
type mediaObject struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
}

// media returns the media object for whichever media type the message carries,
// or nil for a non-media message.
func (in inboundMessage) media() *mediaObject {
	switch {
	case in.Image != nil:
		return in.Image
	case in.Document != nil:
		return in.Document
	case in.Video != nil:
		return in.Video
	case in.Audio != nil:
		return in.Audio
	case in.Sticker != nil:
		return in.Sticker
	default:
		return nil
	}
}

// parseWebhook extracts inbound user messages from a Cloud API webhook payload.
func parseWebhook(body []byte) ([]*core.WhatsAppMessage, error) {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}

	var out []*core.WhatsAppMessage
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			for _, raw := range change.Value.Messages {
				m, err := parseMessage(raw)
				if err != nil {
					return nil, err
				}
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// parseMessage converts one raw value.messages[] object into a WhatsAppMessage,
// using the media caption as the text when the message carries media but no body.
func parseMessage(raw json.RawMessage) (*core.WhatsAppMessage, error) {
	var in inboundMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}

	msg := &core.WhatsAppMessage{
		From: in.From,
		ID:   in.ID,
		Type: in.Type,
		Text: in.Text.Body,
		Raw:  raw,
	}
	if media := in.media(); media != nil {
		msg.Media = &core.WhatsAppMedia{ID: media.ID, MimeType: media.MimeType, Filename: media.Filename}
		if msg.Text == "" {
			msg.Text = media.Caption
		}
	}
	return msg, nil
}
