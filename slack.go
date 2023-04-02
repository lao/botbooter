package botbooter

import (
	"context"
	"fmt"
	"log"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func InitAsSlackBot(appToken, botToken string) *Bot {
	client := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)

	socketClient := socketmode.New(client)

	return &Bot{
		SlackClient:           client,
		SlackSocketClient:     socketClient,
		Commands:              []Command{},
		UnknownCommandHandler: nil,
	}
}

func (b *Bot) connectSlack() error {
	fmt.Println("Connecting...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func(ctx context.Context) {
		fmt.Println("Listening for events...")
		for {
			select {
			case <-ctx.Done():
				fmt.Println("Done")
				return
			case evt := <-b.SlackSocketClient.Events:
				switch evt.Type {
				case socketmode.EventTypeEventsAPI:
					payload, ok := evt.Data.(slackevents.EventsAPIEvent)
					if !ok {
						fmt.Println("Ignored")
						continue
					}
					b.SlackSocketClient.Ack(*evt.Request)
					log.Println("Event received: ", payload.Type)
					b.handleSlackEventsApi(payload)
				}
			}
		}
	}(ctx)

	err := b.SlackSocketClient.Run()
	return err
}

func isSlackBotMessage(event slackevents.EventsAPIEvent) bool {
	// get event data
	data := event.InnerEvent.Data

	// type switch to get message event
	switch ev := data.(type) {
	case *slackevents.MessageEvent:
		// if bot id is not empty then it is a bot message
		if ev.BotID != "" || ev.SubType == "bot_message" || ev.Text == "" {
			return true
		}
	case *slackevents.AppMentionEvent:
		if ev.BotID != "" {
			return true
		}
	case *slackevents.MessageMetadataPostedEvent:
		if ev.BotId != "" {
			return true
		}
	case *slackevents.MessageMetadataUpdatedEvent:
		if ev.BotId != "" {
			return true
		}
	case *slackevents.MessageMetadataDeletedEvent:
		if ev.BotId != "" {
			return true
		}
	default:
		return false
	}

	return false
}

func logMessageEvent(ev *slackevents.MessageEvent) {
	log.Println(ev.Text)
	log.Println(ev.Files)
	log.Println(ev.User)
	log.Println(ev.Channel)
	log.Println(ev.BotID)
	log.Println(ev.SubType)
	log.Println(ev.ClientMsgID)
}

func (b *Bot) handleSlackEventsApi(e slackevents.EventsAPIEvent) {

	if isSlackBotMessage(e) {
		return
	}

	switch ev := e.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		logMessageEvent(ev)
		msg := e.InnerEvent.Data.(*slackevents.MessageEvent)

		fmt.Println("Handlers:", b.Commands)

		message := &Message{
			UserID:    msg.User,
			ChannelID: msg.Channel,
			Content:   msg.Text,
			SlackData: msg,
		}

		b.handleMessageWithCommand(message)
	}
}

func (b *Bot) disconnectSlack() error {
	close(b.SlackSocketClient.Events)
	return nil
}
func getAttachmentsFromSlackMessage(m *slackevents.MessageEvent) []Attachment {
	var attachments []Attachment

	for _, file := range m.Files {
		isImage := file.Mimetype != "" && file.Mimetype[:5] == "image"
		attachments = append(attachments, Attachment{
			IsImage:   isImage,
			URL:       file.URLPrivate,
			ExtraData: file,
		})
	}

	return attachments
}
