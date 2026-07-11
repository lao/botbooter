// Package core holds botbooter's platform-agnostic engine: the Bot type, its
// command/middleware dispatch, and the connection lifecycle.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// ErrUnknownBotType is returned by Bot methods when the Bot has no adapter.
var ErrUnknownBotType = errors.New("botbooter: unknown bot type")

// ErrAlreadyConnected is returned by Connect when the Bot is already connected.
var ErrAlreadyConnected = errors.New("botbooter: already connected")

// ErrNilMessage is returned by Bot methods handed a nil *Message argument.
var ErrNilMessage = errors.New("botbooter: nil message")

// BotType identifies the messaging platform a Bot is connected to.
type BotType int

// The supported bot types.
const (
	SlackBotType BotType = iota
	DiscordBotType
	CLIBotType
	TelegramBotType
	WhatsAppBotType
	TeamsBotType
	WhatsMeowBotType
)

func (t BotType) String() string {
	switch t {
	case SlackBotType:
		return "slack"
	case DiscordBotType:
		return "discord"
	case CLIBotType:
		return "cli"
	case TelegramBotType:
		return "telegram"
	case WhatsAppBotType:
		return "whatsapp"
	case TeamsBotType:
		return "teams"
	case WhatsMeowBotType:
		return "whatsmeow"
	default:
		return fmt.Sprintf("BotType(%d)", int(t))
	}
}

// Message is a platform-agnostic incoming message handed to command handlers.
// UserID, ChannelID and Content are always set; the remaining normalized fields
// are best-effort per platform. Raw carries the originating platform's untouched
// event; read it with the matching typed accessor (e.g. discord.RawEvent).
type Message struct {
	ID         string
	UserID     string
	AuthorName string
	ChannelID  string
	Content    string
	Timestamp  time.Time
	ReplyToID  string
	// MentionedUserIDs lists each mentioned user once, in first-mention order.
	// Whether the bot's own mention appears follows Content: Teams strips the
	// bot's mention from Content and so excludes its ID here; Slack and Discord
	// keep it in both.
	MentionedUserIDs []string

	Raw any
}

// CommandHandler handles a dispatched message for a matched command.
type CommandHandler func(ctx context.Context, b *Bot, m *Message)

// Middleware wraps message dispatch; it must call next to continue the chain.
type Middleware func(ctx context.Context, b *Bot, m *Message, next CommandHandler)

// Command pairs a regular-expression Pattern with the Handler to run on a match.
type Command struct {
	Pattern string
	Handler CommandHandler

	re *regexp.Regexp
}

func (c *Command) match(content string) bool {
	// re is always non-nil: AddHandler is the only path that appends to Bot.commands.
	return c.re.MatchString(content)
}

// Attachment is a platform-agnostic file attached to a message.
type Attachment struct {
	IsImage   bool
	URL       string
	ExtraData any
}

// Adapter is the platform-specific half of a Bot. The Bot drives it through this
// interface, so the core has no compile-time dependency on any platform.
type Adapter interface {
	Connect(ctx context.Context, deps AdapterDeps) error
	Disconnect() error
	Send(ctx context.Context, channelID, text string, opts SendOptions) error
	Attachments(m *Message) ([]Attachment, error)
}

// AttachmentResolver is an optional capability an Adapter may implement to turn
// an Attachment into a downloadable URL; adapters whose Attachment.URL is already
// usable ride the passthrough in [Bot.ResolveAttachmentURL].
type AttachmentResolver interface {
	ResolveAttachmentURL(ctx context.Context, att Attachment) (string, error)
}

// SendOptions is the resolved set of per-send modifiers an Adapter reads off a
// Send call. Its zero value means "a plain channel message". A threading anchor
// is platform-specific, so each adapter derives its own from these fields:
//   - ReplyTo: reply anchored on this whole message; the adapter picks the
//     correct native anchor (Slack thread_ts from ReplyToID; Discord/Telegram/
//     WhatsApp the replied-to/quoted message id).
//   - ThreadID: a raw native anchor supplied by the caller, used verbatim. It
//     wins over ReplyTo when both are set. On Slack it is a thread_ts; on
//     Discord/Telegram/WhatsApp a reply/quote message id (NOT a Discord
//     thread-channel id).
type SendOptions struct {
	ReplyTo  *Message
	ThreadID string
}

