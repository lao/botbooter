// Package slack is the Slack adapter for botbooter, connecting via Socket Mode.
package slack

import (
	"context"
	"strconv"
	"strings"
	"time"

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
	msg, _ := m.Raw.(*slackevents.MessageEvent)
	return attachmentsFromMessage(msg), nil
}

// RawEvent returns the raw Slack message event carried on m, reporting whether
// m originated from Slack.
func RawEvent(m *core.Message) (*slackevents.MessageEvent, bool) {
	e, ok := m.Raw.(*slackevents.MessageEvent)
	return e, ok
}

// Client returns the Slack Web API client backing b, or nil if b is not a Slack
// bot.
func Client(b *core.Bot) *slackapi.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

// SocketClient returns the Socket Mode client backing b, or nil if b is not a
// Slack bot.
func SocketClient(b *core.Bot) *socketmode.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.socket
	}
	return nil
}

// toMessage maps a Slack message event onto a platform-agnostic Message.
// AuthorName is left empty: the event carries only a user id, and resolving a
// name would require a per-message API call.
func toMessage(msg *slackevents.MessageEvent) *core.Message {
	return &core.Message{
		ID:        msg.TimeStamp,
		UserID:    msg.User,
		ChannelID: msg.Channel,
		Content:   msg.Text,
		Timestamp: parseSlackTimestamp(msg.TimeStamp),
		ReplyToID: msg.ThreadTimeStamp,
		Raw:       msg,
	}
}

// parseSlackTimestamp converts a Slack ts ("1700000000.000100", seconds with a
// 6-digit microsecond fraction) into a UTC time, returning the zero time for an
// empty or malformed value.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	secs, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var nsec int64
	if frac != "" {
		for len(frac) < 6 {
			frac += "0"
		}
		if micros, err := strconv.ParseInt(frac[:6], 10, 64); err == nil {
			nsec = micros * 1000
		}
	}
	return time.Unix(s, nsec).UTC()
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
		deps.Dispatch(ctx, toMessage(msg))
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
