package botbooter

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// InitAsSlackBot creates a Slack bot that connects via Socket Mode. appToken is
// the app-level token (xapp-...) and botToken is the bot token (xoxb-...).
func InitAsSlackBot(appToken, botToken string) *Bot {
	client := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)

	return &Bot{
		BotType:           SlackBotType,
		SlackClient:       client,
		SlackSocketClient: socketmode.New(client),
	}
}

// connectSlack starts the Socket Mode event loop in the background. It returns
// immediately; the loop runs until ctx is canceled.
func (b *Bot) connectSlack(ctx context.Context) error {
	done := b.done

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-b.SlackSocketClient.Events:
				if !ok {
					return
				}
				b.handleSlackSocketEvent(ctx, evt)
			}
		}
	}()

	go func() {
		done <- b.SlackSocketClient.RunContext(ctx)
	}()

	return nil
}

// disconnectSlack stops the Socket Mode loop. The loop is driven by the run
// context, so canceling it (via Bot.Disconnect) is what actually stops the
// connection; there is nothing else to close.
func (b *Bot) disconnectSlack() error {
	return nil
}

func (b *Bot) handleSlackSocketEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		payload, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		if evt.Request != nil {
			b.SlackSocketClient.Ack(*evt.Request)
		}
		b.handleSlackEventsApi(ctx, payload)
	}
}

func (b *Bot) handleSlackEventsApi(ctx context.Context, e slackevents.EventsAPIEvent) {
	if isSlackBotMessage(e) {
		return
	}

	if msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent); ok {
		b.dispatch(ctx, &Message{
			UserID:    msg.User,
			ChannelID: msg.Channel,
			Content:   msg.Text,
			SlackData: msg,
		})
	}
}

// isSlackBotMessage reports whether an event originated from a bot (or is
// otherwise not a user message we should handle), so we can ignore it and avoid
// reply loops.
func isSlackBotMessage(event slackevents.EventsAPIEvent) bool {
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		return ev.BotID != "" || ev.SubType == "bot_message" || ev.Text == ""
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

func getAttachmentsFromSlackMessage(m *slackevents.MessageEvent) []Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]Attachment, 0, len(m.Files))
	for _, file := range m.Files {
		attachments = append(attachments, Attachment{
			IsImage:   strings.HasPrefix(file.Mimetype, "image"),
			URL:       file.URLPrivate,
			ExtraData: file,
		})
	}
	return attachments
}
