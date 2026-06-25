// Package botbooter is a small framework for building chat bots that behave
// the same way across multiple platforms.
//
// A single [Bot] value abstracts over Slack, Discord and a local CLI adapter.
// You register [Command] handlers (matched against message content with a
// regular expression) and optional [Middleware], then run the bot:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//
//	bot, err := botbooter.InitAsDiscordBot(os.Getenv("DISCORD_BOT_TOKEN"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	if err := bot.AddHandler(botbooter.Command{
//		Pattern: "^ping$",
//		Handler: func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
//			_ = b.SendMessageContext(ctx, m.ChannelID, "pong")
//		},
//	}); err != nil {
//		log.Fatal(err)
//	}
//	if err := bot.Run(ctx); err != nil {
//		log.Fatal(err)
//	}
package botbooter

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// ErrUnknownBotType is returned by [Bot] methods when the bot's [BotType] does
// not correspond to a supported platform.
var ErrUnknownBotType = errors.New("botbooter: unknown bot type")

// ErrAlreadyConnected is returned by [Bot.Connect] when the bot is already
// connected.
var ErrAlreadyConnected = errors.New("botbooter: already connected")

// BotType identifies the messaging platform a [Bot] is connected to.
type BotType int

const (
	// SlackBotType is a bot that talks to Slack via Socket Mode.
	SlackBotType BotType = iota
	// DiscordBotType is a bot that talks to Discord via the Gateway.
	DiscordBotType
	// CLIBotType is a bot that reads from an [io.Reader] and writes to an
	// [io.Writer], useful for local development and testing.
	CLIBotType
)

// String returns a human-readable name for the bot type.
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

// Message is a platform-agnostic representation of an incoming message.
//
// DiscordData and SlackData expose the underlying platform payload for callers
// that need more than the common fields; exactly one is non-nil depending on
// the bot type (both are nil for the CLI adapter).
type Message struct {
	UserID    string
	ChannelID string
	Content   string

	DiscordData *discordgo.MessageCreate
	SlackData   *slackevents.MessageEvent
	CLIData     *CLIMessage
}

// CLIMessage is the payload for a message received through the CLI adapter.
// Attachments are local files referenced by path in the typed message (see
// [InitAsCLIBot]).
type CLIMessage struct {
	Text        string
	Attachments []Attachment
}

// CommandHandler handles an incoming message that matched a [Command] (or the
// unknown-command fallback). The context is derived from the one passed to
// [Bot.Run]/[Bot.Connect] and is canceled when the bot shuts down.
type CommandHandler func(ctx context.Context, b *Bot, m *Message)

// Middleware wraps message handling. It must call next to continue the chain;
// returning without calling next short-circuits handling.
type Middleware func(ctx context.Context, b *Bot, m *Message, next CommandHandler)

// Command associates a regular-expression Pattern with a Handler. Pattern is
// compiled once when the command is registered with [Bot.AddHandler].
//
// A Command may also be constructed directly and matched without registration,
// in which case Pattern is compiled lazily on each match and an invalid pattern
// silently never matches. Prefer [Bot.AddHandler] (or [Bot.HandleFunc]), which
// compiles Pattern up front and returns an error for an invalid expression.
type Command struct {
	Pattern string
	Handler CommandHandler

	re *regexp.Regexp
}

// match reports whether content matches the command's pattern.
func (c *Command) match(content string) bool {
	if c.re != nil {
		return c.re.MatchString(content)
	}
	// Fallback for commands constructed without AddHandler.
	matched, err := regexp.MatchString(c.Pattern, content)
	return err == nil && matched
}

// Attachment is a platform-agnostic file attached to a [Message].
type Attachment struct {
	IsImage   bool
	URL       string
	ExtraData any
}

// Bot is a platform-agnostic chat bot. Construct one with [InitAsSlackBot],
// [InitAsDiscordBot] or [InitAsCLIBot]. A Bot is safe to configure (AddHandler,
// AddMiddleware, ...) before it is connected; configuration after Connect is
// not synchronized and should be avoided.
type Bot struct {
	BotType BotType

	// Underlying platform clients. These are exported as an escape hatch for
	// advanced use; prefer the Bot methods where possible.
	DiscordSession    *discordgo.Session
	SlackClient       *slack.Client
	SlackSocketClient *socketmode.Client

	cliIn  io.Reader
	cliOut io.Writer

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
}

