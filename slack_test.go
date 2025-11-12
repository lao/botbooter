package botbooter

import (
	"testing"

	"github.com/slack-go/slack/slackevents"
)

func TestInitAsSlackBot(t *testing.T) {
	// Arrange
	appToken := "app_token"
	botToken := "bot_token"

	// Act
	bot := InitAsSlackBot(appToken, botToken)

	// Assert
	assertNotNil(t, bot, "Bot should be initialized")
	assertNotNil(t, bot.SlackClient, "Slack client should be initialized")
	assertNotNil(t, bot.SlackSocketClient, "Slack socket client should be initialized")
}

func TestIsSlackBotMessage(t *testing.T) {
	// Arrange - Table-driven test cases
	tests := []struct {
		name                 string
		event                slackevents.EventsAPIEvent
		expectedIsBotMessage bool
	}{
		{
			name: "message with bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageEvent{
						BotID: "B01",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "message with bot_message subtype",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageEvent{
						SubType: "bot_message",
						Text:    "test",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "message with empty text",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageEvent{
						Text: "",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "user message",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageEvent{
						Text: "Hello from user",
					},
				},
			},
			expectedIsBotMessage: false,
		},
		{
			name: "app mention with bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.AppMentionEvent{
						BotID: "B01",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "app mention without bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.AppMentionEvent{},
				},
			},
			expectedIsBotMessage: false,
		},
		{
			name: "message metadata posted with bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataPostedEvent{
						BotId: "B01",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "message metadata posted without bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataPostedEvent{},
				},
			},
			expectedIsBotMessage: false,
		},
		{
			name: "message metadata updated with bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataUpdatedEvent{
						BotId: "B01",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "message metadata updated without bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataUpdatedEvent{},
				},
			},
			expectedIsBotMessage: false,
		},
		{
			name: "message metadata deleted with bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataDeletedEvent{
						BotId: "B01",
					},
				},
			},
			expectedIsBotMessage: true,
		},
		{
			name: "message metadata deleted without bot ID",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageMetadataDeletedEvent{},
				},
			},
			expectedIsBotMessage: false,
		},
		{
			name: "unknown event type",
			event: slackevents.EventsAPIEvent{
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: "some string", // Not a recognized event type
				},
			},
			expectedIsBotMessage: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			isBotMessage := isSlackBotMessage(tt.event)

			// Assert
			assertEqual(t, isBotMessage, tt.expectedIsBotMessage, "isSlackBotMessage result")
		})
	}
}

func TestGetAttachmentsFromSlackMessage(t *testing.T) {
	// Arrange - Table-driven test cases
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
					{
						Mimetype:   "image/png",
						URLPrivate: "https://example.com/image.png",
					},
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
					{
						Mimetype:   "image/png",
						URLPrivate: "https://example.com/image1.png",
					},
					{
						Mimetype:   "image/jpeg",
						URLPrivate: "https://example.com/image2.jpg",
					},
					{
						Mimetype:   "application/pdf",
						URLPrivate: "https://example.com/document.pdf",
					},
				},
			},
			wantCount:   3,
			wantIsImage: []bool{true, true, false},
			wantURLs:    []string{"https://example.com/image1.png", "https://example.com/image2.jpg", "https://example.com/document.pdf"},
		},
		{
			name: "no files",
			message: &slackevents.MessageEvent{
				Files: []slackevents.File{},
			},
			wantCount:   0,
			wantIsImage: []bool{},
			wantURLs:    []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			attachments := getAttachmentsFromSlackMessage(tt.message)

			// Assert
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

func TestHandleSlackEventsApi(t *testing.T) {
	t.Run("BotMessage", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType

		handlerCalled := false
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)

		event := slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{
					BotID: "B01",
					Text:  "hello",
				},
			},
		}

		// Act
		bot.handleSlackEventsApi(event)

		// Assert
		// Handler should not be called for bot messages
		assertFalse(t, handlerCalled, "Handler should not be called for bot message")
	})

	t.Run("UserMessage", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType

		handlerCalled := false
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)

		event := slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{
					Text:    "hello",
					User:    "U123",
					Channel: "C456",
				},
			},
		}

		// Act
		bot.handleSlackEventsApi(event)

		// Assert
		// Handler should be called for user messages
		assertTrue(t, handlerCalled, "Handler should be called for user message")
	})
}

func TestDisconnectSlack(t *testing.T) {
	// Arrange
	bot := InitAsSlackBot("xapp-test", "xoxb-test")
	bot.BotType = SlackBotType

	// Act
	err := bot.disconnectSlack()

	// Assert
	assertNoError(t, err, "Disconnect Slack should not fail")
}

func TestHandleSlackEventsApi_NonMessageEvent(t *testing.T) {
	// Arrange
	bot := InitAsSlackBot("xapp-test", "xoxb-test")
	bot.BotType = SlackBotType

	handlerCalled := false
	handler := Command{
		Pattern: ".*",
		Handler: func(bot *Bot, message *Message) {
			handlerCalled = true
		},
	}
	bot.AddHandler(handler)

	// Create an event that's not a MessageEvent (e.g., AppMentionEvent)
	event := slackevents.EventsAPIEvent{
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{
				Text:    "mention",
				User:    "U123",
				Channel: "C456",
			},
		},
	}

	// Act
	bot.handleSlackEventsApi(event)

	// Assert
	// Handler should not be called for non-MessageEvent types
	assertFalse(t, handlerCalled, "Handler should not be called for non-MessageEvent")
}

func TestConnectSlack_EventHandling(t *testing.T) {
	t.Run("ValidEventsAPIEvent", func(t *testing.T) {
		// This test covers the event handling code path in connectSlack
		// by directly simulating the event through handleSlackEventsApi
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType

		handlerCalled := false
		handler := Command{
			Pattern: "^test$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)

		// Create a valid MessageEvent
		event := slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{
					Text:    "test",
					User:    "U123",
					Channel: "C456",
				},
			},
		}

		// Act - This simulates what happens in the event loop
		bot.handleSlackEventsApi(event)

		// Assert
		assertTrue(t, handlerCalled, "Handler should be called for valid message event")
	})
}
