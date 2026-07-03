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

// Reaction is a platform-agnostic emoji reaction added to a message, handed to
// handlers registered with [Bot.OnReaction]. UserID, ChannelID and MessageID are
// always set; MessageID identifies the reacted message and is the reply target for
// [Bot.ReplyToMessage]. Emoji is best-effort and platform-shaped: a shortname
// ("thumbsup") on Slack, a unicode character elsewhere. AuthorName is best-effort
// and inline-only — empty on platforms whose reaction payload carries only a user
// id. Raw carries the originating platform event; read it with the matching typed
// accessor (e.g. slack.RawReaction).
type Reaction struct {
	Emoji      string
	UserID     string
	AuthorName string
	ChannelID  string
	MessageID  string

	Raw any
}

// ReactionHandler handles an emoji reaction dispatched to [Bot.OnReaction].
type ReactionHandler func(ctx context.Context, b *Bot, r *Reaction)

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
	if c.re != nil {
		return c.re.MatchString(content)
	}
	// Fallback for commands constructed without AddHandler.
	matched, err := regexp.MatchString(c.Pattern, content)
	return err == nil && matched
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

// ThreadedSender is an OPTIONAL capability an Adapter may implement to post a
// reply nested on a specific message. Like [AttachmentResolver] it is deliberately
// NOT part of the mandatory Adapter interface: adapters that implement it get
// threaded replies via [Bot.ReplyToMessage]; those that do not fall back to a
// plain channel message.
type ThreadedSender interface {
	SendThreaded(ctx context.Context, channelID, replyToID, text string) error
}

// AdapterDeps is the set of callbacks an Adapter uses to talk back to the Bot.
// DispatchReaction is optional: adapters on platforms without reaction events
// simply never call it.
type AdapterDeps struct {
	Dispatch         func(ctx context.Context, m *Message)
	DispatchReaction func(ctx context.Context, r *Reaction)
	Done             func(err error)
	Disconnect       func() error
}

// Bot is the platform-agnostic chat bot. A Bot is safe for concurrent use.
type Bot struct {
	BotType BotType

	adapter Adapter

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware
	reactionHandlers      []ReactionHandler

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	stop   func() error
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

// OnReaction registers h to run whenever a user adds an emoji reaction, on the
// platforms that surface reaction events (Slack, Discord, Telegram, WhatsApp).
// Handlers are not regex-matched — branch on Reaction.Emoji inside the handler —
// and, unlike message dispatch, reactions bypass the Middleware chain.
func (b *Bot) OnReaction(h ReactionHandler) {
	b.reactionHandlers = append(b.reactionHandlers, h)
}

// dispatchReaction runs every registered reaction handler. Each handler call is
// recovered individually, so a panic in one handler neither skips the others nor
// takes down the adapter's event loop.
func (b *Bot) dispatchReaction(ctx context.Context, r *Reaction) {
	for _, h := range b.reactionHandlers {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("botbooter: recovered from panic while handling reaction: %v", rec)
				}
			}()
			h(ctx, b, r)
		}()
	}
}

// Connect starts the adapter's event loop and returns without blocking. It
// returns ErrAlreadyConnected if a connection is already active, ErrUnknownBotType
// if the Bot has no adapter, or any error from the adapter's own Connect.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	if b.cancel != nil {
		b.mu.Unlock()
		return ErrAlreadyConnected
	}
	runCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.done = make(chan error, 1)

	// Each connection gets its own stop closure guarded by a fresh sync.Once.
	// A reconnect installs a new closure rather than resetting a shared Once,
	// so a lingering disconnect goroutine from a previous connection can never
	// race a Once that the new connection has reset.
	var once sync.Once
	b.stop = func() error {
		var err error
		once.Do(func() {
			if b.adapter == nil {
				err = ErrUnknownBotType
				return
			}
			err = b.adapter.Disconnect()
		})
		return err
	}
	b.mu.Unlock()

	if b.adapter == nil {
		cancel()
		b.clearConnection()
		return ErrUnknownBotType
	}

	deps := AdapterDeps{
		Dispatch:         b.dispatch,
		DispatchReaction: b.dispatchReaction,
		Done:             func(err error) { b.done <- err },
		Disconnect:       b.Disconnect,
	}
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		b.clearConnection()
		return err
	}
	return nil
}

func (b *Bot) clearConnection() {
	b.mu.Lock()
	b.cancel = nil
	b.stop = nil
	b.mu.Unlock()
}

// Disconnect tears down the active connection: it cancels the run context and
// runs the adapter's Disconnect exactly once. It is safe to call when not
// connected, returning ErrUnknownBotType only if the Bot has no adapter.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	cancel := b.cancel
	b.cancel = nil
	stop := b.stop
	b.stop = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if stop != nil {
		return stop()
	}

	// Not connected: nothing to tear down. Still validate the bot type so a
	// misconfigured bot reports the same error it would on connect.
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return nil
}

// Run connects the Bot and blocks until ctx is canceled or the event loop ends,
// then disconnects. A clean shutdown via ctx cancellation returns nil rather
// than ctx.Err(), so callers can safely do log.Fatal(bot.Run(ctx)).
func (b *Bot) Run(ctx context.Context) error {
	if err := b.Connect(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	done := b.done
	b.mu.Unlock()

	var loopErr error
	select {
	case <-ctx.Done():
	case loopErr = <-done:
	}

	disconnectErr := b.Disconnect()

	// Canceling the run context is the normal graceful-shutdown signal. The
	// event loop (e.g. socketmode.RunContext) reports it back through done as
	// context.Canceled; don't surface that to callers, which commonly do
	// log.Fatal(bot.Run(ctx)) and would exit non-zero on a clean Ctrl-C.
	if ctx.Err() != nil && errors.Is(loopErr, ctx.Err()) {
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

// SendMessage sends text to channelID using a background context.
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

// ReplyToMessage posts text as a reply nested on the message identified by
// replyToID in channelID. If the Bot's adapter implements [ThreadedSender] the
// call is delegated; otherwise it falls back to a plain [Bot.SendMessageContext],
// so the reply still reaches the channel, just not threaded. It returns
// [ErrUnknownBotType] if the Bot has no adapter.
func (b *Bot) ReplyToMessage(ctx context.Context, channelID, replyToID, text string) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	if s, ok := b.adapter.(ThreadedSender); ok {
		return s.SendThreaded(ctx, channelID, replyToID, text)
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
