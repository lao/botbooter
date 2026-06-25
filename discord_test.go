package botbooter

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestInitAsDiscordBot(t *testing.T) {
	bot, err := InitAsDiscordBot("test_token")

	assertNoError(t, err, "InitAsDiscordBot should not fail for a well-formed token")
	assertNotNil(t, bot, "Bot should be initialized")
	assertEqual(t, bot.BotType, DiscordBotType, "Bot type should be Discord")
	assertNotNil(t, bot.DiscordSession, "Discord session should be initialized")
	assertTrue(t,
		bot.DiscordSession.Identify.Intents&discordgo.IntentMessageContent != 0,
		"message-content intent should be enabled")
}

func TestConnectDiscord(t *testing.T) {
	bot := newDiscordBot(t)

	err := bot.connectDiscord(context.Background())

	// A fake token cannot open a real gateway connection.
	assertError(t, err, "Connect with fake token should fail")
}

func TestDisconnectDiscord(t *testing.T) {
	bot := newDiscordBot(t)

	assertNoError(t, bot.disconnectDiscord(), "Disconnect should not fail")
}

func TestGetAttachmentsFromDiscordMessage(t *testing.T) {
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
			attachments := getAttachmentsFromDiscordMessage(tt.message)

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

func TestGetAttachmentsFromDiscordMessage_Nil(t *testing.T) {
	assertEqual(t, len(getAttachmentsFromDiscordMessage(nil)), 0, "nil message yields no attachments")
}
