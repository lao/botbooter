// Package signal is the Signal adapter for botbooter. It talks to a
// signal-cli-rest-api container (https://github.com/bbernhard/signal-cli-rest-api)
// running in json-rpc mode.
//
// Signal has no official bot API, so the container owns the Signal protocol
// and this adapter speaks two channels to it: inbound messages arrive over the
// container's receive WebSocket (GET /v1/receive/{number}, upgraded), and
// replies go out as plain REST calls (POST /v2/send). The container's API is
// unauthenticated — bind it to localhost or a private network only.
package signal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lao/botbooter/internal/core"
)

// groupChannelPrefix marks a core ChannelID as a Signal group id rather than a
// phone number. Inbound group messages carry it and Send converts it to the
// REST API's group-recipient form.
const groupChannelPrefix = "group:"

// restGroupPrefix is the recipient prefix signal-cli-rest-api expects for
// groups: "group." + base64(internal group id).
const restGroupPrefix = "group."

// Shutdown budgets: how long Disconnect waits for the read loop to exit and
// for in-flight dispatches to drain. Package variables so tests can shrink them.
var (
	loopExitTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultSendTimeout = 30 * time.Second

	// maxMessageBytes caps a single WebSocket message from the container.
	// Envelopes are small (attachments arrive as ids, not inline bytes); the
	// cap only bounds a misbehaving peer.
	maxMessageBytes = 1 << 20 // 1 MiB
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("signal: missing required config field")

// Config configures a Signal bot backed by a signal-cli-rest-api container.
type Config struct {
	// BaseURL is the container's API base URL, e.g. "http://127.0.0.1:8080".
	// http and https are accepted (the receive socket dials ws/wss
	// accordingly). Required.
	BaseURL string
	// Number is the bot's own registered E.164 number, e.g. "+15550001". It
	// routes the receive socket, names the sender on outbound messages, and
	// drops the bot's own messages. Required.
	Number string
	// DialTimeout bounds the receive-socket handshake on Connect; defaults to
	// 10s when zero.
	DialTimeout time.Duration
	// SendTimeout bounds each Send's REST round-trip regardless of the
	// caller's context; defaults to 30s when zero.
	SendTimeout time.Duration
	// HTTPClient is the client for outbound REST calls; defaults to a plain
	// client (each call is already bounded by SendTimeout).
	HTTPClient *http.Client
}

// Message is the parsed payload of a signal-cli receive envelope. Raw holds
// the envelope's original JSON for callers that need more.
type Message struct {
	Source      string
	SourceUUID  string
	SourceName  string
	Timestamp   time.Time
	Text        string
	GroupID     string
	Attachments []Attachment
	Raw         json.RawMessage
}

// Attachment is a file attached to a Signal message, delivered by id. The
// container serves the bytes at GET /v1/attachments/{id}.
type Attachment struct {
	ID          string
	ContentType string
	Filename    string
	Size        int64
}

type adapter struct {
	cfg    Config
	wsURL  string // receive socket, derived from BaseURL + Number
	client *http.Client

	mu   sync.Mutex
	conn *wsConn
}

// wsConn holds one live receive-socket's state. Connect creates a fresh one
// and Disconnect drops it, so nothing leaks across reconnects.
type wsConn struct {
	ws *websocket.Conn

	// cancelDispatch aborts this connection's dispatch goroutines; Disconnect
	// calls it after the drain window so a stuck handler cannot leak.
	cancelDispatch context.CancelFunc
	inflight       atomic.Int64

	// closed marks an intentional teardown so the read loop's resulting error
	// is suppressed instead of reported via deps.Done.
	closed atomic.Bool
	// loopDone is closed when the read loop returns. Disconnect waits on it
	// after closing the socket: Close only stops future reads, and a message
	// already read keeps dispatching (and incrementing inflight) until the
	// loop exits, so the loop's exit is the happens-before edge that makes
	// the drain counter complete.
	loopDone chan struct{}
}

// New creates a Signal bot that talks to the signal-cli-rest-api container at
// cfg.BaseURL. It returns ErrMissingConfig if BaseURL or Number is empty, or
// an error for an unparseable BaseURL; the container is not dialed until the
// bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.SignalBotType, a), nil
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("%w: BaseURL is required", ErrMissingConfig)
	}
	if cfg.Number == "" {
		return nil, fmt.Errorf("%w: Number is required", ErrMissingConfig)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	wsURL, err := receiveSocketURL(cfg.BaseURL, cfg.Number)
	if err != nil {
		return nil, err
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = defaultSendTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &adapter{cfg: cfg, wsURL: wsURL, client: client}, nil
}

// receiveSocketURL derives the ws/wss receive endpoint from the http/https
// base URL.
func receiveSocketURL(baseURL, number string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("signal: invalid BaseURL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("signal: BaseURL scheme must be http or https, got %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/receive/" + url.PathEscape(number)
	return u.String(), nil
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	dialer := websocket.Dialer{HandshakeTimeout: a.cfg.DialTimeout}
	ws, resp, err := dialer.DialContext(ctx, a.wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return fmt.Errorf("signal: receive socket dial failed (is the container running in json-rpc mode?): %w", err)
	}
	ws.SetReadLimit(maxMessageBytes)

	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets in-flight handler processing finish during the
	// shutdown drain, and WithCancel lets Disconnect abort stragglers after
	// it. Replies keep working during the drain — they travel over the REST
	// client, not the receive socket Disconnect closes.
	dispatchCtx, cancelDispatch := context.WithCancel(context.WithoutCancel(ctx))
	c := &wsConn{
		ws:             ws,
		cancelDispatch: cancelDispatch,
		loopDone:       make(chan struct{}),
	}

	a.mu.Lock()
	a.conn = c
	a.mu.Unlock()

	go a.readLoop(c, dispatchCtx, deps)

	// Tear down when the run context is canceled.
	go func() {
		<-ctx.Done()
		a.mu.Lock()
		current := a.conn == c
		a.mu.Unlock()
		if current {
			_ = deps.Disconnect()
		}
	}()

	return nil
}

// readLoop consumes receive envelopes from the WebSocket until the connection
// dies, dispatching each user message. A malformed message is logged and
// skipped.
func (a *adapter) readLoop(c *wsConn, dispatchCtx context.Context, deps core.AdapterDeps) {
	defer close(c.loopDone)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			if c.closed.Load() {
				return // intentional Disconnect; core settles the lifecycle itself
			}
			deps.Done(fmt.Errorf("signal: receive socket closed: %w", err))
			return
		}
		a.handleReceive(c, dispatchCtx, data, deps)
	}
}

