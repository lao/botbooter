// Package discord is the Discord adapter for botbooter. It connects via the
// Gateway and implements core.Adapter.
package discord

import (
	"context"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter/internal/core"
)

// adapter is the Discord implementation of core.Adapter.
type adapter struct {
	session *discordgo.Session

	mu            sync.Mutex
	removeHandler func()
}

// New creates a Discord bot from a bot token. It returns an error if the token
// cannot be used to construct a session.
//
// The message-content intent is enabled so handlers receive message text. That
// intent is privileged and must also be enabled for the application in the
// Discord developer portal, otherwise Discord delivers empty message content.
func New(token string) (*core.Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent

	bot := core.New(core.DiscordBotType, &adapter{session: dg})
	bot.DiscordSession = dg
	return bot, nil
}

// Connect opens the gateway and registers the message handler. It returns once
// the connection is established; the session runs in the background until ctx
// is canceled.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	remove := a.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		a.onMessage(ctx, s, m, deps)
	})

	if err := a.session.Open(); err != nil {
		remove()
		return err
	}
	a.mu.Lock()
	a.removeHandler = remove
	a.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = deps.Disconnect()
	}()

	return nil
}

// onMessage converts an incoming Discord message into a platform-agnostic
// Message and dispatches it, ignoring messages without an author and the bot's
// own messages (to avoid reply loops).
func (a *adapter) onMessage(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, deps core.AdapterDeps) {
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

	deps.Dispatch(ctx, &core.Message{
		UserID:      m.Author.ID,
		ChannelID:   m.ChannelID,
		Content:     m.Content,
		DiscordData: m,
	})
}

func (a *adapter) Disconnect() error {
	a.mu.Lock()
	remove := a.removeHandler
	a.removeHandler = nil
	a.mu.Unlock()
	if remove != nil {
		remove()
	}
	if a.session == nil {
		return nil
	}
	return a.session.Close()
}

func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	_, err := a.session.ChannelMessageSend(channelID, text, discordgo.WithContext(ctx))
	return err
}

func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	if m.DiscordData == nil {
		return nil, nil
	}
	return attachmentsFromMessage(m.DiscordData.Message), nil
}

func attachmentsFromMessage(m *discordgo.Message) []core.Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]core.Attachment, 0, len(m.Attachments))
	for _, attachment := range m.Attachments {
		attachments = append(attachments, core.Attachment{
			IsImage:   attachment.Width > 0 && attachment.Height > 0,
			URL:       attachment.URL,
			ExtraData: attachment,
		})
	}
	return attachments
}
