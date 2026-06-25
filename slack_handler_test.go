package botbooter

import (
	"context"
	"testing"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// TestHandleSlackSocketEvent tests the Slack socket event handler logic.
func TestHandleSlackSocketEvent(t *testing.T) {
	t.Run("ValidEventsAPIEvent", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		evt := socketmode.Event{
			Type: socketmode.EventTypeEventsAPI,
			Data: slackEvent(&slackevents.MessageEvent{Text: "hello", User: "U123", Channel: "C456"}),
			Request: &socketmode.Request{
				EnvelopeID: "test-envelope",
			},
		}

		bot.handleSlackSocketEvent(context.Background(), evt)

		assertTrue(t, handlerCalled, "Handler should be called for valid message event")
	})

	t.Run("InvalidTypeAssertion", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		handlerCalled := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		evt := socketmode.Event{
			Type:    socketmode.EventTypeEventsAPI,
			Data:    "invalid data type", // fails the type assertion
			Request: &socketmode.Request{EnvelopeID: "test-envelope"},
		}

		bot.handleSlackSocketEvent(context.Background(), evt)

		assertFalse(t, handlerCalled, "Handler should not be called for invalid event data")
	})

	t.Run("NonEventsAPIEventType", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		handlerCalled := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		evt := socketmode.Event{Type: socketmode.EventTypeConnecting, Data: nil}

		bot.handleSlackSocketEvent(context.Background(), evt)

		assertFalse(t, handlerCalled, "Handler should not be called for non-EventsAPI event types")
	})
}
