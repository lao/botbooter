// Package botbooter holds the platform-agnostic shared types for building chat
// bots that behave the same way across Slack, Discord, Telegram, WhatsApp,
// Microsoft Teams, GitHub, GitLab, Signal and a local CLI.
//
// This package is SDK-free: it imports no platform SDK and only re-exports the
// shared types from internal/core. Construct a bot from one of the per-platform
// packages — botbooter/slack, botbooter/discord, botbooter/telegram,
// botbooter/whatsapp/cloud, botbooter/whatsapp/whatsmeow, botbooter/teams,
// botbooter/github, botbooter/gitlab, botbooter/signal or botbooter/cli — each
// of which pulls in only its own platform SDK (WhatsApp Cloud API and Teams
// speak REST APIs over plain HTTP and need none; GitHub pulls google/go-github
// plus bradleyfalzon/ghinstallation for GitHub App auth; GitLab pulls
// gitlab.com/gitlab-org/api/client-go; Signal speaks REST plus a receive
// WebSocket to a signal-cli-rest-api container and pulls only the
// gorilla/websocket transport), then drive it through the shared types
// re-exported here. A bot that uses one platform never compiles the other
// platforms' SDKs into its binary.
//
// WhatsApp comes in two flavors selected by import path: whatsapp/cloud (Meta
// Cloud API webhook, needs a Meta Business account and a public HTTPS URL) and
// whatsapp/whatsmeow (WhatsApp Web multidevice protocol via whatsmeow, QR-linked
// to a phone, no Meta account or webhook needed).
//
// Every type here is a re-exported alias of its [internal/core] counterpart, so
// the full method and field documentation lives on the core types (pkg.go.dev
// resolves the [core.Bot]-style links to them). The summaries below capture the
// contracts most consumers need from the public import path.
package botbooter

import "github.com/lao/botbooter/internal/core"

// Errors returned by [Bot] methods.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
	ErrNilMessage       = core.ErrNilMessage
)

// Errors returned by [Bot.HandleFlow]; check them with errors.Is.
var (
	ErrFlowNil               = core.ErrFlowNil
	ErrFlowEmptyID           = core.ErrFlowEmptyID
	ErrFlowNoSteps           = core.ErrFlowNoSteps
	ErrFlowEmptyStepKey      = core.ErrFlowEmptyStepKey
	ErrFlowEmptyStepPrompt   = core.ErrFlowEmptyStepPrompt
	ErrFlowDuplicateKey      = core.ErrFlowDuplicateKey
	ErrFlowNoOnComplete      = core.ErrFlowNoOnComplete
	ErrFlowAlreadyRegistered = core.ErrFlowAlreadyRegistered
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
	GitHubBotType    = core.GitHubBotType
	GitLabBotType    = core.GitLabBotType
	SignalBotType    = core.SignalBotType
)

type (
	// Bot is the platform-agnostic chat bot. Register every command handler,
	// middleware, flow (HandleFlow) and reaction handler BEFORE calling Connect
	// or Run; registering after Connect races the dispatch goroutine. After
	// Connect, Connect/Run/Disconnect/Send are safe to call concurrently. See
	// [core.Bot].
	Bot = core.Bot
	// Message is an incoming message handed to handlers. UserID, ChannelID and
	// Content are always set; the other normalized fields (AuthorName, ReplyToID,
	// Timestamp, MentionedUserIDs) are best-effort per platform, and Raw carries
	// the platform's untouched event for the matching typed accessor. See
	// [core.Message].
	Message = core.Message
	// Command pairs a regexp pattern with a handler. See [core.Command].
	Command = core.Command
	// Attachment is a platform-agnostic file attachment. See [core.Attachment].
	Attachment = core.Attachment
	// CommandHandler handles a matched message. See [core.CommandHandler].
	CommandHandler = core.CommandHandler
	// Middleware wraps message dispatch. See [core.Middleware].
	Middleware = core.Middleware

	// Flow is a declarative multi-step conversational dialog. See [core.Flow].
	Flow = core.Flow
	// Answers is the read-only set of answers collected by a flow. See [core.Answers].
	Answers = core.Answers
	// AskOption configures a flow step (see [Validate], [Secret]). See [core.AskOption].
	AskOption = core.AskOption

	// Reaction is an emoji reaction added to a message. See [core.Reaction].
	Reaction = core.Reaction
	// ReactionHandler handles a reaction. See [core.ReactionHandler].
	ReactionHandler = core.ReactionHandler
	// SendOption modifies a send. See [core.SendOption].
	SendOption = core.SendOption
)

// NewFlow starts building a multi-step conversational [Flow] with the given
// stable id, registered via [Bot.HandleFlow]. See [core.NewFlow].
func NewFlow(id string) *Flow { return core.NewFlow(id) }

// Validate attaches a validator to a flow step; a non-nil error re-prompts the
// step. See [core.Validate].
func Validate(fn func(string) error) AskOption { return core.Validate(fn) }

// Secret marks a flow step's answer sensitive: kept out of framework logs and any
// future serialized Store state. It is not encryption. See [core.Secret].
func Secret() AskOption { return core.Secret() }

// InReplyTo anchors a send on m so the adapter posts into m's thread or
// reply-chain, deriving the correct per-platform anchor. See [core.InReplyTo].
func InReplyTo(m *Message) SendOption { return core.InReplyTo(m) }

// WithThreadID anchors a send on a raw native id the adapter uses verbatim; it
// takes precedence over [InReplyTo]. See [core.WithThreadID].
func WithThreadID(id string) SendOption { return core.WithThreadID(id) }
