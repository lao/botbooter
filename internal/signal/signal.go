// Package signal is the Signal adapter for botbooter. It talks to a signal-cli
// daemon (https://github.com/AsamK/signal-cli) over its JSON-RPC TCP socket
// (`signal-cli daemon --tcp <addr>`) and implements core.Adapter.
//
// Signal has no official bot API, so the daemon owns the Signal protocol and
// this adapter speaks newline-delimited JSON-RPC 2.0 to it over plain TCP:
// inbound messages arrive as "receive" notifications on the long-lived
// connection, and replies go out as "send" requests on the same connection.
// The socket is unauthenticated — bind the daemon to localhost or a private
// network only.
package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lao/botbooter/internal/core"
)

// groupChannelPrefix marks a core ChannelID as a Signal group id rather than a
// phone number. Inbound group messages carry it and Send strips it back off.
const groupChannelPrefix = "group:"

// Shutdown budgets: how long Disconnect waits for the read loop to exit and
// for in-flight dispatches to drain. Package variables so tests can shrink them.
var (
	loopExitTimeout = 5 * time.Second
	drainTimeout    = 5 * time.Second
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultSendTimeout = 30 * time.Second

	// maxLineBytes caps a single JSON-RPC frame from the daemon. Envelopes are
	// small (attachments arrive as ids, not inline bytes); the cap only bounds a
	// misbehaving peer.
	maxLineBytes = 1 << 20 // 1 MiB
)

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("signal: missing required config field")

// ErrNotConnected is returned by Send when the adapter has no live daemon
// connection to write to.
var ErrNotConnected = errors.New("signal: not connected")

// Config configures a Signal bot backed by a signal-cli daemon.
type Config struct {
	// Address is the TCP address of the signal-cli daemon's JSON-RPC socket,
	// e.g. "127.0.0.1:7583" (the daemon's default). Required.
	Address string
	// Account is the bot's own E.164 number, e.g. "+15550001". It is passed on
	// outbound requests (required by multi-account daemons) and used to drop
	// the bot's own messages. Optional for single-account daemons; without it,
	// self-message filtering falls back to the account the daemon reports.
	Account string
	// DialTimeout bounds the Connect dial; defaults to 10s when zero.
	DialTimeout time.Duration
	// SendTimeout bounds each Send's request/response round-trip regardless of
	// the caller's context; defaults to 30s when zero.
	SendTimeout time.Duration
}

// Message is the parsed payload of a signal-cli receive envelope. Raw holds
// the notification's original params JSON for callers that need more.
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

// Attachment is a file attached to a Signal message. signal-cli delivers
// attachments by id and stores the bytes in its own data directory
// ($XDG_DATA_HOME/signal-cli/attachments/<id>); this adapter carries the
// metadata only.
type Attachment struct {
	ID          string
	ContentType string
	Filename    string
	Size        int64
}

type adapter struct {
	cfg Config

	mu   sync.Mutex
	conn *rpcConn
}

// rpcConn holds one live daemon connection's state. Connect creates a fresh
// one and Disconnect drops it, so nothing leaks across reconnects.
type rpcConn struct {
	net net.Conn

	writeMu sync.Mutex

	pendMu  sync.Mutex
	pending map[uint64]chan rpcResult
	nextID  atomic.Uint64

	// cancelDispatch aborts this connection's dispatch goroutines; Disconnect
	// calls it after the drain window so a stuck handler cannot leak.
	cancelDispatch context.CancelFunc
	inflight       atomic.Int64

	// closed marks an intentional teardown so the read loop's resulting error
	// is suppressed instead of reported via deps.Done.
	closed atomic.Bool
	// loopDone is closed when the read loop returns. Disconnect waits on it
	// after closing the socket: Close only stops future reads, and frames
	// already buffered in the scanner keep dispatching (and incrementing
	// inflight) until the loop exits, so the loop's exit is the happens-before
	// edge that makes the drain counter complete.
	loopDone chan struct{}
}

type rpcResult struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// New creates a Signal bot that talks to a signal-cli daemon at cfg.Address.
// It returns ErrMissingConfig if Address is empty; the daemon is not dialed
// until the bot connects.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.SignalBotType, a), nil
}

