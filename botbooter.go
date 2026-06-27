// Package botbooter is a small framework for building chat bots that behave the
// same way across Slack, Discord and a local CLI. It is a thin facade over the
// internal packages, so consumers keep a single import path.
package botbooter

import (
	"io"

	"github.com/bwmarrin/discordgo"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/cli"
	"github.com/lao/botbooter/internal/core"
	"github.com/lao/botbooter/internal/discord"
	"github.com/lao/botbooter/internal/slack"
	"github.com/lao/botbooter/internal/telegram"
	"github.com/lao/botbooter/internal/webhook"
)

// Errors returned by [Bot] methods.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
	// ErrWebhookNoSender is returned by SendMessage on a webhook bot created
	// without a [WebhookSender]. See [webhook.ErrNoSender].
	ErrWebhookNoSender = webhook.ErrNoSender
)

// BotType identifies the messaging platform a [Bot] is connected to.
type BotType = core.BotType

// Supported bot types.
const (
	SlackBotType    = core.SlackBotType
	DiscordBotType  = core.DiscordBotType
	CLIBotType      = core.CLIBotType
	TelegramBotType = core.TelegramBotType
	WebhookBotType  = core.WebhookBotType
)

type (
	// Bot is the platform-agnostic chat bot. See [core.Bot].
	Bot = core.Bot
	// Message is an incoming message handed to handlers. See [core.Message].
	Message = core.Message
	// CLIMessage is the raw payload of a CLI message. See [core.CLIMessage].
	CLIMessage = core.CLIMessage
	// Command pairs a regexp pattern with a handler. See [core.Command].
	Command = core.Command
	// Attachment is a platform-agnostic file attachment. See [core.Attachment].
	Attachment = core.Attachment
	// CommandHandler handles a matched message. See [core.CommandHandler].
	CommandHandler = core.CommandHandler
	// Middleware wraps message dispatch. See [core.Middleware].
	Middleware = core.Middleware
	// WebhookConfig configures a webhook bot. See [webhook.Config].
	WebhookConfig = webhook.Config
	// WebhookPayload is the default JSON body of a webhook message. See [webhook.Payload].
	WebhookPayload = webhook.Payload
	// WebhookDecoder turns an HTTP request into a [Message]. See [webhook.Decoder].
	WebhookDecoder = webhook.Decoder
	// WebhookSender delivers a webhook bot's outbound replies. See [webhook.Sender].
	WebhookSender = webhook.Sender
)

// InitAsSlackBot creates a Slack bot that connects via Socket Mode.
func InitAsSlackBot(appToken, botToken string) *Bot {
	return slack.New(appToken, botToken)
}

// InitAsDiscordBot creates a Discord bot that connects via the Gateway.
func InitAsDiscordBot(token string) (*Bot, error) {
	return discord.New(token)
}

// InitAsTelegramBot creates a Telegram bot that connects via the Bot API.
func InitAsTelegramBot(token string) (*Bot, error) {
	return telegram.New(token)
}

// InitAsCLIBot creates a local CLI bot.
func InitAsCLIBot(in io.Reader, out io.Writer) *Bot {
	return cli.New(in, out)
}

// InitAsWebhookBot creates a bot that receives messages over HTTP POST. With the
// zero [WebhookConfig] it listens on :8080 and decodes the default JSON payload;
// pair it with (*Bot).WithWorkers for asynchronous, per-channel sharded dispatch
// under load.
func InitAsWebhookBot(cfg WebhookConfig) *Bot {
	return webhook.New(cfg)
}

// DiscordRawEvent returns the raw Discord event carried on m, reporting whether
// m originated from Discord.
func DiscordRawEvent(m *Message) (*discordgo.MessageCreate, bool) { return discord.RawEvent(m) }

// SlackRawEvent returns the raw Slack event carried on m, reporting whether m
// originated from Slack.
func SlackRawEvent(m *Message) (*slackevents.MessageEvent, bool) { return slack.RawEvent(m) }

// TelegramRawEvent returns the raw Telegram update carried on m, reporting
// whether m originated from Telegram.
func TelegramRawEvent(m *Message) (*models.Update, bool) { return telegram.RawUpdate(m) }

// CLIRawEvent returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func CLIRawEvent(m *Message) (*CLIMessage, bool) { return cli.RawData(m) }

// WebhookRawPayload returns the default-decoded payload carried on m, reporting
// whether m came from a webhook bot using the default decoder.
func WebhookRawPayload(m *Message) (*WebhookPayload, bool) { return webhook.RawPayload(m) }

// DiscordSession returns the discordgo session backing b, or nil if b is not a
// Discord bot.
func DiscordSession(b *Bot) *discordgo.Session { return discord.Session(b) }

// SlackClient returns the Slack Web API client backing b, or nil if b is not a
// Slack bot.
func SlackClient(b *Bot) *slackapi.Client { return slack.Client(b) }

// SlackSocketClient returns the Socket Mode client backing b, or nil if b is not
// a Slack bot.
func SlackSocketClient(b *Bot) *socketmode.Client { return slack.SocketClient(b) }

// TelegramClient returns the go-telegram bot client backing b, or nil if b is
// not a Telegram bot.
func TelegramClient(b *Bot) *bot.Bot { return telegram.Client(b) }
