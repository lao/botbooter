// Package telegram is the Telegram adapter for botbooter: it connects via the
// Bot API getUpdates long-poll loop and implements core.Adapter.
package telegram

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter/internal/core"
)

// adapter owns a client factory plus a long-lived client used for outbound Send
// and the exported escape hatch. It holds no per-connection poll state: Connect
// builds a fresh client per run (so a canceled run's buffered updates can't carry
// over), and dispatch callbacks ride on the context Connect hands that poll loop.
type adapter struct {
	newClient func() (*bot.Bot, error)
	client    *bot.Bot
	selfID    int64
}

// depsContextKey keys the per-connection deps on the poll loop context; a
// dedicated type avoids collisions with other context values (SA1029).
type depsContextKey struct{}

func withDeps(ctx context.Context, deps *core.AdapterDeps) context.Context {
	return context.WithValue(ctx, depsContextKey{}, deps)
}

func depsFrom(ctx context.Context) *core.AdapterDeps {
	deps, _ := ctx.Value(depsContextKey{}).(*core.AdapterDeps)
	return deps
}

// New creates a Telegram bot from a BotFather token.
func New(token string) (*core.Bot, error) {
	a := &adapter{}
	// WithSkipGetMe avoids a network round-trip; the bot id comes from the token
	// prefix instead. The factory lets Connect build a fresh client per run.
	a.newClient = func() (*bot.Bot, error) {
		return bot.New(token, bot.WithDefaultHandler(a.onUpdate), bot.WithSkipGetMe())
	}

	tg, err := a.newClient()
	if err != nil {
		return nil, err
	}
	a.client = tg
	a.selfID = tg.ID()

	b := core.New(core.TelegramBotType, a)
	b.TelegramBot = tg
	return b, nil
}

// Connect starts the getUpdates long-poll loop in the background; it returns immediately.
//
// Each connection builds its own *bot.Bot, so its update channel — and anything the
// library buffered in it — dies when the run is canceled. A shared client would let an
// update buffered at cancel time be drained, and dispatched, by the next connection.
// Dispatch callbacks likewise ride on the context handed to Start, so a straggling
// update reaches its own run's deps and is dropped once that run is canceled.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	ctx = withDeps(ctx, &deps)

	tg, err := a.newClient()
	if err != nil {
		return err
	}

	go func() {
		// Start blocks until ctx is canceled (getUpdates retries every non-context
		// error forever), so ctx.Err() is always non-nil here; report it like Slack
		// so Run can swallow the clean shutdown. Start has no other exit — no guard.
		tg.Start(ctx)
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
	u, _ := m.Raw.(*models.Update)
	if u == nil {
		return nil, nil
	}
	return attachmentsFromMessage(u.Message), nil
}

// RawUpdate returns the raw Telegram update carried on m, reporting whether m
// originated from Telegram.
func RawUpdate(m *core.Message) (*models.Update, bool) {
	u, ok := m.Raw.(*models.Update)
	return u, ok
}

// Client returns the go-telegram bot client backing b, or nil if b is not a
// Telegram bot.
func Client(b *core.Bot) *bot.Bot {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

// toMessage maps a Telegram update's message onto a platform-agnostic Message.
// onUpdate passes only updates with a non-nil Message; the From guard tolerates
// a missing sender. Content is the text, or the caption for media-only messages.
func toMessage(u *models.Update) *core.Message {
	m := u.Message
	content := m.Text
	if content == "" {
		content = m.Caption
	}
	msg := &core.Message{
		ID:        strconv.Itoa(m.ID),
		ChannelID: strconv.FormatInt(m.Chat.ID, 10),
		Content:   content,
		Timestamp: time.Unix(int64(m.Date), 0).UTC(),
		Raw:       u,
	}
	if m.From != nil {
		msg.UserID = strconv.FormatInt(m.From.ID, 10)
		msg.AuthorName = telegramAuthorName(m.From)
	}
	if m.ReplyToMessage != nil {
		msg.ReplyToID = strconv.Itoa(m.ReplyToMessage.ID)
	}
	msg.Mentions = telegramMentions(m)
	return msg
}

// telegramAuthorName prefers the @username, falling back to the first name.
func telegramAuthorName(u *models.User) string {
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return u.Username
	}
	return u.FirstName
}

// telegramMentions collects user ids from text_mention entities — the only
// entity kind that carries a numeric user id (a plain @username "mention"
// entity references a name, not an id, so it is skipped). It reads the message
// entities, falling back to the caption entities for media. Returns nil when
// there are none.
func telegramMentions(m *models.Message) []string {
	entities := m.Entities
	if len(entities) == 0 {
		entities = m.CaptionEntities
	}
	var ids []string
	for _, e := range entities {
		if e.Type == models.MessageEntityTypeTextMention && e.User != nil {
			ids = append(ids, strconv.FormatInt(e.User.ID, 10))
		}
	}
	return ids
}

func (a *adapter) onUpdate(ctx context.Context, _ *bot.Bot, u *models.Update) {
	// Drop updates once the run context is canceled: the library can invoke this
	// on a handler goroutine after Start has returned (see Connect).
	if ctx.Err() != nil {
		return
	}

	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	if m.From.IsBot || m.From.ID == a.selfID {
		return
	}

	// deps rides on this connection's context (see Connect).
	deps := depsFrom(ctx)
	if deps == nil {
		return
	}

	deps.Dispatch(ctx, toMessage(u))
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
