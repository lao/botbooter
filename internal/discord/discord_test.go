package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func newTestAdapter(t *testing.T) *adapter {
	t.Helper()
	dg, err := discordgo.New("Bot test_token")
	asserts.NoError(t, err, "discordgo.New")
	return &adapter{session: dg}
}

func captureDeps(got **core.Message) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(ctx context.Context, m *core.Message) { *got = m },
	}
}

func TestNew(t *testing.T) {
	bot, err := New("test_token")

	asserts.NoError(t, err, "New should not fail for a well-formed token")
	asserts.NotNil(t, bot, "Bot should be initialized")
	asserts.Equal(t, bot.BotType, core.DiscordBotType, "Bot type should be Discord")
	asserts.NotNil(t, bot.DiscordSession, "Discord session should be initialized")
	asserts.True(t,
		bot.DiscordSession.Identify.Intents&discordgo.IntentMessageContent != 0,
		"message-content intent should be enabled")
}

func TestConnect(t *testing.T) {
	a := newTestAdapter(t)

	err := a.Connect(context.Background(), core.AdapterDeps{})

	// A fake token cannot open a real gateway connection.
	asserts.Error(t, err, "Connect with fake token should fail")
}

func TestDisconnect(t *testing.T) {
	a := newTestAdapter(t)

	asserts.NoError(t, a.Disconnect(), "Disconnect should not fail")
}

func TestDisconnect_NilSession(t *testing.T) {
	a := &adapter{}

	asserts.NoError(t, a.Disconnect(), "Disconnect with nil session should not fail")
}

func TestDisconnect_RemovesMessageHandler(t *testing.T) {
	a := newTestAdapter(t)
	removed := false
	a.removeHandler = func() { removed = true }

	asserts.NoError(t, a.Disconnect(), "Disconnect should not fail")
	asserts.True(t, removed, "Disconnect should remove the registered message handler")
	asserts.True(t, a.removeHandler == nil, "remove func should be cleared after disconnect")
}

func TestOnMessage(t *testing.T) {
	newSession := func(botUserID string) *discordgo.Session {
		s := &discordgo.Session{State: discordgo.NewState()}
		s.State.User = &discordgo.User{ID: botUserID}
		return s
	}

	t.Run("UserMessageDispatched", func(t *testing.T) {
		a := newTestAdapter(t)
		var got *core.Message

		a.onMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Author:    &discordgo.User{ID: "user-1"},
				ChannelID: "chan-1",
				Content:   "hello",
			},
		}, captureDeps(&got))

		asserts.NotNil(t, got, "handler should be called for user message")
		asserts.Equal(t, got.UserID, "user-1", "message user")
		asserts.Equal(t, got.ChannelID, "chan-1", "message channel")
	})

	t.Run("OwnMessageIgnored", func(t *testing.T) {
		a := newTestAdapter(t)
		var got *core.Message

		a.onMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Author: &discordgo.User{ID: "bot-id"}, Content: "hi"},
		}, captureDeps(&got))

		asserts.True(t, got == nil, "bot's own message should be ignored")
	})

	t.Run("NilAuthorIgnored", func(t *testing.T) {
		a := newTestAdapter(t)
		var got *core.Message

		a.onMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Content: "hi"},
		}, captureDeps(&got))

		asserts.True(t, got == nil, "message without author should be ignored")
	})

	t.Run("OtherBotIgnored", func(t *testing.T) {
		a := newTestAdapter(t)
		var got *core.Message

		a.onMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Author: &discordgo.User{ID: "other-bot", Bot: true}, Content: "hi"},
		}, captureDeps(&got))

		asserts.True(t, got == nil, "another bot's message should be ignored")
	})
}

func TestAttachmentsFromMessage(t *testing.T) {
	tests := []struct {
		name        string
		message     *discordgo.Message
		wantCount   int
		wantIsImage []bool
		wantURLs    []string
	}{
		{
			name: "single image attachment",
			message: &discordgo.Message{
				Attachments: []*discordgo.MessageAttachment{
					{URL: "https://example.com/image.png", Width: 100, Height: 100},
				},
			},
			wantCount:   1,
			wantIsImage: []bool{true},
			wantURLs:    []string{"https://example.com/image.png"},
		},
		{
			name: "multiple attachments with mixed types",
			message: &discordgo.Message{
				Attachments: []*discordgo.MessageAttachment{
					{URL: "https://example.com/image1.png", Width: 100, Height: 100},
					{URL: "https://example.com/image2.jpg", Width: 200, Height: 200},
					{URL: "https://example.com/document.pdf", Width: 0, Height: 0},
				},
			},
			wantCount:   3,
			wantIsImage: []bool{true, true, false},
			wantURLs:    []string{"https://example.com/image1.png", "https://example.com/image2.jpg", "https://example.com/document.pdf"},
		},
		{
			name:        "no attachments",
			message:     &discordgo.Message{Attachments: []*discordgo.MessageAttachment{}},
			wantCount:   0,
			wantIsImage: []bool{},
			wantURLs:    []string{},
		},
		{
			name: "non-image attachment",
			message: &discordgo.Message{
				Attachments: []*discordgo.MessageAttachment{
					{URL: "https://example.com/document.pdf", Width: 0, Height: 0},
				},
			},
			wantCount:   1,
			wantIsImage: []bool{false},
			wantURLs:    []string{"https://example.com/document.pdf"},
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
