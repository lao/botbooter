package botbooter

import (
	"context"
	"testing"

	"github.com/slack-go/slack/slackevents"
)

func TestInitAsSlackBot(t *testing.T) {
	bot := InitAsSlackBot("app_token", "bot_token")

	assertNotNil(t, bot, "Bot should be initialized")
	assertEqual(t, bot.BotType, SlackBotType, "Bot type should be Slack")
	assertNotNil(t, bot.SlackClient, "Slack client should be initialized")
	assertNotNil(t, bot.SlackSocketClient, "Slack socket client should be initialized")
}

func TestIsSlackBotMessage(t *testing.T) {
	tests := []struct {
		name                 string
		event                slackevents.EventsAPIEvent
		expectedIsBotMessage bool
	}{
		{
			name:                 "message with bot ID",
			event:                slackEvent(&slackevents.MessageEvent{BotID: "B01"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "message with bot_message subtype",
			event:                slackEvent(&slackevents.MessageEvent{SubType: "bot_message", Text: "test"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "message with empty text",
			event:                slackEvent(&slackevents.MessageEvent{Text: ""}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "caption-less file upload",
			event:                slackEvent(&slackevents.MessageEvent{Text: "", User: "U123", Files: []slackevents.File{{Mimetype: "image/png"}}}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "user message",
			event:                slackEvent(&slackevents.MessageEvent{Text: "Hello from user"}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "app mention with bot ID",
			event:                slackEvent(&slackevents.AppMentionEvent{BotID: "B01"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "app mention without bot ID",
			event:                slackEvent(&slackevents.AppMentionEvent{}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "message metadata posted with bot ID",
			event:                slackEvent(&slackevents.MessageMetadataPostedEvent{BotId: "B01"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "message metadata posted without bot ID",
			event:                slackEvent(&slackevents.MessageMetadataPostedEvent{}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "message metadata updated with bot ID",
			event:                slackEvent(&slackevents.MessageMetadataUpdatedEvent{BotId: "B01"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "message metadata updated without bot ID",
			event:                slackEvent(&slackevents.MessageMetadataUpdatedEvent{}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "message metadata deleted with bot ID",
			event:                slackEvent(&slackevents.MessageMetadataDeletedEvent{BotId: "B01"}),
			expectedIsBotMessage: true,
		},
		{
			name:                 "message metadata deleted without bot ID",
			event:                slackEvent(&slackevents.MessageMetadataDeletedEvent{}),
			expectedIsBotMessage: false,
		},
		{
			name:                 "unknown event type",
			event:                slackEvent("some string"),
			expectedIsBotMessage: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertEqual(t, isSlackBotMessage(tt.event), tt.expectedIsBotMessage, "isSlackBotMessage result")
		})
	}
}

func TestGetAttachmentsFromSlackMessage(t *testing.T) {
	tests := []struct {
		name        string
		message     *slackevents.MessageEvent
		wantCount   int
		wantIsImage []bool
		wantURLs    []string
	}{
		{
			name: "single image attachment",
			message: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{Mimetype: "image/png", URLPrivate: "https://example.com/image.png"},
				},
			},
			wantCount:   1,
			wantIsImage: []bool{true},
			wantURLs:    []string{"https://example.com/image.png"},
		},
		{
			name: "multiple files with mixed types",
			message: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{Mimetype: "image/png", URLPrivate: "https://example.com/image1.png"},
					{Mimetype: "image/jpeg", URLPrivate: "https://example.com/image2.jpg"},
					{Mimetype: "application/pdf", URLPrivate: "https://example.com/document.pdf"},
				},
			},
			wantCount:   3,
			wantIsImage: []bool{true, true, false},
			wantURLs:    []string{"https://example.com/image1.png", "https://example.com/image2.jpg", "https://example.com/document.pdf"},
		},
		{
			name:        "no files",
			message:     &slackevents.MessageEvent{Files: []slackevents.File{}},
			wantCount:   0,
			wantIsImage: []bool{},
			wantURLs:    []string{},
		},
		{
			name: "short mimetype does not panic",
			message: &slackevents.MessageEvent{
				Files: []slackevents.File{{Mimetype: "img", URLPrivate: "https://example.com/x"}},
			},
			wantCount:   1,
			wantIsImage: []bool{false},
			wantURLs:    []string{"https://example.com/x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attachments := getAttachmentsFromSlackMessage(tt.message)

			assertEqual(t, len(attachments), tt.wantCount, "Number of attachments")
			for i := 0; i < len(attachments) && i < len(tt.wantIsImage); i++ {
				assertEqual(t, attachments[i].IsImage, tt.wantIsImage[i], "IsImage property for attachment")
			}
			for i := 0; i < len(attachments) && i < len(tt.wantURLs); i++ {
				assertEqual(t, attachments[i].URL, tt.wantURLs[i], "URL for attachment")
			}
		})
	}
}

func TestGetAttachmentsFromSlackMessage_Nil(t *testing.T) {
	assertEqual(t, len(getAttachmentsFromSlackMessage(nil)), 0, "nil message yields no attachments")
}

func TestHandleSlackEventsApi(t *testing.T) {
	t.Run("BotMessage", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.handleSlackEventsApi(context.Background(), slackEvent(&slackevents.MessageEvent{
			BotID: "B01",
			Text:  "hello",
		}))

		assertFalse(t, handlerCalled, "Handler should not be called for bot message")
	})

	t.Run("UserMessage", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		var got *Message
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			got = m
		})

		bot.handleSlackEventsApi(context.Background(), slackEvent(&slackevents.MessageEvent{
			Text:    "hello",
			User:    "U123",
			Channel: "C456",
		}))

		assertNotNil(t, got, "Handler should be called for user message")
		assertEqual(t, got.UserID, "U123", "message user")
		assertEqual(t, got.ChannelID, "C456", "message channel")
	})

	t.Run("NonMessageEvent", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		handlerCalled := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.handleSlackEventsApi(context.Background(), slackEvent(&slackevents.AppMentionEvent{
			Text:    "mention",
			User:    "U123",
			Channel: "C456",
		}))

		assertFalse(t, handlerCalled, "Handler should not be called for non-MessageEvent")
	})
}

func TestDisconnectSlack(t *testing.T) {
	bot := InitAsSlackBot("xapp-test", "xoxb-test")

	assertNoError(t, bot.disconnectSlack(), "Disconnect Slack should not fail")
}

// slackEvent wraps inner event data in an EventsAPIEvent for tests.
func slackEvent(data any) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: data},
	}
}
