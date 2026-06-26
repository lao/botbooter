// Package slack is the Slack adapter for botbooter, connecting via Socket Mode.
package slack

import (
	"context"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/core"
)

type adapter struct {
	client *slackapi.Client
	socket *socketmode.Client
}

// New creates a Slack bot that connects via Socket Mode.
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

// Disconnect is a no-op: the loop is driven by the run context, so canceling it
// is what stops the connection.
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
