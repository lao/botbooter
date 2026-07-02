// Package core holds botbooter's platform-agnostic engine: the Bot type, its
// command/middleware dispatch, and the connection lifecycle.
package core

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	default:
		return fmt.Sprintf("BotType(%d)", int(t))
	}
}

// Message is a platform-agnostic incoming message handed to command handlers.
// UserID, ChannelID and Content are always set. The remaining normalized fields
// are best-effort: a platform that cannot supply one leaves it at its zero
// value. Raw carries the originating platform's untouched event; read it with
// the matching typed accessor (e.g. discord.RawEvent).
//
// MentionedUserIDs holds mentioned user ids and is best-effort per platform: Slack and
// Discord surface every mention, while Telegram contributes only text_mention
// entities (a plain @username carries no numeric id and is omitted).
type Message struct {
	ID               string
	UserID           string
	AuthorName       string
	ChannelID        string
	Content          string
	Timestamp        time.Time
	ReplyToID        string
	MentionedUserIDs []string

	Raw any
}

// CLIMessage is the raw payload of a message read from the CLI adapter.
type CLIMessage struct {
	Text        string
	Attachments []Attachment
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
	// re is compiled once in AddHandler, the only path that appends to
	// Bot.commands, so it is always non-nil for a dispatched command.
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
	Send(ctx context.Context, channelID, text string) error
	Attachments(m *Message) ([]Attachment, error)
}

// AttachmentResolver is an OPTIONAL capability an Adapter may implement to turn
// an Attachment into a downloadable URL. It is deliberately NOT part of the
// mandatory Adapter interface: adapters whose Attachment.URL is already usable
// implement nothing and ride the passthrough in [Bot.ResolveAttachmentURL].
type AttachmentResolver interface {
	ResolveAttachmentURL(ctx context.Context, att Attachment) (string, error)
}

// AdapterDeps is the set of callbacks an Adapter uses to talk back to the Bot.
type AdapterDeps struct {
	Dispatch   func(ctx context.Context, m *Message)
	Done       func(err error)
	Disconnect func() error
}

// Bot is the platform-agnostic chat bot. A Bot is safe for concurrent use once
// its handlers and middleware are registered: register them (AddHandler,
// AddMiddleware, SetUnknownCommandHandler) before Connect, then call
// Connect/Run/Disconnect/Send concurrently as needed. Registering after Connect
// races the dispatch goroutine.
type Bot struct {
	BotType BotType

	adapter Adapter

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware

	mu   sync.Mutex
	conn *connection
}

// connection holds a single connection's lifecycle state. Connect creates a
// fresh one and Disconnect drops it, so nothing from a prior connection can
// leak into a successor — the crux of surviving disconnect→reconnect races.
type connection struct {
	cancel  context.CancelFunc
	done    chan error    // adapter reports event-loop termination here
	runDone chan struct{} // closed exactly once when this connection tears down
	once    sync.Once
	adapter Adapter
}

// teardown cancels the run context and closes runDone exactly once. It runs the
// adapter's Disconnect only when disconnectAdapter is true; a superseded
// connection passes false so a lingering goroutine can never disconnect the
// shared adapter that a newer connection now owns.
func (c *connection) teardown(disconnectAdapter bool) error {
	c.cancel()
	var err error
	c.once.Do(func() {
		close(c.runDone)
		if disconnectAdapter {
			if c.adapter == nil {
				err = ErrUnknownBotType
				return
			}
			err = c.adapter.Disconnect()
		}
	})
	return err
}

// New creates a Bot of the given type backed by adapter.
func New(botType BotType, adapter Adapter) *Bot {
	return &Bot{BotType: botType, adapter: adapter}
}

// AdapterAs returns the Bot's adapter as T, reporting whether it is that type.
// Adapter packages use it to recover their concrete adapter — and the platform
// client it holds — from a *Bot, so callers get typed access without core
// importing any platform SDK.
func AdapterAs[T any](b *Bot) (T, bool) {
	a, ok := b.adapter.(T)
	return a, ok
}

// AddHandler registers cmd, compiling its Pattern and returning an error if it
// is not valid. Commands are matched in registration order, first match wins.
func (b *Bot) AddHandler(cmd Command) error {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		return fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err)
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
	return nil
}

