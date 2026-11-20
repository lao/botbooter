// Package botbooter provides shared handlers, middleware and conversation flows
// for chat bots. Create a Bot with a platform package such as slack, discord,
// whatsapp/cloud or whatsapp/whatsmeow. Each imports only its own dependencies.
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
	SlackBotType         = core.SlackBotType
	DiscordBotType       = core.DiscordBotType
	CLIBotType           = core.CLIBotType
	TelegramBotType      = core.TelegramBotType
	WhatsAppCloudBotType = core.WhatsAppCloudBotType
	TeamsBotType         = core.TeamsBotType
	WhatsAppWebBotType   = core.WhatsAppWebBotType
	GitHubBotType        = core.GitHubBotType
	GitLabBotType        = core.GitLabBotType
	BitbucketBotType     = core.BitbucketBotType
	SignalBotType        = core.SignalBotType
)

// Earlier names for the WhatsApp transports.
const (
	WhatsAppBotType  = WhatsAppCloudBotType
	WhatsMeowBotType = WhatsAppWebBotType
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

// Secret marks a flow step's answer sensitive: kept out of framework logs. It is
// not encryption. See [core.Secret].
func Secret() AskOption { return core.Secret() }

// InReplyTo anchors a send on m so the adapter posts into m's thread or
// reply-chain, deriving the correct per-platform anchor. See [core.InReplyTo].
func InReplyTo(m *Message) SendOption { return core.InReplyTo(m) }

// WithThreadID anchors a send on a raw native id the adapter uses verbatim; it
// takes precedence over [InReplyTo]. See [core.WithThreadID].
func WithThreadID(id string) SendOption { return core.WithThreadID(id) }
