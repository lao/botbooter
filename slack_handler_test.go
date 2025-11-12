package botbooter

import (
	"testing"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// TestHandleSlackSocketEvent tests the Slack socket event handler logic
func TestHandleSlackSocketEvent(t *testing.T) {
	t.Run("ValidEventsAPIEvent", func(t *testing.T) {
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

		// Create a valid EventsAPI event
		innerEvent := slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{
					Text:    "hello",
					User:    "U123",
					Channel: "C456",
				},
			},
		}

		evt := socketmode.Event{
			Type: socketmode.EventTypeEventsAPI,
			Data: innerEvent,
			Request: &socketmode.Request{
				EnvelopeID: "test-envelope",
			},
		}

		// Act
		bot.handleSlackSocketEvent(evt)

		// Assert
		assertTrue(t, handlerCalled, "Handler should be called for valid message event")
	})

	t.Run("InvalidTypeAssertion", func(t *testing.T) {
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

		// Create an event with invalid data type (not EventsAPIEvent)
		evt := socketmode.Event{
			Type: socketmode.EventTypeEventsAPI,
			Data: "invalid data type", // This should fail type assertion
			Request: &socketmode.Request{
				EnvelopeID: "test-envelope",
			},
		}

		// Act - This should handle the failed type assertion gracefully
		bot.handleSlackSocketEvent(evt)

		// Assert
		assertFalse(t, handlerCalled, "Handler should not be called for invalid event data")
	})

	t.Run("NonEventsAPIEventType", func(t *testing.T) {
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

		// Create an event with a different type
		evt := socketmode.Event{
			Type: socketmode.EventTypeConnecting,
			Data: nil,
		}

		// Act
		bot.handleSlackSocketEvent(evt)

		// Assert
		assertFalse(t, handlerCalled, "Handler should not be called for non-EventsAPI event types")
	})
}
