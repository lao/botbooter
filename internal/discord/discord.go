// Package discord is the Discord adapter for botbooter.
package discord

import (
	"context"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter/internal/core"
)

type adapter struct {
	session *discordgo.Session

	mu            sync.Mutex
	removeHandler func()
}

// New creates a Discord bot that connects via the Gateway.
func New(token string) (*core.Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent

	return core.New(core.DiscordBotType, &adapter{session: dg}), nil
}

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

	deps.Dispatch(ctx, toMessage(m))
}

// toMessage maps a Discord message-create event onto a platform-agnostic Message.
func toMessage(m *discordgo.MessageCreate) *core.Message {
	msg := &core.Message{
		ID:        m.ID,
		ChannelID: m.ChannelID,
		Content:   m.Content,
		Timestamp: m.Timestamp,
		Raw:       m,
	}
	if m.Author != nil {
		msg.UserID = m.Author.ID
		msg.AuthorName = m.Author.Username
	}
	if m.MessageReference != nil {
		msg.ReplyToID = m.MessageReference.MessageID
	}
	for _, u := range m.Mentions {
		if u != nil {
			msg.MentionedUserIDs = append(msg.MentionedUserIDs, u.ID)
		}
	}
	return msg
}

// Disconnect removes the message handler and closes the gateway session.
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

// Send posts text to channelID. With a threading option it posts an inline reply
// referencing the resolved message id (opts.ThreadID when set, else the inbound
// message's ID). With no id to reference it degrades to a plain channel send,
// since an empty MessageReference is an invalid Discord API request. The
// reference is built against channelID, so an anchor only works when sending to
// the message's own channel.
//
// Discord defaults message references to fail_if_not_exists=true and discordgo
// v0.27.1's MessageReference exposes no field to override it, so replying to a
// since-deleted message fails the whole send rather than degrading to a plain
// message. Lifting that needs a discordgo bump.
func (a *adapter) Send(ctx context.Context, channelID, text string, opts core.SendOptions) error {
	refID := opts.ThreadID
	if refID == "" && opts.ReplyTo != nil {
		refID = opts.ReplyTo.ID
	}
	if refID == "" {
		_, err := a.session.ChannelMessageSend(channelID, text, discordgo.WithContext(ctx))
		return err
	}
	ref := &discordgo.MessageReference{MessageID: refID, ChannelID: channelID}
	_, err := a.session.ChannelMessageSendReply(channelID, text, ref, discordgo.WithContext(ctx))
	return err
}

func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	mc, ok := RawEvent(m)
	if !ok {
		return nil, nil
	}
	return attachmentsFromMessage(mc.Message), nil
}

// RawEvent returns the raw Discord message-create event carried on m, reporting
// whether m originated from Discord.
func RawEvent(m *core.Message) (*discordgo.MessageCreate, bool) {
	e, ok := m.Raw.(*discordgo.MessageCreate)
	return e, ok
}

// Session returns the discordgo gateway session backing b, or nil if b is not a
// Discord bot.
func Session(b *core.Bot) *discordgo.Session {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.session
	}
	return nil
}

func attachmentsFromMessage(m *discordgo.Message) []core.Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]core.Attachment, 0, len(m.Attachments))
	for _, attachment := range m.Attachments {
		attachments = append(attachments, core.Attachment{
			IsImage:   isImageAttachment(attachment),
			URL:       attachment.URL,
			ExtraData: attachment,
		})
	}
	return attachments
}

// isImageAttachment reports whether a Discord attachment is an image. Discord
// sets Width/Height on videos and GIFs too, so prefer the MIME ContentType and
// fall back to dimensions only when Discord omits it.
func isImageAttachment(a *discordgo.MessageAttachment) bool {
	if a.ContentType != "" {
		return strings.HasPrefix(a.ContentType, "image/")
	}
	return a.Width > 0 && a.Height > 0
}