// SendOption modifies a SendOptions. Construct them with [InReplyTo] /
// [WithThreadID] and pass them to [Bot.SendMessageContext] / [Bot.SendMessage].
type SendOption func(*SendOptions)

// InReplyTo anchors the send on m so the adapter posts into m's thread or
// reply-chain, deriving the correct per-platform anchor itself.
func InReplyTo(m *Message) SendOption { return func(o *SendOptions) { o.ReplyTo = m } }

// WithThreadID anchors the send on a raw native id the adapter uses verbatim.
// It takes precedence over [InReplyTo]. The caller owns platform-correctness.
func WithThreadID(id string) SendOption { return func(o *SendOptions) { o.ThreadID = id } }

// resolveSendOptions folds opts into a single SendOptions, skipping nil options
// so a conditionally-built option slice can carry a nil entry without panicking.
func resolveSendOptions(opts ...SendOption) SendOptions {
	var o SendOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// AdapterDeps is the set of callbacks an Adapter uses to talk back to the Bot,
// plus the Bot's logger so adapter diagnostics route through the same sink.
type AdapterDeps struct {
	Dispatch   func(ctx context.Context, m *Message)
	Done       func(err error)
	Disconnect func() error
	Logger     *slog.Logger // always non-nil
}

// Bot is the platform-agnostic chat bot. Register handlers and middleware
// before Connect; after that, Connect/Run/Disconnect/Send are safe to call
// concurrently. Registering after Connect races the dispatch goroutine.
type Bot struct {
	BotType BotType

	adapter Adapter

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware
	setupErrs             []error // registration errors, surfaced by Connect
	logger                *slog.Logger

	mu   sync.Mutex
	conn *connection
}

// connection holds a single connection's lifecycle state. Connect creates a
// fresh one and Disconnect drops it, so nothing from a prior connection can
// leak into a successor across disconnect→reconnect races.
type connection struct {
	cancel  context.CancelFunc
	done    chan error    // adapter reports event-loop termination here
	runDone chan struct{} // closed exactly once when this connection tears down
	once    sync.Once
	discErr error // adapter.Disconnect result, recorded once and shared by all callers
	adapter Adapter
}

// teardown cancels the run context and closes runDone exactly once. It runs the
// adapter's Disconnect only when disconnectAdapter is true; a superseded
// connection passes false so it can never disconnect the shared adapter a newer
// connection now owns.
func (c *connection) teardown(disconnectAdapter bool) error {
	c.cancel()
	c.once.Do(func() {
		close(c.runDone)
		if disconnectAdapter {
			c.discErr = c.adapter.Disconnect()
		}
	})
	return c.discErr
}

// New creates a Bot of the given type backed by adapter.
func New(botType BotType, adapter Adapter) *Bot {
	return &Bot{BotType: botType, adapter: adapter}
}

// AdapterAs returns the Bot's adapter as T, reporting whether it is that type.
// Adapter packages use it to recover their concrete adapter from a *Bot.
func AdapterAs[T any](b *Bot) (T, bool) {
	a, ok := b.adapter.(T)
	return a, ok
}

// AddHandler registers cmd, compiling its Pattern. An invalid pattern is
// recorded rather than returned — it surfaces from Connect (and Run) and is
// also logged at record time (so a registration after Connect is not silently
// dropped), and the command is not registered. Commands are matched in
// registration order, first match wins.
func (b *Bot) AddHandler(cmd Command) {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		b.log().Error("botbooter: invalid command pattern", "pattern", cmd.Pattern, "error", err)
		b.setupErrs = append(b.setupErrs, fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err))
		return
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
}

// HandleFunc is a convenience wrapper around AddHandler.
func (b *Bot) HandleFunc(pattern string, handler CommandHandler) {
	b.AddHandler(Command{Pattern: pattern, Handler: handler})
}

// SetUnknownCommandHandler sets the handler invoked when a message matches no
// registered command; if unset, unmatched messages are ignored.
func (b *Bot) SetUnknownCommandHandler(handler CommandHandler) {
	b.unknownCommandHandler = handler
}

