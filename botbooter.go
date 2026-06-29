// Package botbooter holds the platform-agnostic shared types for building chat
// bots that behave the same way across Slack, Discord, Telegram, WhatsApp and a
// local CLI.
//
// This package is SDK-free: it imports no platform SDK and only re-exports the
// shared types from internal/core. Construct a bot from one of the per-platform
// packages — botbooter/slack, botbooter/discord, botbooter/telegram,
// botbooter/whatsapp or botbooter/cli — each of which pulls in only its own
// platform SDK (WhatsApp speaks the Cloud API over plain HTTP and needs none),
// then drive it through the shared types re-exported here. A bot that uses one
// platform never compiles the other platforms' SDKs into its binary.
package botbooter

import "github.com/lao/botbooter/internal/core"

// Errors returned by [Bot] methods.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
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
)
