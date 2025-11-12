package botbooter

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestInitAsDiscordBot(t *testing.T) {
	// Arrange
	token := "test_token"

	// Act
	bot := InitAsDiscordBot(token)

	// Assert
	assertNotNil(t, bot, "Bot should be initialized")
	assertEqual(t, bot.BotType, DiscordBotType, "Bot type should be Discord")
	assertNotNil(t, bot.DiscordSession, "Discord session should be initialized")
}

func TestConnectDiscord(t *testing.T) {
	// Arrange
	bot := InitAsDiscordBot("test_token")

	// Act
	err := bot.connectDiscord()

	// Assert
	// We expect an error because we're using a fake token
	assertError(t, err, "Connect with fake token should fail")
}

func TestDisconnectDiscord(t *testing.T) {
	// Arrange
	bot := InitAsDiscordBot("test_token")

	// Act
	err := bot.disconnectDiscord()

	// Assert
	assertNoError(t, err, "Disconnect should not fail")
}

func TestGetAttachmentsFromDiscordMessage(t *testing.T) {
	// Arrange - Table-driven test cases
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
					{
						URL:    "https://example.com/image.png",
						Width:  100,
						Height: 100,
					},
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
					{
						URL:    "https://example.com/image1.png",
						Width:  100,
						Height: 100,
					},
					{
						URL:    "https://example.com/image2.jpg",
						Width:  200,
						Height: 200,
					},
					{
						URL:    "https://example.com/document.pdf",
						Width:  0,
						Height: 0,
					},
				},
			},
			wantCount:   3,
			wantIsImage: []bool{true, true, false},
			wantURLs:    []string{"https://example.com/image1.png", "https://example.com/image2.jpg", "https://example.com/document.pdf"},
		},
		{
			name: "no attachments",
			message: &discordgo.Message{
				Attachments: []*discordgo.MessageAttachment{},
			},
			wantCount:   0,
			wantIsImage: []bool{},
			wantURLs:    []string{},
		},
		{
			name: "non-image attachment",
			message: &discordgo.Message{
				Attachments: []*discordgo.MessageAttachment{
					{
						URL:    "https://example.com/document.pdf",
						Width:  0,
						Height: 0,
					},
				},
			},
			wantCount:   1,
			wantIsImage: []bool{false},
			wantURLs:    []string{"https://example.com/document.pdf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			attachments := getAttachmentsFromDiscordMessage(tt.message)

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