// AddHandler registers a command. It compiles the command's pattern and
// returns an error if the pattern is not a valid regular expression.
func (b *Bot) AddHandler(cmd Command) error {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		return fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err)
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
	return nil
}

// HandleFunc registers handler for messages whose content matches pattern. It
// is a convenience wrapper around [Bot.AddHandler] and returns an error if
// pattern is not a valid regular expression.
func (b *Bot) HandleFunc(pattern string, handler CommandHandler) error {
	return b.AddHandler(Command{Pattern: pattern, Handler: handler})
}

// SetUnknownCommandHandler sets the handler invoked when no command matches.
func (b *Bot) SetUnknownCommandHandler(handler CommandHandler) {
	b.unknownCommandHandler = handler
}

// AddMiddleware appends a middleware to the chain. Middlewares run in the order
// they are added, wrapping the command dispatch.
func (b *Bot) AddMiddleware(middleware Middleware) {
	b.middlewares = append(b.middlewares, middleware)
}

// Connect opens the platform connection and begins delivering incoming
// messages to handlers in the background. It returns once the connection is
// established. The provided context governs the connection's lifetime: when it
// is canceled the bot stops receiving events. Use [Bot.Disconnect] to stop the
// bot, or [Bot.Run] for a blocking lifecycle.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	if b.cancel != nil {
		b.mu.Unlock()
		return ErrAlreadyConnected
	}
	runCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.done = make(chan error, 1)
	b.stopOnce = sync.Once{}
	b.mu.Unlock()

	var err error
	switch b.BotType {
	case SlackBotType:
		err = b.connectSlack(runCtx)
	case DiscordBotType:
		err = b.connectDiscord(runCtx)
	case CLIBotType:
		err = b.connectCLI(runCtx)
	default:
		err = ErrUnknownBotType
	}
	if err != nil {
		cancel()
		b.mu.Lock()
		b.cancel = nil
		b.mu.Unlock()
		return err
	}
	return nil
}

// Disconnect stops event delivery and closes the platform connection. It is
// safe to call more than once.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	cancel := b.cancel
	b.cancel = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	var err error
	b.stopOnce.Do(func() {
		switch b.BotType {
		case SlackBotType:
			err = b.disconnectSlack()
		case DiscordBotType:
			err = b.disconnectDiscord()
		case CLIBotType:
			err = b.disconnectCLI()
		default:
			err = ErrUnknownBotType
		}
	})
	return err
}

// Run connects the bot and blocks until ctx is canceled or the underlying
// event loop ends, then disconnects cleanly. It is the simplest way to operate
// a bot:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	if err := bot.Run(ctx); err != nil {
//		log.Fatal(err)
//	}
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

	if err := b.Disconnect(); err != nil && loopErr == nil {
		loopErr = err
	}
	return loopErr
}

// Start connects the bot and blocks until an interrupt (SIGINT/SIGTERM) is
// received, then shuts down gracefully. It is shorthand for [Bot.Run] with a
// signal-bound context.
func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}

// SendMessage sends text to the given channel using a background context.
func (b *Bot) SendMessage(channelID, text string) error {
	return b.SendMessageContext(context.Background(), channelID, text)
}

// SendMessageContext sends text to the given channel, honoring the context for
// cancellation and deadlines where the platform supports it.
func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string) error {
	switch b.BotType {
	case SlackBotType:
		_, _, err := b.SlackClient.PostMessageContext(ctx, channelID, slack.MsgOptionText(text, false))
		return err
	case DiscordBotType:
		_, err := b.DiscordSession.ChannelMessageSend(channelID, text, discordgo.WithContext(ctx))
		return err
	case CLIBotType:
		_, err := fmt.Fprintln(b.cliOut, text)
		return err
	default:
		return ErrUnknownBotType
	}
}

// GetAttachments returns the attachments of a message in a platform-agnostic
// form.
func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	switch b.BotType {
	case SlackBotType:
		return getAttachmentsFromSlackMessage(message.SlackData), nil
	case DiscordBotType:
		if message.DiscordData == nil {
			return nil, nil
		}
		return getAttachmentsFromDiscordMessage(message.DiscordData.Message), nil
	case CLIBotType:
		if message.CLIData == nil {
			return nil, nil
		}
		return message.CLIData.Attachments, nil
	default:
		return nil, ErrUnknownBotType
	}
}

// dispatch runs the middleware chain and command matching for a message. A
// panic in a handler or middleware is recovered and logged so that a single bad
// message cannot bring down the bot's event loop.
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
