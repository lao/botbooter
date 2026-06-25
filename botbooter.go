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

type Bot struct {
	BotType BotType

	DiscordSession    *discordgo.Session
	SlackClient       *slack.Client
	SlackSocketClient *socketmode.Client

	cliIn  io.Reader
	cliOut io.Writer

	removeDiscordHandler func()

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	stop   func() error
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
		b.stop = nil
		b.mu.Unlock()
		return err
	}
	return nil
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
	switch b.BotType {
	case SlackBotType, DiscordBotType, CLIBotType:
		return nil
	default:
		return ErrUnknownBotType
	}
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
