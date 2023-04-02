package botbooter

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type BotType int

const (
	SlackBotType BotType = iota
	DiscordBotType
)

type Bot struct {
	BotType               BotType
	DiscordSession        *discordgo.Session
	SlackClient           *slack.Client
	SlackSocketClient     *socketmode.Client
	Commands              []Command
	UnknownCommandHandler UnknownCommandHandler
	Middlewares           []Middleware
}

type Message struct {
	UserID      string
	ChannelID   string
	Content     string
	DiscordData *discordgo.MessageCreate
	SlackData   *slackevents.MessageEvent
}

type CommandHandler func(bot *Bot, message *Message)

type Command struct {
	Pattern string
	Handler CommandHandler
}

type UnknownCommandHandler func(bot *Bot, message *Message)

type Middleware func(bot *Bot, message *Message, next CommandHandler)

type Attachment struct {
	IsImage   bool
	URL       string
	ExtraData interface{}
}

func (b *Bot) Connect() error {
	switch b.BotType {
	case SlackBotType:
		return b.connectSlack()
	case DiscordBotType:
		return b.connectDiscord()
	default:
		return fmt.Errorf("Unknown bot type")
	}
}

func (b *Bot) Disconnect() error {
	switch b.BotType {
	case SlackBotType:
		return b.disconnectSlack()
	case DiscordBotType:
		return b.disconnectDiscord()
	default:
		return fmt.Errorf("Unknown bot type")
	}
}

func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	switch b.BotType {
	case SlackBotType:
		return getAttachmentsFromSlackMessage(message.SlackData), nil
	case DiscordBotType:
		return getAttachmentsFromDiscordMessage(message.DiscordData.Message), nil
	default:
		return nil, errors.New("Unknown bot type")
	}
}

func (b *Bot) SendMessage(channelID string, message string) error {
	switch b.BotType {
	case SlackBotType:
		_, _, err := b.SlackClient.PostMessage(
			channelID,
			slack.MsgOptionText(message, false),
		)
		return err
	case DiscordBotType:
		_, err := b.DiscordSession.ChannelMessageSend(channelID, message)
		return err
	default:
		return fmt.Errorf("Unknown bot type")
	}
}

func (b *Bot) AddHandler(handler Command) {
	fmt.Println("Adding handler:", handler)
	b.Commands = append(b.Commands, handler)
}

func (b *Bot) SetUnknownCommandHandler(handler UnknownCommandHandler) {
	b.UnknownCommandHandler = handler
}

func (b *Bot) StartListening() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	log.Println("Bot is shutting down...")
	err := b.Disconnect()
	if err != nil {
		log.Println("Failed to disconnect:", err)
	}
}

func (b *Bot) AddMiddleware(middleware Middleware) {
	b.Middlewares = append(b.Middlewares, middleware)
}

func (b *Bot) handleMessageWithCommand(message *Message) {
	handler := func(bot *Bot, message *Message) {
		for _, command := range bot.Commands {
			matched, err := regexp.MatchString(command.Pattern, message.Content)
			if err == nil && matched {
				command.Handler(bot, message)
				return
			}
		}
		if bot.UnknownCommandHandler != nil {
			bot.UnknownCommandHandler(bot, message)
		}
	}

	finalHandler := handler
	for i := len(b.Middlewares) - 1; i >= 0; i-- {
		middleware := b.Middlewares[i]
		next := finalHandler
		finalHandler = func(bot *Bot, message *Message) {
			middleware(bot, message, next)
		}
	}

	finalHandler(b, message)
}
