// Package slack exposes the Slack (Socket Mode) constructor and the raw-event
// and client accessors for botbooter. Import it for a Slack bot; a Slack-only
// binary pulls in slack-go but never discordgo or go-telegram.
package slack

import (
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter"
	slackint "github.com/lao/botbooter/internal/slack"
)

// New creates a Slack bot that connects via Socket Mode.
func New(appToken, botToken string) *botbooter.Bot {
	return slackint.New(appToken, botToken)
}

// RawEvent returns the raw Slack message event carried on m, reporting whether m
// originated from Slack.
func RawEvent(m *botbooter.Message) (*slackevents.MessageEvent, bool) {
	return slackint.RawEvent(m)
}

// Client returns the Slack Web API client backing b, or nil if b is not a Slack
// bot.
func Client(b *botbooter.Bot) *slackapi.Client {
	return slackint.Client(b)
}

// SocketClient returns the Socket Mode client backing b, or nil if b is not a
// Slack bot.
func SocketClient(b *botbooter.Bot) *socketmode.Client {
	return slackint.SocketClient(b)
}
