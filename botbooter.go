// Package botbooter is a small framework for building chat bots that behave the
// same way across Slack, Discord and a local CLI. A single [Bot] abstracts over
// the platforms; you register [Command] handlers and optional [Middleware],
// then run the bot.
//
// This package is a thin facade over the implementation in the internal
// packages, so that consumers keep a single import path.
package botbooter

import (
	"io"

	"github.com/lao/botbooter/internal/cli"
	"github.com/lao/botbooter/internal/core"
	"github.com/lao/botbooter/internal/discord"
	"github.com/lao/botbooter/internal/slack"
)

// Errors returned by [Bot] methods.
var (
	ErrUnknownBotType   = core.ErrUnknownBotType
	ErrAlreadyConnected = core.ErrAlreadyConnected
)

// BotType identifies the messaging platform a [Bot] is connected to.
type BotType = core.BotType

// Supported bot types.
const (
	SlackBotType   = core.SlackBotType
	DiscordBotType = core.DiscordBotType
	CLIBotType     = core.CLIBotType
)

// Core types, re-exported from the internal core package. These are aliases, so
// values are interchangeable with the internal package.
type (
	Bot            = core.Bot
	Message        = core.Message
	CLIMessage     = core.CLIMessage
	Command        = core.Command
	Attachment     = core.Attachment
	CommandHandler = core.CommandHandler
	Middleware     = core.Middleware
)

// InitAsSlackBot creates a Slack bot that connects via Socket Mode. appToken is
// the app-level token (xapp-...) and botToken is the bot token (xoxb-...).
func InitAsSlackBot(appToken, botToken string) *Bot {
	return slack.New(appToken, botToken)
}

// InitAsDiscordBot creates a Discord bot from a bot token. It returns an error
// if the token cannot be used to construct a session.
func InitAsDiscordBot(token string) (*Bot, error) {
	return discord.New(token)
}

// InitAsCLIBot creates a bot that reads newline-delimited messages from in and
// writes replies to out. When in or out is nil, os.Stdin and os.Stdout are used
// respectively. It is intended for trusted, local input only.
func InitAsCLIBot(in io.Reader, out io.Writer) *Bot {
	return cli.New(in, out)
}
