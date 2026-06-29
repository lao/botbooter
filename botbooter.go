// Package botbooter is a small framework for building chat bots that behave the
// same way across Slack, Discord, Telegram, WhatsApp and a local CLI. It is a
// thin facade over the internal packages, so consumers keep a single import path.
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
	"github.com/lao/botbooter/internal/whatsapp"
)

// Errors returned by [Bot] methods and platform helpers.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
	// ErrMissingWhatsAppConfig is returned by InitAsWhatsAppBot when a required
	// WhatsAppConfig field is empty.
	ErrMissingWhatsAppConfig = whatsapp.ErrMissingConfig
)

// BotType identifies the messaging platform a [Bot] is connected to.
type BotType = core.BotType

// Supported bot types.
const (
	SlackBotType    = core.SlackBotType
	DiscordBotType  = core.DiscordBotType
	CLIBotType      = core.CLIBotType
	TelegramBotType = core.TelegramBotType
	WhatsAppBotType = core.WhatsAppBotType
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
	// WhatsAppMessage is the parsed payload of a WhatsApp message. See [whatsapp.Message].
	WhatsAppMessage = whatsapp.Message
	// WhatsAppMedia identifies media attached to a WhatsApp message. See [whatsapp.Media].
	WhatsAppMedia = whatsapp.Media
	// WhatsAppConfig configures a WhatsApp Cloud API bot. See [whatsapp.Config].
	WhatsAppConfig = whatsapp.Config
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

// InitAsWhatsAppBot creates a WhatsApp bot backed by the Meta Cloud API. It runs
// an inbound webhook HTTP server at cfg.Addr, so put a TLS-terminating proxy in
// front and register the public HTTPS URL in Meta's webhook settings. Inbound
// media arrives as an id in Attachment.ExtraData (not a URL); resolve the bytes
// with GET /{media-id} using your access token. It returns an error if a
// required config field is missing.
func InitAsWhatsAppBot(cfg WhatsAppConfig) (*Bot, error) {
	return whatsapp.New(cfg)
}

// DiscordRawEvent returns the raw Discord event carried on m, reporting whether m originated from Discord.
func DiscordRawEvent(m *Message) (*discordgo.MessageCreate, bool) { return discord.RawEvent(m) }

// SlackRawEvent returns the raw Slack event carried on m, reporting whether m originated from Slack.
func SlackRawEvent(m *Message) (*slackevents.MessageEvent, bool) { return slack.RawEvent(m) }

// TelegramRawEvent returns the raw Telegram update carried on m, reporting whether m originated from Telegram.
func TelegramRawEvent(m *Message) (*models.Update, bool) { return telegram.RawUpdate(m) }

// CLIRawEvent returns the parsed CLI line carried on m, reporting whether m originated from the CLI adapter.
func CLIRawEvent(m *Message) (*CLIMessage, bool) { return cli.RawData(m) }

// WhatsAppRawEvent returns the parsed WhatsApp message carried on m, reporting
// whether m originated from WhatsApp. AuthorName and Timestamp on the returned
// value are enriched, not present in its Raw JSON.
func WhatsAppRawEvent(m *Message) (*WhatsAppMessage, bool) { return whatsapp.RawMessage(m) }

// DiscordSession returns the discordgo session backing b, or nil if b is not a Discord bot.
func DiscordSession(b *Bot) *discordgo.Session { return discord.Session(b) }

// SlackClient returns the Slack Web API client backing b, or nil if b is not a Slack bot.
func SlackClient(b *Bot) *slackapi.Client { return slack.Client(b) }

// SlackSocketClient returns the Socket Mode client backing b, or nil if b is not a Slack bot.
func SlackSocketClient(b *Bot) *socketmode.Client { return slack.SocketClient(b) }

// TelegramClient returns the go-telegram bot client backing b, or nil if b is not a Telegram bot.
func TelegramClient(b *Bot) *bot.Bot { return telegram.Client(b) }

// TelegramEnvSuppressURLWarning names the environment variable that silences the
// plaintext-token warning logged on every successful Telegram resolve via
// [Bot.ResolveAttachmentURL]. Set it to any non-empty value to opt out.
const TelegramEnvSuppressURLWarning = telegram.EnvSuppressURLWarning