func newAdapter(cfg Config) (*adapter, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("%w: Address is required", ErrMissingConfig)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = defaultSendTimeout
	}
	return &adapter{cfg: cfg}, nil
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	netConn, err := net.DialTimeout("tcp", a.cfg.Address, a.cfg.DialTimeout)
	if err != nil {
		return err
	}

	// One detached, cancelable context per connection parents all dispatch:
	// WithoutCancel lets an acked reply finish during the shutdown drain, and
	// WithCancel lets Disconnect abort stragglers after it.
	dispatchCtx, cancelDispatch := context.WithCancel(context.WithoutCancel(ctx))
	c := &rpcConn{
		net:            netConn,
		pending:        make(map[uint64]chan rpcResult),
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

// readLoop consumes newline-delimited JSON-RPC frames from the daemon until
// the connection dies, routing responses to pending Sends and "receive"
// notifications to dispatch. A malformed line is logged and skipped.
func (a *adapter) readLoop(c *rpcConn, dispatchCtx context.Context, deps core.AdapterDeps) {
	defer close(c.loopDone)
	sc := bufio.NewScanner(c.net)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		var frame struct {
			ID     *uint64         `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			log.Printf("signal: skipping unparseable frame: %v", err)
			continue
		}
		switch {
		case frame.ID != nil:
			c.settle(*frame.ID, rpcResult{result: frame.Result, err: frame.Error})
		case frame.Method == "receive":
			a.handleReceive(c, dispatchCtx, frame.Params, deps)
		}
	}

	// The connection is gone either way; unblock every pending Send.
	c.failPending()

	if c.closed.Load() {
		return // intentional Disconnect; core settles the lifecycle itself
	}
	err := sc.Err()
	if err == nil {
		err = errors.New("signal: daemon closed the connection")
	}
	deps.Done(err)
}

// receiveParams mirrors the subset of a signal-cli "receive" notification the
// adapter consumes. Only envelopes carrying a dataMessage are user messages;
// receipts, typing indicators and sync messages have none and are skipped.
type receiveParams struct {
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

func (a *adapter) handleReceive(c *rpcConn, dispatchCtx context.Context, params json.RawMessage, deps core.AdapterDeps) {
	var p receiveParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Printf("signal: skipping unparseable receive params: %v", err)
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
	self := a.cfg.Account
	if self == "" {
		self = p.Account
	}
	if source == self {
		return
	}

	dm := p.Envelope.DataMessage
	m := &Message{
		Source:     source,
		SourceUUID: p.Envelope.SourceUUID,
		SourceName: p.Envelope.SourceName,
		Timestamp:  time.UnixMilli(p.Envelope.Timestamp).UTC(),
		Text:       dm.Message,
		// params was copied out of the scanner buffer by RawMessage.UnmarshalJSON,
		// so it is already private to this frame.
		Raw: params,
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
	// a newer connection while this loop still flushes buffered frames, and its
	// drain must not be charged for work it never spawned.
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

func (a *adapter) currentConn() *rpcConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

// Send delivers text over the daemon as a JSON-RPC "send" request and waits
// for the matching response, bounded by cfg.SendTimeout. channelID is a
// recipient number, or a group id carrying the "group:" prefix as produced on
// inbound group messages.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	c := a.currentConn()
	if c == nil {
		return ErrNotConnected
	}

	// Bound the round-trip independently of the caller: Bot.SendMessage passes
	// a background context, and a request the daemon accepts but never answers
	// would otherwise block that caller forever.
	ctx, cancel := context.WithTimeout(ctx, a.cfg.SendTimeout)
	defer cancel()

	params := map[string]any{"message": text}
	if groupID, ok := strings.CutPrefix(channelID, groupChannelPrefix); ok {
		params["groupId"] = groupID
	} else {
		params["recipient"] = []string{channelID}
	}
	if a.cfg.Account != "" {
		params["account"] = a.cfg.Account
	}

	id := c.nextID.Add(1)
	ch := make(chan rpcResult, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "send",
		"params":  params,
	})
	if err != nil {
		return err
	}
	frame = append(frame, '\n')

	c.writeMu.Lock()
	_, err = c.net.Write(frame)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("signal: send write failed: %w", err)
	}

	// loopDone guards the registration race: a Send that registered its pending
	// entry after the dying read loop already ran failPending would otherwise
	// wait on a channel nothing can ever settle. loopDone closes strictly after
	// failPending, so one of the first two cases always fires on a dead conn.
	select {
	case res, ok := <-ch:
		return sendOutcome(res, ok)
	case <-c.loopDone:
		// The loop may have settled the response just before exiting; prefer it.
		select {
		case res, ok := <-ch:
			return sendOutcome(res, ok)
		default:
			return errors.New("signal: connection closed before send response")
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendOutcome maps a settled (or failed-pending) response to Send's error.
func sendOutcome(res rpcResult, ok bool) error {
	if !ok {
		return errors.New("signal: connection closed before send response")
	}
	if res.err != nil {
		return fmt.Errorf("signal: send failed: %s (code %d)", res.err.Message, res.err.Code)
	}
	return nil
}

// settle delivers the daemon's response for request id to its waiting Send.
func (c *rpcConn) settle(id uint64, res rpcResult) {
	c.pendMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendMu.Unlock()
	if ch != nil {
		ch <- res
	}
}

// failPending closes every pending response channel so Sends waiting on a
// dead connection fail instead of hanging.
func (c *rpcConn) failPending() {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
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
	_ = c.net.Close()

	// Let the read loop finish consuming already-buffered frames before the
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
// received message is processed rather than dropped at shutdown. It polls an
// atomic counter rather than a WaitGroup: an Add racing Wait would risk a
// misuse panic.
func (c *rpcConn) drainDispatch(ctx context.Context) {
	for c.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Attachments returns the files attached to m. signal-cli delivers attachments
// by id, so Attachment.URL is empty and ExtraData carries the *Attachment; the
// bytes live in the daemon's attachment directory under that id.
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
