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
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

var ErrUnknownBotType = errors.New("botbooter: unknown bot type")

var ErrAlreadyConnected = errors.New("botbooter: already connected")

type BotType int

const (
	SlackBotType BotType = iota
	DiscordBotType
	CLIBotType
)

func (t BotType) String() string {
	switch t {
	case SlackBotType:
		return "slack"
	case DiscordBotType:
		return "discord"
	case CLIBotType:
		return "cli"
	default:
		return fmt.Sprintf("BotType(%d)", int(t))
	}
}

type Message struct {
	UserID    string
	ChannelID string
	Content   string

	DiscordData *discordgo.MessageCreate
	SlackData   *slackevents.MessageEvent
	CLIData     *CLIMessage
}

type CLIMessage struct {
	Text        string
	Attachments []Attachment
}

type CommandHandler func(ctx context.Context, b *Bot, m *Message)

type Middleware func(ctx context.Context, b *Bot, m *Message, next CommandHandler)

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

type Bot struct {
	BotType BotType

	DiscordSession    *discordgo.Session
	SlackClient       *slack.Client
	SlackSocketClient *socketmode.Client

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

func (b *Bot) AddHandler(cmd Command) error {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		return fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err)
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
	return nil
}

func (b *Bot) HandleFunc(pattern string, handler CommandHandler) error {
	return b.AddHandler(Command{Pattern: pattern, Handler: handler})
}

func (b *Bot) SetUnknownCommandHandler(handler CommandHandler) {
	b.unknownCommandHandler = handler
}

func (b *Bot) AddMiddleware(middleware Middleware) {
	b.middlewares = append(b.middlewares, middleware)
}

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

func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}

func (b *Bot) SendMessage(channelID, text string) error {
	return b.SendMessageContext(context.Background(), channelID, text)
}

func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return b.adapter.Send(ctx, channelID, text)
}

func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	return b.adapter.Attachments(message)
}

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