// receiveEnvelope mirrors the subset of a signal-cli receive payload the
// adapter consumes. Only envelopes carrying a dataMessage are user messages;
// receipts, typing indicators and sync messages have none and are skipped.
type receiveEnvelope struct {
	Account  string `json:"account"`
	Envelope struct {
		Source       string `json:"source"`
		SourceNumber string `json:"sourceNumber"`
		SourceUUID   string `json:"sourceUuid"`
		SourceName   string `json:"sourceName"`
		Timestamp    int64  `json:"timestamp"`
		DataMessage  *struct {
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
			Attachments []struct {
				ID          string `json:"id"`
				ContentType string `json:"contentType"`
				Filename    string `json:"filename"`
				Size        int64  `json:"size"`
			} `json:"attachments"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

func (a *adapter) handleReceive(c *wsConn, dispatchCtx context.Context, data []byte, deps core.AdapterDeps) {
	var p receiveEnvelope
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("signal: skipping unparseable receive payload: %v", err)
		return
	}
	if p.Envelope.DataMessage == nil {
		return // receipt, typing, sync, … — not a user message
	}

	source := p.Envelope.SourceNumber
	if source == "" {
		source = p.Envelope.Source
	}
	// Drop the bot's own messages to avoid reply loops.
	if source == a.cfg.Number {
		return
	}

	dm := p.Envelope.DataMessage
	m := &Message{
		Source:     source,
		SourceUUID: p.Envelope.SourceUUID,
		SourceName: p.Envelope.SourceName,
		Timestamp:  time.UnixMilli(p.Envelope.Timestamp).UTC(),
		Text:       dm.Message,
		// data is this message's own buffer (ReadMessage allocates per
		// message), so it is already private to this envelope.
		Raw: data,
	}
	if dm.GroupInfo != nil {
		m.GroupID = dm.GroupInfo.GroupID
	}
	for _, att := range dm.Attachments {
		m.Attachments = append(m.Attachments, Attachment(att))
	}

	// Dispatch off the read loop so a slow handler cannot stall the socket; the
	// counter lets Disconnect drain in-flight messages instead of dropping them.
	// Account on the loop's own connection, not a.conn: a reconnect can install
	// a newer connection while this loop still flushes buffered messages, and
	// its drain must not be charged for work it never spawned.
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Add(-1)
		deps.Dispatch(dispatchCtx, toCoreMessage(m))
	}()
}

func toCoreMessage(m *Message) *core.Message {
	channelID := m.Source
	if m.GroupID != "" {
		channelID = groupChannelPrefix + m.GroupID
	}
	return &core.Message{
		ID:         strconv.FormatInt(m.Timestamp.UnixMilli(), 10),
		UserID:     m.Source,
		AuthorName: m.SourceName,
		ChannelID:  channelID,
		Content:    m.Text,
		Timestamp:  m.Timestamp,
		Raw:        m,
	}
}

// Send delivers text as a POST /v2/send REST call, bounded by cfg.SendTimeout.
// channelID is a recipient number, or a group id carrying the "group:" prefix
// as produced on inbound group messages. Send needs no live receive socket —
// the REST endpoint stands on its own.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	// Bound the round-trip independently of the caller: Bot.SendMessage passes
	// a background context, and a request the container accepts but never
	// answers would otherwise block that caller forever.
	ctx, cancel := context.WithTimeout(ctx, a.cfg.SendTimeout)
	defer cancel()

	recipient := channelID
	if internalID, ok := strings.CutPrefix(channelID, groupChannelPrefix); ok {
		// The REST API addresses groups as "group." + base64 of the internal
		// group id signal-cli reports on inbound envelopes.
		recipient = restGroupPrefix + base64.StdEncoding.EncodeToString([]byte(internalID))
	}

	body, err := json.Marshal(map[string]any{
		"number":     a.cfg.Number,
		"recipients": []string{recipient},
		"message":    text,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v2/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("signal: send request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("signal: send failed: %s: %s", resp.Status, restErrorMessage(resp.Body))
	}
	return nil
}

// restErrorMessage extracts the container's {"error": "..."} payload from a
// failed response, falling back to the raw (truncated) body.
func restErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 4<<10))
	if err != nil || len(raw) == 0 {
		return "(no error body)"
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return string(raw)
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return nil
	}

	// Mark intentional before closing so the read loop suppresses the
	// resulting error instead of reporting it via deps.Done.
	c.closed.Store(true)
	_ = c.ws.Close()

	// Let the read loop finish the message it may already hold before the
	// drain: every inflight.Add happens inside the loop, so waiting here is
	// what guarantees the drain counter observes all of them. The loop cannot
	// hang — reads fail after Close and handleReceive never blocks.
	select {
	case <-c.loopDone:
	case <-time.After(loopExitTimeout):
		log.Print("signal: read loop did not exit within the shutdown budget")
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	c.drainDispatch(drainCtx)

	var drainErr error
	if n := c.inflight.Load(); n > 0 {
		log.Printf("signal: drain deadline reached; canceling %d in-flight dispatch(es)", n)
		drainErr = fmt.Errorf("signal: dispatch drain timed out with %d in-flight dispatch(es)", n)
	}

	// Clear the shared field only if a reconnect has not installed a newer
	// connection; either way, cancel THIS connection's dispatch context after
	// the drain so a stuck handler cannot leak past shutdown.
	a.mu.Lock()
	if a.conn == c {
		a.conn = nil
	}
	a.mu.Unlock()
	c.cancelDispatch()

	return drainErr
}

// drainDispatch waits, bounded by ctx, for in-flight dispatch goroutines so a
// received message finishes processing rather than being dropped at shutdown.
// Replies still work during the drain — they go over REST, not the closed
// receive socket. It polls an atomic counter rather than a WaitGroup: an Add
// racing Wait would risk a misuse panic.
func (c *wsConn) drainDispatch(ctx context.Context) {
	for c.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Attachments returns the files attached to m. The container serves attachment
// bytes at /v1/attachments/{id}, so URL points there; ExtraData carries the
// *Attachment with the id, content type, filename and size.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	sm, ok := RawMessage(m)
	if !ok || sm == nil {
		return nil, nil
	}
	out := make([]core.Attachment, 0, len(sm.Attachments))
	for i := range sm.Attachments {
		att := &sm.Attachments[i]
		out = append(out, core.Attachment{
			IsImage:   strings.HasPrefix(att.ContentType, "image/"),
			URL:       a.cfg.BaseURL + "/v1/attachments/" + url.PathEscape(att.ID),
			ExtraData: att,
		})
	}
	return out, nil
}

// RawMessage returns the parsed Signal message carried on m, reporting whether
// m originated from Signal.
func RawMessage(m *core.Message) (*Message, bool) {
	sm, ok := m.Raw.(*Message)
	return sm, ok
}