// HandleFunc is a convenience wrapper around AddHandler.
func (b *Bot) HandleFunc(pattern string, handler CommandHandler) error {
	return b.AddHandler(Command{Pattern: pattern, Handler: handler})
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

// Connect starts the adapter's event loop and returns without blocking. It
// returns ErrAlreadyConnected if a connection is already active, ErrUnknownBotType
// if the Bot has no adapter, or any error from the adapter's own Connect.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return ErrAlreadyConnected
	}
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := &connection{
		cancel:  cancel,
		done:    make(chan error, 1),
		runDone: make(chan struct{}),
		adapter: b.adapter,
	}

	// The callbacks capture THIS connection (c), never a bot field read late, so
	// a lingering goroutine from a prior connection writes into its own dead
	// channel and disconnectConn skips the shared adapter for it.
	deps := AdapterDeps{
		Dispatch:   b.dispatch,
		Done:       func(err error) { c.done <- err },
		Disconnect: func() error { return b.disconnectConn(c) },
	}

	// adapter.Connect is non-blocking by contract (it starts the event loop in a
	// goroutine and returns). Holding b.mu across it — and only installing b.conn
	// on success — means a concurrent Disconnect serializes behind this lock
	// rather than racing a half-connected adapter, so the adapter is never
	// connected-then-double-disconnected. The event-loop goroutine only invokes
	// deps.Disconnect on runCtx cancellation, never synchronously here, so this
	// cannot deadlock on b.mu.
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		return err
	}
	b.conn = c
	return nil
}

// disconnectConn tears down connection c. If c is still the installed
// connection it is popped and the adapter is disconnected; if c has already
// been superseded (a reconnect installed a newer connection) the adapter is
// left untouched — a newer connection owns it — and only c's own runCtx/runDone
// are settled for any Run still watching c.
func (b *Bot) disconnectConn(c *connection) error {
	b.mu.Lock()
	current := b.conn == c
	if current {
		b.conn = nil
	}
	b.mu.Unlock()
	return c.teardown(current)
}

// Disconnect tears down the active connection: it cancels the run context and
// runs the adapter's Disconnect exactly once. It is safe to call when not
// connected, returning ErrUnknownBotType only if the Bot has no adapter.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	c := b.conn
	b.conn = nil
	b.mu.Unlock()

	if c != nil {
		return c.teardown(true)
	}

	// Not connected: nothing to tear down. Still validate the bot type so a
	// misconfigured bot reports the same error it would on connect.
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return nil
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
	// A concurrent Disconnect between Connect and here already tore the
	// connection down; nothing to block on.
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

	// Both graceful-shutdown paths surface here as loopErr echoing the canceling
	// context's error, and neither should reach callers (who commonly do
	// log.Fatal(bot.Run(ctx)) and must not exit non-zero on a clean Ctrl-C):
	//   - caller ctx canceled/timed out — the event loop (e.g.
	//     socketmode.RunContext) reports ctx.Err() back through done;
	//   - a local Disconnect cancels runCtx (not ctx), reported as
	//     context.Canceled.
	if errors.Is(loopErr, context.Canceled) ||
		(ctx.Err() != nil && errors.Is(loopErr, ctx.Err())) {
		loopErr = nil
	}

	if disconnectErr != nil {
		if ctx.Err() != nil {
			log.Printf("botbooter: error disconnecting during shutdown: %v", disconnectErr)
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
func (b *Bot) SendMessage(channelID, text string) error {
	return b.SendMessageContext(context.Background(), channelID, text)
}

// SendMessageContext sends text to channelID, honoring ctx for cancellation.
func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return b.adapter.Send(ctx, channelID, text)
}

// GetAttachments returns the platform-agnostic attachments of message.
func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	return b.adapter.Attachments(message)
}

// ResolveAttachmentURL returns a downloadable URL for att — the unified
// cross-platform entry point. If the Bot's adapter implements
// [AttachmentResolver] the call is delegated in full (the adapter owns the
// result, including ("", nil) meaning "nothing to resolve"); otherwise att.URL is
// returned verbatim. It returns [ErrUnknownBotType] if the Bot has no adapter. An
// empty string with a nil error means "not resolvable", not a failure.
//
// The result is consumed DIFFERENTLY per platform and is not uniformly fetchable
// with a bare GET:
//   - Discord: att.URL is already a signed CDN link (~24h), returned as-is via the
//     passthrough; fetch it with a plain GET and consume promptly.
//   - Slack: NOT directly fetchable — download via the Slack Web API client
//     (SlackClient(b).GetFileContext), which injects the bot token.
//   - Telegram: a plain GET on a SECRET, ~1h URL that embeds the bot token in
//     plaintext — never log or cache it. Each successful Telegram resolve logs a
//     warning, suppressible via the BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING
//     environment variable.
//   - WhatsApp: NOT directly fetchable — GET it with an Authorization: Bearer
//     <token> header (the Cloud API token used to send). Short-lived; consume promptly.
//   - Teams: an uploaded file's URL is a pre-authorized link (plain GET, but it
//     carries a short-lived token — consume promptly, never log or cache). An inline
//     image's URL may need an Authorization: Bearer <bot token> header, which this
//     adapter does not yet supply. Returned as-is via the passthrough.
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
			log.Printf("botbooter: recovered from panic while handling message: %v", r)
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
