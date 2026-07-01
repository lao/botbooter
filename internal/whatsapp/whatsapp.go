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
	"sync/atomic"
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

	// maxMediaMetaBytes caps the getMedia metadata response decoded when resolving
	// an attachment URL. The payload is a small JSON object (url, mime_type, ...);
	// the cap bounds memory from an unexpected response.
	maxMediaMetaBytes = 64 << 10 // 64 KiB
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("whatsapp: missing required config field")

// Config configures a WhatsApp Cloud API bot.
type Config struct {
	// Token is the Cloud API access token sent as a Bearer credential on
	// outbound calls. Prefer a long-lived system-user token; short-lived user
	// tokens expire in ~24h, after which Send fails.
	Token         string
	PhoneNumberID string
	// AppSecret verifies the X-Hub-Signature-256 HMAC on inbound webhook
	// requests. Required: without it the endpoint would accept spoofed payloads.
	AppSecret   string
	VerifyToken string
	// Addr is the local TCP address the webhook server binds, e.g. ":8080". A
	// bare port ("8080") is accepted as shorthand for ":8080".
	Addr         string
	Path         string
	GraphVersion string
	HTTPClient   *http.Client
}

// Message is the parsed payload of a WhatsApp Cloud API webhook message.
// AuthorName and Timestamp are enriched, not lifted from Raw: AuthorName is
// correlated from the webhook's sibling contacts list and Timestamp is parsed
// from the message's unix-seconds field. Raw holds the original message JSON for
// callers that need more.
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

// Media is a media object attached to a WhatsApp message. The Cloud API delivers
// media by ID, not URL: resolve the bytes with GET /{ID} using your access token.
type Media struct {
	ID       string
	MimeType string
	Filename string
}

type adapter struct {
	cfg     Config
	baseURL string
	http    *http.Client

	mu       sync.Mutex
	srv      *http.Server
	inflight atomic.Int64
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

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.Token == "" || cfg.PhoneNumberID == "" || cfg.AppSecret == "" || cfg.VerifyToken == "" || cfg.Addr == "" {
		return nil, fmt.Errorf("%w: Token, PhoneNumberID, AppSecret, VerifyToken and Addr are required", ErrMissingConfig)
	}
	// A bare port ("8080") is shorthand for ":8080"; a host, host:port, :port or
	// IPv6 literal is left for net.Listen to validate.
	if _, err := strconv.Atoi(cfg.Addr); err == nil {
		cfg.Addr = ":" + cfg.Addr
	}
	if cfg.Path == "" {
		cfg.Path = defaultPath
	}
	// A pattern without a leading slash panics ServeMux at Connect; normalize one in.
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	if cfg.GraphVersion == "" {
		cfg.GraphVersion = defaultGraphVersion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &adapter{cfg: cfg, baseURL: graphBaseURL, http: cfg.HTTPClient}, nil
}

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

	// Tear down when the run context is canceled
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

	// Decouple dispatch from the run context's cancellation. On shutdown core
	// cancels runCtx *before* calling Disconnect, whose drainDispatch then waits
	// for this in-flight handler; if the handler threaded ctx into its reply the
	// send would fail with "context canceled" mid-drain, defeating the drain.
	// WithoutCancel keeps the ctx's values but drops its deadline and
	// cancellation, so an already acked message can finish its reply within the
	// drain window. A stuck reply is then bounded only by the outbound HTTP
	// client's timeout — 30s for the default client; set one on a custom
	// cfg.HTTPClient — since ctx no longer aborts it.
	dispatchCtx := context.WithoutCancel(ctx)
	a.inflight.Add(1)
	go func() {
		defer a.inflight.Add(-1)
		for _, m := range messages {
			deps.Dispatch(dispatchCtx, toMessage(m))
		}
	}()
}

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
	err := srv.Shutdown(ctx)
	a.drainDispatch(ctx)
	return err
}

// drainDispatch waits for in-flight dispatch goroutines to finish so an acked
// message is processed rather than dropped at shutdown, bounded by ctx. It polls
// an atomic counter rather than a WaitGroup: the dispatch goroutines are started
// from request handlers that Shutdown may abandon at its deadline, and a
// WaitGroup Add racing that Wait would risk a misuse panic.
func (a *adapter) drainDispatch(ctx context.Context) {
	for a.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

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

// Attachments returns the media attached to m. The Cloud API delivers media by
// ID rather than URL, so Attachment.URL is empty and ExtraData carries the
// *Media; resolve the bytes with GET /{ID} (using your access token).
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	wm, ok := RawMessage(m)
	if !ok || wm == nil {
		return nil, nil
	}
	if wm.Media == nil {
		return []core.Attachment{}, nil
	}
	return []core.Attachment{{
		IsImage:   strings.HasPrefix(wm.Media.MimeType, "image/"),
		ExtraData: wm.Media,
	}}, nil
}

// ResolveAttachmentURL implements [core.AttachmentResolver]: it turns att's
// WhatsApp media id (carried in ExtraData as *Media) into a Cloud API download
// URL via GET /{media-id}, or returns ("", nil) when att carries no media id.
//
// The returned URL is NOT fetchable with a bare GET: download it with an
// Authorization: Bearer <token> header — the same Cloud API token used to send.
// Meta scopes the URL to a short window, so consume it promptly.
func (a *adapter) ResolveAttachmentURL(ctx context.Context, att core.Attachment) (string, error) {
	media, ok := att.ExtraData.(*Media)
	if !ok || media == nil || media.ID == "" {
		return "", nil
	}

	url := fmt.Sprintf("%s/%s/%s", a.baseURL, a.cfg.GraphVersion, media.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return "", fmt.Errorf("whatsapp: resolve media %s failed with status %d: %s",
			media.ID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMediaMetaBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("whatsapp: decode media %s response: %w", media.ID, err)
	}
	// Drain any trailing bytes so the connection can be reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	return out.URL, nil
}

// RawMessage returns the parsed WhatsApp message carried on m, reporting whether
// m originated from WhatsApp.
func RawMessage(m *core.Message) (*Message, bool) {
	wm, ok := m.Raw.(*Message)
	return wm, ok
}

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

type mediaObject struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
}

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

func parseTimestamp(s string) time.Time {
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
}

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
