// Package slack is the Slack adapter for botbooter. It connects via Socket Mode
// and implements core.Adapter.
package slack

import (
	"context"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/core"
)

// adapter is the Slack implementation of core.Adapter.
type adapter struct {
	client *slackapi.Client
	socket *socketmode.Client
}

// New creates a Slack bot that connects via Socket Mode. appToken is the
// app-level token (xapp-...) and botToken is the bot token (xoxb-...).
func New(appToken, botToken string) *core.Bot {
	client := slackapi.New(
		botToken,
		slackapi.OptionAppLevelToken(appToken),
	)
	socket := socketmode.New(client)

	bot := core.New(core.SlackBotType, &adapter{client: client, socket: socket})
	bot.SlackClient = client
	bot.SlackSocketClient = socket
	return bot
}

// Connect starts the Socket Mode event loop in the background. It returns
// immediately; the loop runs until ctx is canceled.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-a.socket.Events:
				if !ok {
					return
				}
				a.handleSocketEvent(ctx, evt, deps)
			}
		}
	}()

	go func() {
		deps.Done(a.socket.RunContext(ctx))
	}()

	return nil
}

// Disconnect stops the Socket Mode loop. The loop is driven by the run context,
// so canceling it (via Bot.Disconnect) is what actually stops the connection;
// there is nothing else to close.
func (a *adapter) Disconnect() error {
	return nil
}

// Send posts text to channelID via the Web API.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	_, _, err := a.client.PostMessageContext(ctx, channelID, slackapi.MsgOptionText(text, false))
	return err
}

// Attachments returns the files attached to the message's Slack event.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	return attachmentsFromMessage(m.SlackData), nil
}

// handleSocketEvent acknowledges and routes a single Socket Mode event,
// handling only Events API payloads.
func (a *adapter) handleSocketEvent(ctx context.Context, evt socketmode.Event, deps core.AdapterDeps) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		payload, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		if evt.Request != nil {
			a.socket.Ack(*evt.Request)
		}
		a.handleEventsAPI(ctx, payload, deps)
	}
}

// handleEventsAPI dispatches a user message from an Events API payload,
// dropping events that originate from a bot to avoid reply loops.
func (a *adapter) handleEventsAPI(ctx context.Context, e slackevents.EventsAPIEvent, deps core.AdapterDeps) {
	if isBotMessage(e) {
		return
	}

	if msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent); ok {
		deps.Dispatch(ctx, &core.Message{
			UserID:    msg.User,
			ChannelID: msg.Channel,
			Content:   msg.Text,
			SlackData: msg,
		})
	}
}

// isBotMessage reports whether an event originated from a bot (or is otherwise
// not a user message we should handle), so we can ignore it and avoid reply
// loops.
func isBotMessage(event slackevents.EventsAPIEvent) bool {
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		return ev.BotID != "" || ev.SubType == "bot_message" || (ev.Text == "" && len(ev.Files) == 0)
	case *slackevents.AppMentionEvent:
		return ev.BotID != ""
	case *slackevents.MessageMetadataPostedEvent:
		return ev.BotId != ""
	case *slackevents.MessageMetadataUpdatedEvent:
		return ev.BotId != ""
	case *slackevents.MessageMetadataDeletedEvent:
		return ev.BotId != ""
	default:
		return false
	}
}

// attachmentsFromMessage converts a Slack message event's files into
// platform-agnostic attachments, returning nil for a nil event.
func attachmentsFromMessage(m *slackevents.MessageEvent) []core.Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]core.Attachment, 0, len(m.Files))
	for _, file := range m.Files {
		attachments = append(attachments, core.Attachment{
			IsImage:   strings.HasPrefix(file.Mimetype, "image"),
			URL:       file.URLPrivate,
			ExtraData: file,
		})
	}
	return attachments
}
