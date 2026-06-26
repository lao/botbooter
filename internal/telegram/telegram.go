// Package telegram is the Telegram adapter for botbooter: it connects via the
// Bot API getUpdates long-poll loop and implements core.Adapter.
package telegram

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter/internal/core"
)

type adapter struct {
	client *bot.Bot
	selfID int64
	deps   atomic.Pointer[core.AdapterDeps]
}

// New creates a Telegram bot from a BotFather token.
func New(token string) (*core.Bot, error) {
	a := &adapter{}

	tg, err := bot.New(token, bot.WithDefaultHandler(a.onUpdate), bot.WithSkipGetMe())
	if err != nil {
		return nil, err
	}
	a.client = tg
	a.selfID = tg.ID()

	b := core.New(core.TelegramBotType, a)
	b.TelegramBot = tg
	return b, nil
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	a.deps.Store(&deps)

	go func() {
		// Start blocks until ctx is canceled (getUpdates retries every non-context
		// error forever), so ctx.Err() is always non-nil here; report it like Slack
		// so Run can swallow the clean shutdown. Start has no other exit — no guard.
		a.client.Start(ctx)
		deps.Done(ctx.Err())
	}()

	return nil
}

// Disconnect is a no-op: the poll loop is stopped by canceling the run context.
func (a *adapter) Disconnect() error {
	return nil
}

// Send delivers text to the chat identified by channelID.
func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	_, err := a.client.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID(channelID),
		Text:   text,
	})
	return err
}

// Attachments returns the files attached to the message's Telegram update.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	if m.TelegramData == nil {
		return nil, nil
	}
	return attachmentsFromMessage(m.TelegramData.Message), nil
}

func (a *adapter) onUpdate(ctx context.Context, _ *bot.Bot, u *models.Update) {
	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	if m.From.IsBot || m.From.ID == a.selfID {
		return
	}

	deps := a.deps.Load()
	if deps == nil {
		return
	}

	content := m.Text
	if content == "" {
		content = m.Caption
	}

	deps.Dispatch(ctx, &core.Message{
		UserID:       strconv.FormatInt(m.From.ID, 10),
		ChannelID:    strconv.FormatInt(m.Chat.ID, 10),
		Content:      content,
		TelegramData: u,
	})
}

func chatID(s string) any {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id
	}
	return s
}

// attachmentsFromMessage converts a message's photo and document into attachments
// (nil for a nil message); other media kinds are not surfaced. URLs are left empty
// because Telegram delivers media by FileID, not URL; the FileID-bearing struct is
// carried in ExtraData for callers to resolve via GetFile.
func attachmentsFromMessage(m *models.Message) []core.Attachment {
	if m == nil {
		return nil
	}

	// Non-nil, possibly-empty slice, mirroring the Discord and Slack adapters.
	attachments := make([]core.Attachment, 0, 2)

	// Photo sizes are in ascending order; the last is the largest.
	if len(m.Photo) > 0 {
		largest := m.Photo[len(m.Photo)-1]
		attachments = append(attachments, core.Attachment{
			IsImage:   true,
			ExtraData: largest,
		})
	}

	if m.Document != nil {
		attachments = append(attachments, core.Attachment{
			IsImage:   strings.HasPrefix(m.Document.MimeType, "image/"),
			ExtraData: m.Document,
		})
	}

	return attachments
}
