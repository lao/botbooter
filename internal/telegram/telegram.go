// Package telegram is the Telegram adapter for botbooter: it connects via the
// Bot API getUpdates long-poll loop and implements core.Adapter.
package telegram

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter/internal/core"
)

type adapter struct {
	newClient func() (*bot.Bot, error)
	selfID    int64

	mu     sync.Mutex
	client *bot.Bot // the client running the active poll loop; guarded by mu
}

func (a *adapter) currentClient() *bot.Bot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

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
	a.newClient = func() (*bot.Bot, error) {
		// message_reaction is excluded from Telegram's default allowed_updates, so
		// it must be requested explicitly. Setting allowed_updates REPLACES the
		// default set, so "message" (the only other update this adapter handles) is
		// listed too — omitting it would silently stop message delivery.
		return bot.New(token,
			bot.WithDefaultHandler(a.onUpdate),
			bot.WithSkipGetMe(),
			bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "message_reaction"}),
		)
	}

	tg, err := a.newClient()
	if err != nil {
		return nil, err
	}
	a.client = tg
	a.selfID = tg.ID()

	return core.New(core.TelegramBotType, a), nil
}

// Connect starts the getUpdates long-poll loop in the background and returns immediately.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	ctx = withDeps(ctx, &deps)

	tg, err := a.newClient()
	if err != nil {
		return err
	}

	// Publish the poll-loop client so Client(b) and Send target the client that
	// actually receives updates — a RegisterHandler on it now fires.
	a.mu.Lock()
	a.client = tg
	a.mu.Unlock()

	go func() {
		tg.Start(ctx)
		deps.Done(ctx.Err())
	}()

	return nil
}

// Disconnect is a no-op; the poll loop stops when the run context is canceled.
func (a *adapter) Disconnect() error {
	return nil
}

func (a *adapter) Send(ctx context.Context, channelID, text string) error {
	_, err := a.currentClient().SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID(channelID),
		Text:   text,
	})
	return err
}

// SendThreaded implements [core.ThreadedSender]: it replies to the Telegram
// message identified by replyToID. Telegram message ids are ints; if replyToID is
// not one, it falls back to a plain (unthreaded) send rather than failing.
func (a *adapter) SendThreaded(ctx context.Context, channelID, replyToID, text string) error {
	replyID, err := strconv.Atoi(replyToID)
	if err != nil {
		return a.Send(ctx, channelID, text)
	}
	_, err = a.currentClient().SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID(channelID),
		Text:            text,
		ReplyParameters: &models.ReplyParameters{MessageID: replyID},
	})
	return err
}

func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	u, ok := RawUpdate(m)
	if !ok {
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

// RawReactionUpdate returns the raw Telegram message_reaction update carried on r,
// reporting whether r originated from a Telegram reaction.
func RawReactionUpdate(r *core.Reaction) (*models.MessageReactionUpdated, bool) {
	u, ok := r.Raw.(*models.Update)
	if !ok || u.MessageReaction == nil {
		return nil, false
	}
	return u.MessageReaction, true
}

// Client returns the go-telegram bot client backing b, or nil if b is not a Telegram bot.
func Client(b *core.Bot) *bot.Bot {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.currentClient()
	}
	return nil
}

// EnvSuppressURLWarning names the environment variable that silences the
// plaintext-token warning
const EnvSuppressURLWarning = "BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING"

// ResolveAttachmentURL implements [core.AttachmentResolver]: it turns att's
// Telegram file id (carried in ExtraData) into a getFile download link, or
// returns ("", nil) when att carries no recognized file id.
//
// The returned URL embeds the bot token in plaintext and is secret — do not log
// it. Each successful resolve warns to that effect unless [EnvSuppressURLWarning]
// is set.
func (a *adapter) ResolveAttachmentURL(ctx context.Context, att core.Attachment) (string, error) {
	id := fileIDOf(att.ExtraData)
	if id == "" {
		return "", nil
	}
	client := a.currentClient()
	f, err := client.GetFile(ctx, &bot.GetFileParams{FileID: id})
	if err != nil {
		return "", fmt.Errorf("resolve telegram file %s: %w", id, err)
	}
	warnTokenInURL()
	return client.FileDownloadLink(f), nil
}

func warnTokenInURL() {
	if os.Getenv(EnvSuppressURLWarning) != "" {
		return
	}
	log.Printf("botbooter: telegram download URL embeds the bot token in plaintext; "+
		"treat it as a secret and do not log it (set %s to silence)", EnvSuppressURLWarning)
}

