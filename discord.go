package botbooter

import (
	"github.com/bwmarrin/discordgo"
)

func (b *Bot) connectDiscord() error {
	b.DiscordSession.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID {
			return
		}

		message := &Message{
			UserID:      m.Author.ID,
			ChannelID:   m.ChannelID,
			Content:     m.Content,
			DiscordData: m,
		}

		b.handleMessageWithCommand(message)
	})

	err := b.DiscordSession.Open()
	if err != nil {
		return err
	}

	return nil
}

func (b *Bot) disconnectDiscord() error {
	return b.DiscordSession.Close()
}

func InitAsDiscordBot(token string) *Bot {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil
	}

	return &Bot{
		BotType:               DiscordBotType,
		DiscordSession:        dg,
		Commands:              []Command{},
		UnknownCommandHandler: nil,
		SlackClient:           nil,
		SlackSocketClient:     nil,
	}
}

func getAttachmentsFromDiscordMessage(m *discordgo.Message) []Attachment {
	var attachments []Attachment

	for _, attachment := range m.Attachments {
		isImage := attachment.Width > 0 && attachment.Height > 0
		attachments = append(attachments, Attachment{
			IsImage:   isImage,
			URL:       attachment.URL,
			ExtraData: attachment,
		})
	}

	return attachments
}
