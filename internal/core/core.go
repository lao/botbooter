// Package core holds botbooter's platform-agnostic engine: the Bot type, its
// command/middleware dispatch, and the connection lifecycle. Platform support
// is provided by adapters (see the Adapter interface) that live in sibling
// internal packages. The public github.com/lao/botbooter package is a thin
// facade over this one.
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

	"github.com/bwmarrin/discordgo"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ErrUnknownBotType is returned by Bot methods when the Bot has no adapter,
// which happens when it was not built through one of the platform constructors.
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
)

// String returns the lowercase platform name (e.g. "slack"), or a
// BotType(<n>) placeholder for an unknown value.
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
	default:
		return fmt.Sprintf("BotType(%d)", int(t))
	}
}

// Message is a platform-agnostic incoming message handed to command handlers.
// UserID, ChannelID and Content are always set; the platform-specific *Data
// field carries the raw event for callers that need it, and only the field for
// the originating platform is non-nil.
type Message struct {
	UserID    string
	ChannelID string
	Content   string

	DiscordData  *discordgo.MessageCreate
	SlackData    *slackevents.MessageEvent
	TelegramData *models.Update
	CLIData      *CLIMessage
}

// CLIMessage is the raw payload of a message read from the CLI adapter: the
// typed line and any attachments resolved from file paths in it.
type CLIMessage struct {
	Text        string
	Attachments []Attachment
}

// CommandHandler handles a dispatched message for a matched command.
type CommandHandler func(ctx context.Context, b *Bot, m *Message)

// Middleware wraps message dispatch. It runs before the matched handler and
// must call next to continue the chain (or omit it to short-circuit).
type Middleware func(ctx context.Context, b *Bot, m *Message, next CommandHandler)

// Command pairs a regular-expression Pattern with the Handler to run when an
// incoming message matches it. Register one with Bot.AddHandler, which compiles
// and caches the pattern.
type Command struct {
	Pattern string
	Handler CommandHandler

	re *regexp.Regexp
}

// match reports whether content matches the command's pattern, using the
// pre-compiled regexp when present and falling back to compiling Pattern for
// commands built without AddHandler.
func (c *Command) match(content string) bool {
	if c.re != nil {
		return c.re.MatchString(content)
	}
	// Fallback for commands constructed without AddHandler.
	matched, err := regexp.MatchString(c.Pattern, content)
	return err == nil && matched
}

// Attachment is a platform-agnostic file attached to a message. ExtraData holds
// the raw platform-specific attachment for callers that need more than URL.
type Attachment struct {
	IsImage   bool
	URL       string
	ExtraData any
}

// Adapter is the platform-specific half of a Bot. Each supported platform
// (Slack, Discord, CLI) provides one; the Bot drives it through this interface,
// so the core has no compile-time dependency on any particular platform's
// connection logic.
type Adapter interface {
	// Connect starts delivering incoming messages and runs until ctx is
	// canceled. It uses deps to dispatch messages and to signal the run loop.
	Connect(ctx context.Context, deps AdapterDeps) error
	// Disconnect tears down the connection. It must be safe to call when the
	// adapter never connected.
	Disconnect() error
	// Send delivers text to channelID.
	Send(ctx context.Context, channelID, text string) error
	// Attachments extracts the platform-agnostic attachments of a message.
	Attachments(m *Message) ([]Attachment, error)
}

// AdapterDeps is the set of callbacks an Adapter uses to talk back to the Bot.
// It keeps the Bot's internals (dispatch, the done channel) unexported while
// still letting adapters in other packages drive them.
type AdapterDeps struct {
	// Dispatch routes an incoming message through middleware and command
	// matching.
	Dispatch func(ctx context.Context, m *Message)
	// Done signals the run loop that the connection has ended with err.
	Done func(err error)
	// Disconnect performs a full Bot.Disconnect; adapters that tear down on
	// context cancellation (e.g. Discord) use it.
	Disconnect func() error
}

// Bot is the platform-agnostic chat bot. It holds the registered commands,
// middleware and unknown-command handler, drives a single Adapter through the
// connection lifecycle, and exposes the raw platform clients (DiscordSession,
// SlackClient, ...) as escape hatches. A Bot is safe for concurrent use.
type Bot struct {
	BotType BotType

	DiscordSession    *discordgo.Session
	SlackClient       *slack.Client
	SlackSocketClient *socketmode.Client
	TelegramBot       *bot.Bot

	adapter Adapter

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	stop   func() error
}

// New creates a Bot of the given type backed by adapter. Constructors in the
// adapter packages use it and then set the exported escape-hatch fields
// (DiscordSession, SlackClient, ...) where applicable.
func New(botType BotType, adapter Adapter) *Bot {
	return &Bot{BotType: botType, adapter: adapter}
}

// AddHandler registers cmd, compiling and caching its Pattern. It returns an
// error if the pattern is not a valid regular expression. Commands are matched
// in registration order, first match wins.
func (b *Bot) AddHandler(cmd Command) error {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		return fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err)
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
	return nil
}

// HandleFunc is a convenience wrapper around AddHandler that registers handler
// for the given pattern.
func (b *Bot) HandleFunc(pattern string, handler CommandHandler) error {
	return b.AddHandler(Command{Pattern: pattern, Handler: handler})
}

// SetUnknownCommandHandler sets the handler invoked when an incoming message
// matches no registered command. If unset, unmatched messages are ignored.
func (b *Bot) SetUnknownCommandHandler(handler CommandHandler) {
	b.unknownCommandHandler = handler
}

// AddMiddleware appends middleware to the dispatch chain. Middleware runs in
// registration order, each wrapping the next, around the matched handler.
func (b *Bot) AddMiddleware(middleware Middleware) {
	b.middlewares = append(b.middlewares, middleware)
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
		Dispatch:   b.dispatch,
		Done:       func(err error) { b.done <- err },
		Disconnect: b.Disconnect,
	}
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		b.clearConnection()
		return err
	}
	return nil
}

// clearConnection resets the per-connection state so a later Connect can run.
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

// Start runs the Bot until the process receives an interrupt or SIGTERM,
// wiring up signal handling for callers that do not manage their own context.
func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}

// SendMessage sends text to channelID using a background context. See
// SendMessageContext to supply your own.
func (b *Bot) SendMessage(channelID, text string) error {
	return b.SendMessageContext(context.Background(), channelID, text)
}

// SendMessageContext sends text to channelID, honoring ctx for cancellation and
// deadlines. It returns ErrUnknownBotType if the Bot has no adapter.
func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return b.adapter.Send(ctx, channelID, text)
}

// GetAttachments returns the platform-agnostic attachments of message. It
// returns ErrUnknownBotType if the Bot has no adapter.
func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	return b.adapter.Attachments(message)
}

// dispatch routes message through the middleware chain to the first matching
// command, or the unknown-command handler. A panic in any handler or middleware
// is recovered and logged so it cannot take down the event loop.
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