// AddMiddleware appends middleware to the dispatch chain, run in registration order.
func (b *Bot) AddMiddleware(middleware Middleware) {
	b.middlewares = append(b.middlewares, middleware)
}

// SetLogger routes the Bot's and its adapter's diagnostics (panic recovery,
// shutdown warnings, webhook rejections) through logger instead of
// [slog.Default]. Like handler registration, call it before Connect.
func (b *Bot) SetLogger(logger *slog.Logger) {
	b.logger = logger
}

// log returns the configured logger, falling back to slog.Default().
func (b *Bot) log() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// Connect starts the adapter's event loop and returns without blocking. It
// returns ErrAlreadyConnected if a connection is already active, ErrUnknownBotType
// if the Bot has no adapter, every registration error recorded by AddHandler
// (joined, one per invalid pattern), or any error from the adapter's own Connect.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return ErrAlreadyConnected
	}
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	if err := errors.Join(b.setupErrs...); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := &connection{
		cancel:  cancel,
		done:    make(chan error, 1),
		runDone: make(chan struct{}),
		adapter: b.adapter,
	}

	// The callbacks capture THIS connection (c), so a lingering goroutine from a
	// prior connection writes into its own dead channel and never touches the
	// shared adapter.
	deps := AdapterDeps{
		Dispatch:   b.dispatch,
		Done:       func(err error) { c.done <- err },
		Disconnect: func() error { return b.disconnectConn(c) },
		Logger:     b.log(),
	}

	// adapter.Connect is non-blocking by contract. Holding b.mu across it — and
	// installing b.conn only on success — serializes a concurrent Disconnect
	// behind this lock instead of racing a half-connected adapter.
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		return err
	}
	b.conn = c
	return nil
}

// disconnectConn tears down connection c. If c is still the installed
// connection the adapter is disconnected; if c has been superseded by a
// reconnect, only c's own runCtx/runDone are settled and the adapter — now
// owned by the newer connection — is left untouched.
//
// b.conn is cleared only AFTER teardown returns, so a Connect racing a
// slow adapter Disconnect (drains can take seconds) gets ErrAlreadyConnected
// instead of starting a second live session on the shared adapter. Concurrent
// teardowns of the same connection serialize on its sync.Once: the losers
// block inside teardown until the winner's adapter Disconnect finishes.
func (b *Bot) disconnectConn(c *connection) error {
	b.mu.Lock()
	current := b.conn == c
	b.mu.Unlock()

	err := c.teardown(current)

	if current {
		b.mu.Lock()
		if b.conn == c {
			b.conn = nil
		}
		b.mu.Unlock()
	}
	return err
}

// Disconnect tears down the active connection: it cancels the run context and
// runs the adapter's Disconnect exactly once. It is safe to call when not
// connected, returning ErrUnknownBotType only if the Bot has no adapter.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()

	if c == nil {
		if b.adapter == nil {
			return ErrUnknownBotType
		}
		return nil
	}
	return b.disconnectConn(c)
}

// Run connects the Bot and blocks until ctx is canceled, the event loop ends,
// or Disconnect is called from elsewhere, then disconnects. A clean shutdown
// (ctx cancellation or a local Disconnect) returns nil rather than ctx.Err(),
// so callers can safely do log.Fatal(bot.Run(ctx)).
func (b *Bot) Run(ctx context.Context) error {
	if err := b.Connect(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	// A concurrent Disconnect already tore the connection down.
	if c == nil {
		return nil
	}

	var loopErr error
	select {
	case <-ctx.Done():
	case <-c.runDone: // a Disconnect from another goroutine woke us
	case loopErr = <-c.done:
	}

	disconnectErr := b.Disconnect()

	// Graceful shutdown (ctx canceled, or a local Disconnect canceling runCtx)
	// surfaces as loopErr echoing the canceling context's error; swallow it so
	// log.Fatal(bot.Run(ctx)) doesn't exit non-zero on a clean Ctrl-C.
	if errors.Is(loopErr, context.Canceled) ||
		(ctx.Err() != nil && errors.Is(loopErr, ctx.Err())) {
		loopErr = nil
	}

	if disconnectErr != nil {
		if ctx.Err() != nil {
			b.log().Error("botbooter: error disconnecting during shutdown", "error", disconnectErr)
		} else if loopErr == nil {
			loopErr = disconnectErr
		}
	}
	return loopErr
}

// Start runs the Bot until the process receives an interrupt or SIGTERM.
func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}

