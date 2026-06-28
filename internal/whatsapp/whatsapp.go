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
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
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

	// maxRequestBytes caps the inbound webhook body. The endpoint is public, so
	// this defends against memory-exhaustion from oversized/never-ending bodies;
	// real Cloud API payloads are a few KB.
	maxRequestBytes = 1 << 20 // 1 MiB

	// maxErrorBodyBytes caps how much of a non-2xx Send response body is read into
	// the returned error, bounding memory and log size from an unexpected response.
	maxErrorBodyBytes = 4 << 10 // 4 KiB
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
	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080".
	Addr string
	// Path is the webhook route. Defaults to "/webhook".
	Path string
	// GraphVersion is the Graph API version. Defaults to "v23.0".
	GraphVersion string
	// HTTPClient is used for outbound Cloud API calls. Defaults to an
	// http.Client with a 30s timeout.
	HTTPClient *http.Client
}

// Message is the parsed payload of a message received from the WhatsApp Cloud
// API webhook: the sender (From, which is also the reply target), the message id
// and type, the text (or media caption), and any attached media. AuthorName and
// Timestamp are enriched, not lifted from Raw: AuthorName is correlated from the
// webhook's sibling contacts list and Timestamp is parsed from the message's
// unix-seconds field. Raw holds the original message JSON object for callers that
// need more.
type Message struct {
	From       string
	ID         string
	Type       string
	Text       string
	AuthorName string
	Timestamp  time.Time
	Media      *Media
	Raw        json.RawMessage
}

// Media identifies a media object attached to a WhatsApp message. The Cloud API
// delivers media by ID rather than URL: fetch the bytes with GET /{ID} to obtain
// a short-lived download URL, then GET that URL with your access token.
type Media struct {
	ID       string
	MimeType string
	Filename string
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
	// Accept a bare port ("8080") as shorthand for ":8080" so net.Listen is happy.
	if !strings.Contains(cfg.Addr, ":") {
		cfg.Addr = ":" + cfg.Addr
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	if cfg.GraphVersion == "" {
		cfg.GraphVersion = defaultGraphVersion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &adapter{cfg: cfg, baseURL: graphBaseURL, http: cfg.HTTPClient}, nil
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

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	a.mu.Lock()
	a.srv = srv
	a.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Done(err)
		}
	}()

	// Tear down when the run context is canceled — but only while this server is
	// still the active one. After a disconnect+reconnect, a.srv points at a newer
	// server; a stale watcher firing on the old context must not shut that one down.
	go func() {
		<-ctx.Done()
		a.mu.Lock()
		current := a.srv == srv
		a.mu.Unlock()
		if current {
			_ = deps.Disconnect()
		}
	}()

	return nil
}

// handleVerify answers Meta's webhook-verification handshake: if the verify
// token matches, it echoes hub.challenge; otherwise it returns 403.
func (a *adapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tokenOK := subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(a.cfg.VerifyToken)) == 1
	if q.Get("hub.mode") == "subscribe" && tokenOK {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, q.Get("hub.challenge"))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

// handleWebhook verifies the request signature, then acknowledges the request
// with 200 BEFORE dispatching, so a slow handler can never delay the ack and
// trigger Meta's webhook retry (which would re-deliver the same message). The
// parsed messages are dispatched off the request path using the run context;
// an unauthenticated request gets 403. The body is size-capped because the
// endpoint is public.
func (a *adapter) handleWebhook(ctx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !validateSignature(a.cfg.AppSecret, r.Header.Get(signatureHeader), body) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	messages := parseWebhook(body)
	w.WriteHeader(http.StatusOK)
	if len(messages) == 0 {
		return
	}

	go func() {
		for _, m := range messages {
			deps.Dispatch(ctx, toMessage(m))
		}
	}()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
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
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("whatsapp: send failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Drain the success body so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Attachments returns the media attached to the message's WhatsApp event. The
// Cloud API delivers media by ID rather than URL, so Attachment.URL is empty and
// ExtraData carries the *Media; resolve the bytes with GET /{ID}
// (using your access token) to obtain a short-lived download URL.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	wm, ok := RawMessage(m)
	if !ok || wm == nil || wm.Media == nil {
		return nil, nil
	}
	return []core.Attachment{{
		IsImage:   strings.HasPrefix(wm.Media.MimeType, "image/"),
		ExtraData: wm.Media,
	}}, nil
}

// RawMessage returns the parsed WhatsApp message carried on m, reporting whether
// m originated from WhatsApp.
func RawMessage(m *core.Message) (*Message, bool) {
	wm, ok := m.Raw.(*Message)
	return wm, ok
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
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []json.RawMessage `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// inboundMessage mirrors a single object in value.messages[].
type inboundMessage struct {
	From      string `json:"from"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Text      struct {
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
	for _, m := range []*mediaObject{in.Image, in.Document, in.Video, in.Audio, in.Sticker} {
		if m != nil {
			return m
		}
	}
	return nil
}

// parseWebhook extracts inbound user messages from a Cloud API webhook payload.
// An individual message that fails to parse is logged and skipped rather than
// failing the whole batch, so one bad entry never drops its valid siblings (and
// the request can still be acked with 200 to stop Meta retrying).
func parseWebhook(body []byte) []*Message {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		log.Printf("whatsapp: discarding webhook with unparseable body: %v", err)
		return nil
	}

	var out []*Message
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			names := make(map[string]string, len(change.Value.Contacts))
			for _, c := range change.Value.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for _, raw := range change.Value.Messages {
				m, err := parseMessage(raw)
				if err != nil {
					log.Printf("whatsapp: skipping unparseable message: %v", err)
					continue
				}
				m.AuthorName = names[m.From]
				out = append(out, m)
			}
		}
	}
	return out
}

// parseMessage converts one raw value.messages[] object into a Message,
// using the media caption as the text when the message carries media but no body.
func parseMessage(raw json.RawMessage) (*Message, error) {
	var in inboundMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}

	msg := &Message{
		From:      in.From,
		ID:        in.ID,
		Type:      in.Type,
		Text:      in.Text.Body,
		Timestamp: parseTimestamp(in.Timestamp),
		Raw:       raw,
	}
	if media := in.media(); media != nil {
		msg.Media = &Media{ID: media.ID, MimeType: media.MimeType, Filename: media.Filename}
		if msg.Text == "" {
			msg.Text = media.Caption
		}
	}
	return msg, nil
}

// parseTimestamp converts a Cloud API unix-seconds timestamp string to UTC time,
// returning the zero time when it is empty or non-numeric.
func parseTimestamp(s string) time.Time {
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
}

// toMessage maps a parsed WhatsApp message onto a platform-agnostic Message; the
// sender doubles as the channel, since a reply goes back to the same wa_id.
func toMessage(m *Message) *core.Message {
	return &core.Message{
		ID:         m.ID,
		UserID:     m.From,
		AuthorName: m.AuthorName,
		ChannelID:  m.From,
		Content:    m.Text,
		Timestamp:  m.Timestamp,
		Raw:        m,
	}
}
