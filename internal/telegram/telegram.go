// Package telegram is the Telegram adapter for botbooter. It connects via the
// Bot API getUpdates long-poll loop (a dial-out model like Slack Socket Mode and
// the Discord Gateway: outbound HTTPS only, no public endpoint or port) and
// implements core.Adapter.
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

// adapter is the Telegram implementation of core.Adapter.
//
// client and selfID are set once in New and never mutated, so they need no
// synchronization. deps is installed per-connection by Connect and read by the
// library's handler goroutines, which can outlive the Start call that spawned
// them (see Connect); it is therefore held in an atomic pointer.
type adapter struct {
	client *bot.Bot
	selfID int64
	deps   atomic.Pointer[core.AdapterDeps]
}

// New creates a Telegram bot from a BotFather token. It returns an error only if
// the token is empty; a malformed-but-non-empty token is accepted here and
// instead surfaces later as authentication errors that the getUpdates loop logs
// and retries (it never receives messages). The constructor performs no network
// I/O — like the Discord and Slack constructors, the live connection is owned by
// Connect's poll loop.
func New(token string) (*core.Bot, error) {
	a := &adapter{}

	// WithSkipGetMe keeps New offline. WithDefaultHandler is given a.onUpdate as a
	// method value bound to a; a.client and a.selfID are populated just below, before
	// any update can be delivered (that only starts in Connect).
	tg, err := bot.New(token, bot.WithDefaultHandler(a.onUpdate), bot.WithSkipGetMe())
	if err != nil {
		return nil, err
	}
	a.client = tg
	// ID parses the bot's numeric id from the "<id>:<secret>" token prefix with no
	// network call. It returns 0 for a non-integer prefix, which only degrades the
	// self-message filter to the IsBot check below — and Telegram never delivers a
	// bot its own messages over getUpdates anyway.
	a.selfID = tg.ID()

	b := core.New(core.TelegramBotType, a)
	b.TelegramBot = tg
	return b, nil
}

// Connect starts the getUpdates long-poll loop in the background. It returns
// immediately; the loop runs until ctx is canceled.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// &deps escapes to the heap (Go escape analysis), so the stored pointer stays
	// valid for the whole life of the poll loop — including the library's handler
	// goroutines, which can outlive the Start call below.
	a.deps.Store(&deps)

	go func() {
		// Start blocks running the poll loop. getUpdates retries every non-context
		// error forever, so Start returns only when ctx is canceled; ctx.Err() is
		// therefore always non-nil here. Report it (like Slack reports
		// socketmode.RunContext's error) so Run can recognize and swallow the clean
		// shutdown. Do not add a "Start exited unexpectedly" guard — Start has no
		// other exit.
		a.client.Start(ctx)
		deps.Done(ctx.Err())
	}()

	return nil
}

// Disconnect is a no-op: the poll loop is driven entirely by the run context, so
// canceling it (via Bot.Disconnect) is what stops the connection. There is no
// other resource to close. Safe to call when never connected.
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

// onUpdate converts an incoming Telegram update into a platform-agnostic Message
// and dispatches it. It ignores updates that carry no message, messages without
// a sender, and messages from any bot (including this one, also caught by the
// self-id check) to avoid reply loops — mirroring the Discord adapter.
//
// Every other human message is passed through, with Content taken from the text
// (or the caption for media). Non-text messages (a sticker, a photo with no
// caption) therefore reach dispatch with empty Content but a populated
// TelegramData; the core's command matching ignores anything that matches no
// pattern. This is the Discord pass-through model, not Slack's empty-message drop
// (which would swallow image-only messages).
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

// chatID converts a botbooter channel id into the value the Telegram API expects:
// a numeric chat id as an int64, or, when the string is not numeric, the string
// itself (so "@channelusername" targets work).
func chatID(s string) any {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id
	}
	return s
}

// attachmentsFromMessage converts a Telegram message's photo and document into
// platform-agnostic attachments, returning nil for a nil message. Other media
// kinds (audio, video, voice, sticker, …) are not surfaced here; callers that
// need them read the raw update on Message.TelegramData.
//
// The URL is left empty: Telegram delivers media by FileID, not by URL. Callers
// that need the bytes resolve the FileID through the raw client exposed on
// bot.TelegramBot (GetFile). The FileID-bearing struct is carried in ExtraData.
func attachmentsFromMessage(m *models.Message) []core.Attachment {
	if m == nil {
		return nil
	}

	// A non-nil, possibly-empty slice mirrors the Discord and Slack adapters.
	attachments := make([]core.Attachment, 0, 2)

	// Photo is a slice of sizes in ascending order; the last is the largest.
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