// SendMessage sends text to channelID using a background context. Prefer
// SendMessageContext from within a handler so the send honors shutdown and
// cancellation; SendMessage's background context outlives Run's teardown.
func (b *Bot) SendMessage(channelID, text string, opts ...SendOption) error {
	return b.SendMessageContext(context.Background(), channelID, text, opts...)
}

// SendMessageContext sends text to channelID, honoring ctx for cancellation.
// Pass [InReplyTo] or [WithThreadID] to thread the message onto an inbound one;
// with no options it is a plain channel message.
func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string, opts ...SendOption) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return b.adapter.Send(ctx, channelID, text, resolveSendOptions(opts...))
}

// Reply is convenience sugar for replying into the thread or reply-chain of the
// inbound message m — it is exactly SendMessageContext(ctx, m.ChannelID, text,
// InReplyTo(m)). Each adapter derives its own platform-specific anchor; see
// [SendOptions]. It returns ErrNilMessage if m is nil, or ErrUnknownBotType if
// the Bot has no adapter.
func (b *Bot) Reply(ctx context.Context, m *Message, text string) error {
	if m == nil {
		return ErrNilMessage
	}
	// A nil adapter is caught by the delegated SendMessageContext.
	return b.SendMessageContext(ctx, m.ChannelID, text, InReplyTo(m))
}

// GetAttachments returns the platform-agnostic attachments of message. It
// returns ErrNilMessage if message is nil, or ErrUnknownBotType if the Bot has
// no adapter.
func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	if message == nil {
		return nil, ErrNilMessage
	}
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	return b.adapter.Attachments(message)
}

// ResolveAttachmentURL returns a downloadable URL for att. If the adapter
// implements [AttachmentResolver] the call is delegated; otherwise att.URL is
// returned verbatim. It returns [ErrUnknownBotType] if the Bot has no adapter.
// An empty string with a nil error means "not resolvable", not a failure.
//
// The result is consumed differently per platform:
//   - Discord: a signed CDN link (~24h); plain GET, consume promptly.
//   - Slack: not directly fetchable — download via the Slack Web API client
//     (SlackClient(b).GetFileContext), which injects the bot token.
//   - Telegram: a plain GET on a secret, ~1h URL that embeds the bot token —
//     never log or cache it. Each resolve logs a warning, suppressible via
//     BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING.
//   - WhatsApp: GET with an "Authorization: Bearer <token>" header (the Cloud
//     API token used to send). Short-lived; consume promptly.
//   - Teams: a pre-authorized link carrying a short-lived token — consume
//     promptly, never log or cache. Inline images may need an Authorization
//     header this adapter does not yet supply.
//   - CLI: a local filesystem path (open with os.Open), not an HTTP URL.
func (b *Bot) ResolveAttachmentURL(ctx context.Context, att Attachment) (string, error) {
	if b.adapter == nil {
		return "", ErrUnknownBotType
	}
	if r, ok := b.adapter.(AttachmentResolver); ok {
		return r.ResolveAttachmentURL(ctx, att)
	}
	return att.URL, nil
}

// dispatch routes message through the middleware chain to the first matching
// command. A panic in any handler or middleware is recovered and logged so it
// cannot take down the event loop.
func (b *Bot) dispatch(ctx context.Context, message *Message) {
	defer func() {
		if r := recover(); r != nil {
			b.log().Error("botbooter: recovered from panic while handling message", "panic", r)
		}
	}()

	handler := func(ctx context.Context, bot *Bot, message *Message) {
		for i := range bot.commands {
			if bot.commands[i].match(message.Content) {
				bot.commands[i].Handler(ctx, bot, message)
				return
			}
		}
		if bot.unknownCommandHandler != nil {
			bot.unknownCommandHandler(ctx, bot, message)
		}
	}

	final := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		middleware := b.middlewares[i]
		next := final
		final = func(ctx context.Context, bot *Bot, message *Message) {
			middleware(ctx, bot, message, next)
		}
	}

	final(ctx, b, message)
}
