package botbooter

import (
	"context"

	"github.com/bwmarrin/discordgo"
)

// InitAsDiscordBot creates a Discord bot from a bot token. It returns an error
// if the token cannot be used to construct a session.
//
// The message-content intent is enabled so handlers receive message text. That
// intent is privileged and must also be enabled for the application in the
// Discord developer portal, otherwise Discord delivers empty message content.
func InitAsDiscordBot(token string) (*Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent

	return &Bot{
		BotType:        DiscordBotType,
		DiscordSession: dg,
	}, nil
}

// connectDiscord opens the gateway and registers the message handler. It
// returns once the connection is established; the session runs in the
// background until ctx is canceled.
func (b *Bot) connectDiscord(ctx context.Context) error {
	remove := b.DiscordSession.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		b.onDiscordMessage(ctx, s, m)
	})

	if err := b.DiscordSession.Open(); err != nil {
		remove()
		return err
	}
	b.mu.Lock()
	b.removeDiscordHandler = remove
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = b.Disconnect()
	}()

	return nil
}

// onDiscordMessage converts an incoming Discord message into a platform-
// agnostic Message and dispatches it, ignoring messages without an author and
// the bot's own messages (to avoid reply loops).
func (b *Bot) onDiscordMessage(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil {
		return
	}
	// Ignore messages from any bot (including our own). This also closes the
	// gap where the self-ID check below cannot run because the session state
	// has not populated the bot user yet.
	if m.Author.Bot {
		return
	}
	if s.State != nil && s.State.User != nil && m.Author.ID == s.State.User.ID {
		return
	}

	b.dispatch(ctx, &Message{
		UserID:      m.Author.ID,
		ChannelID:   m.ChannelID,
		Content:     m.Content,
		DiscordData: m,
	})
}

func (b *Bot) disconnectDiscord() error {
	b.mu.Lock()
	remove := b.removeDiscordHandler
	b.removeDiscordHandler = nil
	b.mu.Unlock()
	if remove != nil {
		remove()
	}
	if b.DiscordSession == nil {
		return nil
	}
	return b.DiscordSession.Close()
}

func getAttachmentsFromDiscordMessage(m *discordgo.Message) []Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]Attachment, 0, len(m.Attachments))
	for _, attachment := range m.Attachments {
		attachments = append(attachments, Attachment{
			IsImage:   attachment.Width > 0 && attachment.Height > 0,
			URL:       attachment.URL,
			ExtraData: attachment,
		})
	}
	return attachments
}
