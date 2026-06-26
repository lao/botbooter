package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func newTestAdapter() *adapter {
	client := slackapi.New("xoxb-test", slackapi.OptionAppLevelToken("xapp-test"))
	return &adapter{client: client, socket: socketmode.New(client)}
}

func captureDeps(got **core.Message) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(ctx context.Context, m *core.Message) { *got = m },
	}
}

func TestNew(t *testing.T) {
	bot := New("app_token", "bot_token")

	asserts.NotNil(t, bot, "Bot should be initialized")
	asserts.Equal(t, bot.BotType, core.SlackBotType, "Bot type should be Slack")
	asserts.NotNil(t, bot.SlackClient, "Slack client should be initialized")
	asserts.NotNil(t, bot.SlackSocketClient, "Slack socket client should be initialized")
}

func TestIsBotMessage(t *testing.T) {
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
			asserts.Equal(t, isBotMessage(tt.event), tt.expectedIsBotMessage, "isBotMessage result")
		})
	}
}

func TestAttachmentsFromMessage(t *testing.T) {
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
			attachments := attachmentsFromMessage(tt.message)

			asserts.Equal(t, len(attachments), tt.wantCount, "Number of attachments")
			for i := 0; i < len(attachments) && i < len(tt.wantIsImage); i++ {
				asserts.Equal(t, attachments[i].IsImage, tt.wantIsImage[i], "IsImage property for attachment")
			}
			for i := 0; i < len(attachments) && i < len(tt.wantURLs); i++ {
				asserts.Equal(t, attachments[i].URL, tt.wantURLs[i], "URL for attachment")
			}
		})
	}
}

func TestAttachmentsFromMessage_Nil(t *testing.T) {
	asserts.Equal(t, len(attachmentsFromMessage(nil)), 0, "nil message yields no attachments")
}

func TestHandleEventsAPI(t *testing.T) {
	t.Run("BotMessage", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		a.handleEventsAPI(context.Background(), slackEvent(&slackevents.MessageEvent{
			BotID: "B01",
			Text:  "hello",
		}), captureDeps(&got))

		asserts.True(t, got == nil, "Handler should not be called for bot message")
	})

	t.Run("UserMessage", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		a.handleEventsAPI(context.Background(), slackEvent(&slackevents.MessageEvent{
			Text:    "hello",
			User:    "U123",
			Channel: "C456",
		}), captureDeps(&got))

		asserts.NotNil(t, got, "Handler should be called for user message")
		asserts.Equal(t, got.UserID, "U123", "message user")
		asserts.Equal(t, got.ChannelID, "C456", "message channel")
	})

	t.Run("NonMessageEvent", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		a.handleEventsAPI(context.Background(), slackEvent(&slackevents.AppMentionEvent{
			Text:    "mention",
			User:    "U123",
			Channel: "C456",
		}), captureDeps(&got))

		asserts.True(t, got == nil, "Handler should not be called for non-MessageEvent")
	})
}

func TestHandleSocketEvent(t *testing.T) {
	t.Run("ValidEventsAPIEvent", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		evt := socketmode.Event{
			Type:    socketmode.EventTypeEventsAPI,
			Data:    slackEvent(&slackevents.MessageEvent{Text: "hello", User: "U123", Channel: "C456"}),
			Request: &socketmode.Request{EnvelopeID: "test-envelope"},
		}

		a.handleSocketEvent(context.Background(), evt, captureDeps(&got))

		asserts.NotNil(t, got, "Handler should be called for valid message event")
	})

	t.Run("InvalidTypeAssertion", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		evt := socketmode.Event{
			Type:    socketmode.EventTypeEventsAPI,
			Data:    "invalid data type",
			Request: &socketmode.Request{EnvelopeID: "test-envelope"},
		}

		a.handleSocketEvent(context.Background(), evt, captureDeps(&got))

		asserts.True(t, got == nil, "Handler should not be called for invalid event data")
	})

	t.Run("NonEventsAPIEventType", func(t *testing.T) {
		a := newTestAdapter()
		var got *core.Message

		evt := socketmode.Event{Type: socketmode.EventTypeConnecting, Data: nil}

		a.handleSocketEvent(context.Background(), evt, captureDeps(&got))

		asserts.True(t, got == nil, "Handler should not be called for non-EventsAPI event types")
	})
}

func TestDisconnect(t *testing.T) {
	a := newTestAdapter()

	asserts.NoError(t, a.Disconnect(), "Disconnect Slack should not fail")
}

func slackEvent(data any) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: data},
	}
}

func TestToMessage(t *testing.T) {
	msg := &slackevents.MessageEvent{
		User:            "U123",
		Channel:         "C456",
		Text:            "hello",
		TimeStamp:       "1700000000.000100",
		ThreadTimeStamp: "1699999999.000000",
	}

	got := toMessage(msg)

	asserts.Equal(t, got.ID, "1700000000.000100", "ID is the ts")
	asserts.Equal(t, got.UserID, "U123", "UserID")
	asserts.Equal(t, got.ChannelID, "C456", "ChannelID")
	asserts.Equal(t, got.Content, "hello", "Content")
	asserts.Equal(t, got.AuthorName, "", "AuthorName empty (Slack gives id only)")
	asserts.Equal(t, got.ReplyToID, "1699999999.000000", "ReplyToID is the thread ts")
	asserts.Equal(t, got.Timestamp.Unix(), int64(1700000000), "Timestamp seconds")
	raw, ok := RawEvent(got)
	asserts.True(t, ok, "RawEvent recovers the event")
	asserts.True(t, raw == msg, "RawEvent returns the same pointer")
}

func TestParseSlackTimestamp(t *testing.T) {
	asserts.True(t, parseSlackTimestamp("").IsZero(), "empty ts is zero time")
	asserts.True(t, parseSlackTimestamp("not-a-ts").IsZero(), "non-numeric seconds is zero time")
	asserts.Equal(t, parseSlackTimestamp("1700000000.000100").Unix(), int64(1700000000), "seconds parsed")
	asserts.Equal(t, parseSlackTimestamp("1700000000.000100").UnixMicro(), int64(1700000000000100), "microsecond precision")
	asserts.Equal(t, parseSlackTimestamp("1700000000.1").UnixMicro(), int64(1700000000100000), "short fraction padded to microseconds")
	asserts.Equal(t, parseSlackTimestamp("1700000000.-1").UnixNano(), int64(1700000000)*1_000_000_000, "malformed fraction kept at second precision, not shifted")
}

func TestClientAccessors(t *testing.T) {
	bot := New("xapp-test", "xoxb-test")
	asserts.NotNil(t, Client(bot), "Client accessor returns the web client")
	asserts.NotNil(t, SocketClient(bot), "SocketClient accessor returns the socket client")
}
