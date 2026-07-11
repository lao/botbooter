// Package telegram is the Telegram adapter for botbooter: it connects via the
// Bot API getUpdates long-poll loop and implements core.Adapter.
package telegram

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
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
	client *bot.Bot     // the client running the active poll loop; guarded by mu
	logger *slog.Logger // set from AdapterDeps at Connect; guarded by mu
}

// log returns the Bot's logger handed over at Connect, or slog.Default()
// before the first Connect.
func (a *adapter) log() *slog.Logger {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
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

// allowedUpdates is Telegram's default allowed_updates set (every update type
// except chat_member and message_reaction_count) plus message_reaction, which
// is excluded from the default and must be requested explicitly. The list is
// exhaustive on purpose: setting allowed_updates REPLACES the server default
// and PERSISTS server-side across getUpdates calls, so narrowing it to just
// what this adapter handles would silently stop delivery of callback_query,
// edited_message, inline_query, … to raw-client consumers who RegisterHandler
// on Client(bot). It is every models.AllowedUpdate* constant minus the two
// default exclusions.
var allowedUpdates = bot.AllowedUpdates{
	models.AllowedUpdateMessage,
	models.AllowedUpdateEditedMessage,
	models.AllowedUpdateChannelPost,
	models.AllowedUpdateEditedChannelPost,
	models.AllowedUpdateBusinessConnection,
	models.AllowedUpdateBusinessMessage,
	models.AllowedUpdateEditedBusinessMessage,
	models.AllowedUpdateDeletedBusinessMessages,
	models.AllowedUpdateMessageReaction,
	models.AllowedUpdateInlineQuery,
	models.AllowedUpdateChosenInlineResult,
	models.AllowedUpdateCallbackQuery,
	models.AllowedUpdateShippingQuery,
	models.AllowedUpdatePreCheckoutQuery,
	models.AllowedUpdatePurchasedPaidMedia,
	models.AllowedUpdatePoll,
	models.AllowedUpdatePollAnswer,
	models.AllowedUpdateMyChatMember,
	models.AllowedUpdateChatJoinRequest,
	models.AllowedUpdateChatBoost,
	models.AllowedUpdateRemovedChatBoost,
	models.AllowedUpdateManagedBot,
	models.AllowedUpdateGuestMessage,
}

// clientOptions is the option set every client (one per connection) is built
// with; the stub-server tests append to it, so it stays the single source of
// truth for production client configuration.
func (a *adapter) clientOptions() []bot.Option {
	return []bot.Option{
		bot.WithDefaultHandler(a.onUpdate),
		bot.WithSkipGetMe(),
		bot.WithAllowedUpdates(allowedUpdates),
	}
}

// New creates a Telegram bot from a BotFather token.
func New(token string) (*core.Bot, error) {
	a := &adapter{}
	a.newClient = func() (*bot.Bot, error) {
		return bot.New(token, a.clientOptions()...)
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
	a.logger = deps.Logger
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

// Send posts text to channelID. With a threading option it sets
// reply_to_message_id from the resolved anchor: opts.ThreadID when set, else the
// inbound message's ID (the received message — the intended reply-quote
// semantics, NOT ReplyToID). A ReplyTo-derived id that isn't a positive integer
// (never expected from Telegram's own ids) degrades to a plain send; an explicit
// WithThreadID that isn't returns an error, so a caller-supplied wrong-platform
// or malformed id fails loudly instead of silently dropping the anchor.
func (a *adapter) Send(ctx context.Context, channelID, text string, opts core.SendOptions) error {
	idStr := opts.ThreadID
	explicit := idStr != ""
	if !explicit && opts.ReplyTo != nil {
		idStr = opts.ReplyTo.ID
	}
	params := &bot.SendMessageParams{ChatID: chatID(channelID), Text: text}
	switch id, err := strconv.Atoi(idStr); {
	case err == nil && id > 0:
		params.ReplyParameters = &models.ReplyParameters{MessageID: id}
	case explicit:
		return fmt.Errorf("telegram: thread id %q is not a positive message id", opts.ThreadID)
	}
	_, err := a.currentClient().SendMessage(ctx, params)
	return err
}

// SendThreaded implements [core.ThreadedSender]: it replies to the Telegram
// message identified by replyToID. Telegram message ids are positive ints; if
// replyToID is not one, it falls back to a plain (unthreaded) send rather than
// failing. It delegates to Send's non-explicit ReplyTo path, which implements
// exactly those degrade-gracefully semantics.
func (a *adapter) SendThreaded(ctx context.Context, channelID, replyToID, text string) error {
	return a.Send(ctx, channelID, text, core.SendOptions{ReplyTo: &core.Message{ID: replyToID}})
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
	id := fileIDOf(a.log(), att.ExtraData)
	if id == "" {
		return "", nil
	}
	client := a.currentClient()
	f, err := client.GetFile(ctx, &bot.GetFileParams{FileID: id})
	if err != nil {
		return "", fmt.Errorf("resolve telegram file %s: %w", id, err)
	}
	warnTokenInURL(a.log())
	return client.FileDownloadLink(f), nil
}

func warnTokenInURL(logger *slog.Logger) {
	if os.Getenv(EnvSuppressURLWarning) != "" {
		return
	}
	logger.Warn("botbooter: telegram download URL embeds the bot token in plaintext; "+
		"treat it as a secret and do not log it", "suppress_env", EnvSuppressURLWarning)
}

// fileIDOf extracts a Telegram FileID from an attachment's ExtraData. Only the
// PhotoSize and Document kinds attachmentsFromMessage emits are recognized; a nil
// ExtraData is a no-op, but any other non-nil type is logged rather than silently
// dropped, so a newly-surfaced media kind cannot fail to resolve unnoticed.
func fileIDOf(logger *slog.Logger, extra any) string {
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
		logger.Warn("botbooter: telegram attachment ExtraData has unexpected type; "+
			"no file id resolved (only photo and document attachments are supported)",
			"type", fmt.Sprintf("%T", extra))
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
// Each id appears once, in first-mention order.
func telegramMentions(m *models.Message) []string {
	entities := m.Entities
	if m.Text == "" {
		entities = m.CaptionEntities
	}
	var ids []string
	seen := make(map[int64]bool)
	for _, e := range entities {
		if e.Type != models.MessageEntityTypeTextMention || e.User == nil || seen[e.User.ID] {
			continue
		}
		seen[e.User.ID] = true
		ids = append(ids, strconv.FormatInt(e.User.ID, 10))
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
// Anonymous channel reactions carry no user and are ignored. Reactions from
// bot users (which covers this bot itself) are dropped, mirroring the message
// path's bot filter, so another bot's setMessageReaction can't fire handlers.
func (a *adapter) onReaction(ctx context.Context, u *models.Update, deps *core.AdapterDeps) {
	mr := u.MessageReaction
	if mr.User == nil || mr.User.IsBot {
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