// fileIDOf extracts a Telegram FileID from an attachment's ExtraData. Only the
// PhotoSize and Document kinds attachmentsFromMessage emits are recognized; a nil
// ExtraData is a no-op, but any other non-nil type is logged rather than silently
// dropped, so a newly-surfaced media kind cannot fail to resolve unnoticed.
func fileIDOf(extra any) string {
	switch v := extra.(type) {
	case models.PhotoSize:
		return v.FileID
	case *models.PhotoSize:
		if v == nil {
			return ""
		}
		return v.FileID
	case *models.Document:
		if v == nil {
			return ""
		}
		return v.FileID
	case nil:
		return ""
	default:
		log.Printf("botbooter: telegram attachment ExtraData has unexpected type %T; "+
			"no file id resolved (only photo and document attachments are supported)", extra)
		return ""
	}
}

func toMessage(u *models.Update) *core.Message {
	m := u.Message
	content := cmp.Or(m.Text, m.Caption)
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
	msg.MentionedUserIDs = telegramMentions(m)
	return msg
}

func telegramAuthorName(u *models.User) string {
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return u.Username
	}
	return u.FirstName
}

// telegramMentions collects user ids from text_mention entities, the only entity
// kind carrying a numeric user id; a plain @username mention references a name.
func telegramMentions(m *models.Message) []string {
	entities := m.Entities
	if m.Text == "" {
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
	if ctx.Err() != nil {
		return
	}

	deps := depsFrom(ctx)
	if deps == nil {
		return
	}

	if u.MessageReaction != nil {
		a.onReaction(ctx, u, deps)
		return
	}

	m := u.Message
	if m == nil || m.From == nil {
		return
	}
	if m.From.IsBot || m.From.ID == a.selfID {
		return
	}

	deps.Dispatch(ctx, toMessage(u))
}

// onReaction dispatches one Reaction per emoji newly added in a message_reaction
// update. Telegram delivers reactions as a diff of the full reaction set, so an
// emoji present in NewReaction but not OldReaction is a genuine add; removals
// (present in Old, gone from New) dispatch nothing (added-only scope). Only emoji
// reactions are surfaced — custom_emoji and paid reaction types are skipped.
// Anonymous channel reactions carry no user and are ignored. There is no
// self-reaction filter: the bot never adds reactions, so no self-reply loop.
func (a *adapter) onReaction(ctx context.Context, u *models.Update, deps *core.AdapterDeps) {
	mr := u.MessageReaction
	if mr.User == nil {
		return
	}
	old := emojiSet(mr.OldReaction)
	for _, rt := range mr.NewReaction {
		if rt.Type != models.ReactionTypeTypeEmoji || rt.ReactionTypeEmoji == nil {
			continue
		}
		emoji := rt.ReactionTypeEmoji.Emoji
		if old[emoji] {
			continue
		}
		deps.DispatchReaction(ctx, toReaction(u, emoji))
	}
}

// emojiSet collects the emoji strings from a reaction list, ignoring non-emoji
// reaction types. It returns nil for an empty list.
func emojiSet(rs []models.ReactionType) map[string]bool {
	if len(rs) == 0 {
		return nil
	}
	set := make(map[string]bool, len(rs))
	for _, rt := range rs {
		if rt.Type == models.ReactionTypeTypeEmoji && rt.ReactionTypeEmoji != nil {
			set[rt.ReactionTypeEmoji.Emoji] = true
		}
	}
	return set
}

// toReaction maps a Telegram message_reaction update (for one added emoji) onto a
// platform-agnostic Reaction. Unlike Slack/Discord, the reactor's name is inline
// on the update, so AuthorName is populated.
func toReaction(u *models.Update, emoji string) *core.Reaction {
	mr := u.MessageReaction
	return &core.Reaction{
		Emoji:      emoji,
		UserID:     strconv.FormatInt(mr.User.ID, 10),
		AuthorName: telegramAuthorName(mr.User),
		ChannelID:  strconv.FormatInt(mr.Chat.ID, 10),
		MessageID:  strconv.Itoa(mr.MessageID),
		Raw:        u,
	}
}

func chatID(s string) any {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id
	}
	return s
}

func attachmentsFromMessage(m *models.Message) []core.Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]core.Attachment, 0, 2)

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
