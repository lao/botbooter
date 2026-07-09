// Package botbooter holds the platform-agnostic shared types for building chat
// bots that behave the same way across Slack, Discord, Telegram, WhatsApp,
// Microsoft Teams and a local CLI.
//
// This package is SDK-free: it imports no platform SDK and only re-exports the
// shared types from internal/core. Construct a bot from one of the per-platform
// packages — botbooter/slack, botbooter/discord, botbooter/telegram,
// botbooter/whatsapp/cloud, botbooter/whatsapp/whatsmeow, botbooter/teams or
// botbooter/cli — each of which pulls in only its own platform SDK (WhatsApp
// Cloud API and Teams speak REST APIs over plain HTTP and need none), then drive
// it through the shared types re-exported here. A bot that uses one platform
// never compiles the other platforms' SDKs into its binary.
//
// WhatsApp comes in two flavors selected by import path: whatsapp/cloud (Meta
// Cloud API webhook, needs a Meta Business account and a public HTTPS URL) and
// whatsapp/whatsmeow (WhatsApp Web multidevice protocol via whatsmeow, QR-linked
// to a phone, no Meta account or webhook needed).
package botbooter

import "github.com/lao/botbooter/internal/core"

// Errors returned by [Bot] methods.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
	ErrNilMessage       = core.ErrNilMessage
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
	TeamsBotType    = core.TeamsBotType
	// WhatsMeowBotType is the WhatsApp Web (whatsmeow) flavor; WhatsAppBotType is
	// the Meta Cloud API flavor.
	WhatsMeowBotType = core.WhatsMeowBotType
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
	// SendOption modifies a send. See [core.SendOption].
	SendOption = core.SendOption
)

// InReplyTo anchors a send on m so the adapter posts into m's thread or
// reply-chain, deriving the correct per-platform anchor. See [core.InReplyTo].
func InReplyTo(m *Message) SendOption { return core.InReplyTo(m) }

// WithThreadID anchors a send on a raw native id the adapter uses verbatim; it
// takes precedence over [InReplyTo]. See [core.WithThreadID].
func WithThreadID(id string) SendOption { return core.WithThreadID(id) }
