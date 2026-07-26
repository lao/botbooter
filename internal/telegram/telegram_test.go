package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func captureDeps(got **core.Message) core.AdapterDeps {
	return core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { *got = m },
	}
}

// newCaptureAdapter bypasses New and the network so onUpdate can be tested
// directly; deps is connection-scoped, so onUpdate must get the returned ctx.
func newCaptureAdapter(selfID int64, got **core.Message) (*adapter, context.Context) {
	a := &adapter{selfID: selfID}
	deps := captureDeps(got)
	return a, withDeps(context.Background(), &deps)
}

// newReactionCapture builds an adapter and a deps-carrying ctx that collects
// every dispatched reaction, so onUpdate's reaction path can be tested directly.
func newReactionCapture(selfID int64) (*adapter, context.Context, *[]*core.Reaction) {
	got := &[]*core.Reaction{}
	deps := core.AdapterDeps{DispatchReaction: func(_ context.Context, r *core.Reaction) { *got = append(*got, r) }}
	return &adapter{selfID: selfID}, withDeps(context.Background(), &deps), got
}

func emojiReaction(e string) models.ReactionType {
	return models.ReactionType{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: e}}
}

// newStubAdapterRaw wires an adapter to an httptest Bot API server, mirroring New
// without real network I/O. It builds the client from the production
// clientOptions, so tests exercise the same option set New installs. The handler
// has full control of every Bot API method, including getMe.
func newStubAdapterRaw(t *testing.T, selfID int64, handler http.HandlerFunc) *adapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	a := &adapter{selfID: selfID}
	a.newClient = func() (*bot.Bot, error) {
		return bot.New("123:test-token", append(a.clientOptions(), bot.WithServerURL(srv.URL))...)
	}
	tg, err := a.newClient()
	asserts.NoError(t, err, "bot.New for stub server")
	a.client = tg
	return a
}

// newStubAdapter is newStubAdapterRaw with Connect's getMe probe pre-answered by
// a stub bot user, so a test's handler only has to cover the methods it cares
// about. (Connect probes getMe to fail fast on a bad token; the returned id is
// irrelevant — the adapter's self id comes from the token prefix / selfID.)
func newStubAdapter(t *testing.T, selfID int64, handler http.HandlerFunc) *adapter {
	t.Helper()
	return newStubAdapterRaw(t, selfID, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"stub"}}`))
			return
		}
		handler(w, r)
	})
}

func TestOnReaction(t *testing.T) {
	reactionUpdate := func(newR, oldR []models.ReactionType) *models.Update {
		return &models.Update{MessageReaction: &models.MessageReactionUpdated{
			Chat:        models.Chat{ID: 100},
			MessageID:   55,
			User:        &models.User{ID: 7, Username: "alice"},
			NewReaction: newR,
			OldReaction: oldR,
		}}
	}

	t.Run("AddedEmojiDispatched", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		a.onUpdate(ctx, nil, reactionUpdate([]models.ReactionType{emojiReaction("👍")}, nil))

		asserts.Equal(t, len(*got), 1, "one reaction dispatched")
		r := (*got)[0]
		asserts.Equal(t, r.Emoji, "👍", "emoji")
		asserts.Equal(t, r.UserID, "7", "reactor id")
		asserts.Equal(t, r.AuthorName, "alice", "reactor name is inline on the update")
		asserts.Equal(t, r.ChannelID, "100", "chat id")
		asserts.Equal(t, r.MessageID, "55", "reacted message id")
		ru, ok := RawReactionUpdate(r)
		asserts.True(t, ok, "RawReactionUpdate recovers the update")
		asserts.Equal(t, ru.MessageID, 55, "raw carries the reaction update")
	})

	t.Run("RemovalNotDispatched", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		a.onUpdate(ctx, nil, reactionUpdate(nil, []models.ReactionType{emojiReaction("👍")}))
		asserts.Equal(t, len(*got), 0, "a removal (present in Old, gone from New) dispatches nothing")
	})

	t.Run("AlreadyPresentNotRedispatched", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		a.onUpdate(ctx, nil, reactionUpdate(
			[]models.ReactionType{emojiReaction("👍"), emojiReaction("❤")},
			[]models.ReactionType{emojiReaction("👍")},
		))
		asserts.Equal(t, len(*got), 1, "only the newly-added emoji dispatches")
		asserts.Equal(t, (*got)[0].Emoji, "❤", "the newly-added emoji")
	})

	t.Run("NonEmojiTypeSkipped", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		// A zero-value ReactionType has a non-emoji Type; a nil ReactionTypeEmoji
		// is likewise skipped.
		a.onUpdate(ctx, nil, reactionUpdate([]models.ReactionType{{}}, nil))
		asserts.Equal(t, len(*got), 0, "non-emoji reaction types are skipped")
	})

	t.Run("BotReactorIgnored", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		u := &models.Update{MessageReaction: &models.MessageReactionUpdated{
			Chat: models.Chat{ID: 100}, MessageID: 55,
			User:        &models.User{ID: 8, IsBot: true},
			NewReaction: []models.ReactionType{emojiReaction("👍")},
		}}
		a.onUpdate(ctx, nil, u)
		asserts.Equal(t, len(*got), 0, "another bot's reaction dispatches nothing, mirroring the message path")
	})

	t.Run("AnonymousNoUser", func(t *testing.T) {
		a, ctx, got := newReactionCapture(999)
		u := &models.Update{MessageReaction: &models.MessageReactionUpdated{
			Chat: models.Chat{ID: 100}, MessageID: 55,
			NewReaction: []models.ReactionType{emojiReaction("👍")},
		}}
		a.onUpdate(ctx, nil, u)
		asserts.Equal(t, len(*got), 0, "anonymous reaction (no user) dispatches nothing")
	})
}

func TestRawReactionUpdate_NonTelegram(t *testing.T) {
	t.Run("foreign raw", func(t *testing.T) {
		u, ok := RawReactionUpdate(&core.Reaction{Raw: "not-a-telegram-update"})
		asserts.False(t, ok, "a non-Telegram reaction is not recovered")
		asserts.True(t, u == nil, "no update returned for a foreign reaction")
	})

	t.Run("telegram update without a reaction", func(t *testing.T) {
		// A Telegram *models.Update whose MessageReaction is nil is not a reaction.
		u, ok := RawReactionUpdate(&core.Reaction{Raw: &models.Update{}})
		asserts.False(t, ok, "an update carrying no MessageReaction is not a reaction")
		asserts.True(t, u == nil, "no update returned when MessageReaction is nil")
	})
}

func TestSendThreaded(t *testing.T) {
	var body string
	a := newStubAdapter(t, 999, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":100,"type":"private"}}}`))
	})

	err := a.SendThreaded(context.Background(), "100", "55", "hi")

	asserts.NoError(t, err, "SendThreaded should succeed")
	asserts.True(t, strings.Contains(body, "reply_parameters"), "reply_parameters sent: "+body)
	asserts.True(t, strings.Contains(body, `"message_id":55`), "reply targets message 55: "+body)
}

// A non-numeric replyToID cannot be a Telegram message id, so SendThreaded falls
// back to a plain send with no reply_parameters rather than failing.
func TestSendThreaded_NonNumericFallsBack(t *testing.T) {
	var gotText, gotReply string
	a := newStubAdapter(t, 999, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			_ = r.ParseMultipartForm(1 << 20)
			gotText = r.FormValue("text")
			gotReply = r.FormValue("reply_parameters")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":100,"type":"private"}}}`))
	})

	err := a.SendThreaded(context.Background(), "100", "not-a-number", "hi")

	asserts.NoError(t, err, "SendThreaded should fall back to a plain send")
	asserts.Equal(t, gotText, "hi", "text is still sent on the fallback path")
	asserts.Equal(t, gotReply, "", "no reply_parameters on the fallback path")
}

// A non-positive replyToID (Telegram message ids start at 1) would make the API
// reject the send with Bad Request, so SendThreaded degrades to a plain send
// with no reply_parameters instead — same as the non-numeric case.
func TestSendThreaded_NonPositiveFallsBack(t *testing.T) {
	var gotText, gotReply string
	a := newStubAdapter(t, 999, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			_ = r.ParseMultipartForm(1 << 20)
			gotText = r.FormValue("text")
			gotReply = r.FormValue("reply_parameters")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":100,"type":"private"}}}`))
	})

	err := a.SendThreaded(context.Background(), "100", "0", "hi")

	asserts.NoError(t, err, "SendThreaded should fall back to a plain send")
	asserts.Equal(t, gotText, "hi", "text is still sent on the fallback path")
	asserts.Equal(t, gotReply, "", "no reply_parameters for a non-positive reply id")
}

func TestNew(t *testing.T) {
	b, err := New("123456:test-token")

	asserts.NoError(t, err, "New should not fail for a non-empty token")
	asserts.NotNil(t, b, "Bot should be initialized")
	asserts.Equal(t, b.BotType, core.TelegramBotType, "Bot type should be Telegram")
	asserts.Equal(t, b.BotType.String(), "telegram", "Bot type string")
	asserts.NotNil(t, Client(b), "Telegram client escape hatch should be set")
	asserts.Equal(t, Client(b).ID(), int64(123456), "self id parsed from token prefix")
}

func TestNew_EmptyToken(t *testing.T) {
	_, err := New("")
	asserts.Error(t, err, "New should fail for an empty token")

	_, err = New("   ")
	asserts.Error(t, err, "New should fail for a blank token")
}

func TestOnUpdate(t *testing.T) {
	const selfID = int64(42)

	userMessage := func(text, caption string) *models.Update {
		return &models.Update{Message: &models.Message{
			From: &models.User{ID: 7},
			Chat: models.Chat{ID: 100},
			Text: text, Caption: caption,
		}}
	}

	t.Run("UserMessageDispatched", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		u := userMessage("hello", "")
		a.onUpdate(ctx, nil, u)

		asserts.NotNil(t, got, "user message should be dispatched")
		asserts.Equal(t, got.UserID, "7", "message user id")
		asserts.Equal(t, got.ChannelID, "100", "message chat id")
		asserts.Equal(t, got.Content, "hello", "message content")
		raw, ok := RawUpdate(got)
		asserts.True(t, ok, "raw update should be recoverable")
		asserts.True(t, raw == u, "raw update carried on Raw")
	})

	t.Run("CaptionUsedWhenTextEmpty", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, userMessage("", "a caption"))

		asserts.NotNil(t, got, "captioned message should be dispatched")
		asserts.Equal(t, got.Content, "a caption", "caption used as content when text is empty")
	})

	t.Run("PhotoOnlyDispatchedWithEmptyContent", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		u := &models.Update{Message: &models.Message{
			From:  &models.User{ID: 7},
			Chat:  models.Chat{ID: 100},
			Photo: []models.PhotoSize{{FileID: "f1"}},
		}}
		a.onUpdate(ctx, nil, u)

		asserts.NotNil(t, got, "photo-only message should still be dispatched (pass-through)")
		asserts.Equal(t, got.Content, "", "photo-only message has empty content")
		raw, ok := RawUpdate(got)
		asserts.True(t, ok, "raw update should be recoverable")
		asserts.True(t, raw == u, "raw update carried so handlers can read the photo")
	})

	t.Run("NoMessageIgnored", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, &models.Update{})

		asserts.True(t, got == nil, "update without a message should be ignored")
	})

	t.Run("NoSenderIgnored", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, &models.Update{Message: &models.Message{Text: "hi"}})

		asserts.True(t, got == nil, "message without a sender should be ignored")
	})

	t.Run("OtherBotIgnored", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, &models.Update{Message: &models.Message{
			From: &models.User{ID: 9, IsBot: true}, Chat: models.Chat{ID: 1}, Text: "hi",
		}})

		asserts.True(t, got == nil, "another bot's message should be ignored")
	})

	t.Run("OwnMessageIgnored", func(t *testing.T) {
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, &models.Update{Message: &models.Message{
			From: &models.User{ID: selfID}, Chat: models.Chat{ID: 1}, Text: "hi",
		}})

		asserts.True(t, got == nil, "the bot's own message should be ignored")
	})

	t.Run("NoDepsIgnored", func(t *testing.T) {
		// An update with no deps on its context (delivered outside Connect) must be
		// dropped, not panic — exercises the depsFrom == nil guard.
		(&adapter{selfID: selfID}).onUpdate(context.Background(), nil, userMessage("hi", ""))
	})

	t.Run("EmptyServiceMessageSkipped", func(t *testing.T) {
		// A sticker-only message (and, likewise, member-join/location/pinned service
		// messages) has no text, no caption, and no attachment the adapter surfaces,
		// so it must not be dispatched as an empty-Content command. Mirrors Slack and
		// whatsmeow.
		var got *core.Message
		a, ctx := newCaptureAdapter(selfID, &got)

		a.onUpdate(ctx, nil, &models.Update{Message: &models.Message{
			From:    &models.User{ID: 7},
			Chat:    models.Chat{ID: 100},
			Sticker: &models.Sticker{FileID: "s1"},
		}})

		asserts.True(t, got == nil, "an empty service message is not dispatched to the command table")
	})
}

// TestOnUpdate_DropsAfterShutdown checks an update on a canceled run context is dropped.
func TestOnUpdate_DropsAfterShutdown(t *testing.T) {
	var got *core.Message
	deps := captureDeps(&got)
	ctx, cancel := context.WithCancel(withDeps(context.Background(), &deps))
	cancel()

	(&adapter{selfID: 42}).onUpdate(ctx, nil, &models.Update{Message: &models.Message{
		From: &models.User{ID: 7}, Chat: models.Chat{ID: 100}, Text: "late",
	}})

	asserts.True(t, got == nil, "update on a canceled run context should be dropped")
}

// TestOnUpdate_DispatchIsConnectionScoped checks onUpdate routes through the deps
// on its own context, so each connection reaches only its own deps.
func TestOnUpdate_DispatchIsConnectionScoped(t *testing.T) {
	var got1, got2 *core.Message
	deps1, deps2 := captureDeps(&got1), captureDeps(&got2)
	ctx1 := withDeps(context.Background(), &deps1)
	ctx2 := withDeps(context.Background(), &deps2)

	a := &adapter{selfID: 42}
	u := &models.Update{Message: &models.Message{
		From: &models.User{ID: 7}, Chat: models.Chat{ID: 100}, Text: "hi",
	}}

	a.onUpdate(ctx1, nil, u)
	asserts.True(t, got1 != nil, "update on connection 1's context reaches deps1")
	asserts.True(t, got2 == nil, "and not deps2")

	got1 = nil
	a.onUpdate(ctx2, nil, u)
	asserts.True(t, got2 != nil, "update on connection 2's context reaches deps2")
	asserts.True(t, got1 == nil, "and not deps1")
}

func TestToMessage(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		ID:             42,
		From:           &models.User{ID: 7, Username: "bob"},
		Chat:           models.Chat{ID: 100},
		Text:           "hey",
		Date:           1700000000,
		ReplyToMessage: &models.Message{ID: 41},
	}}

	got := toMessage(u)

	asserts.Equal(t, got.ID, "42", "ID")
	asserts.Equal(t, got.UserID, "7", "UserID")
	asserts.Equal(t, got.AuthorName, "bob", "AuthorName from username")
	asserts.Equal(t, got.ChannelID, "100", "ChannelID from chat id")
	asserts.Equal(t, got.Content, "hey", "Content")
	asserts.Equal(t, got.ReplyToID, "41", "ReplyToID")
	asserts.Equal(t, got.Timestamp.Unix(), int64(1700000000), "Timestamp")
	raw, ok := RawUpdate(got)
	asserts.True(t, ok, "RawUpdate recovers the update")
	asserts.True(t, raw == u, "RawUpdate returns the same pointer")
}

func TestToMessageCaptionAndFirstName(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		ID:      1,
		From:    &models.User{ID: 7, FirstName: "Bob"},
		Chat:    models.Chat{ID: 100},
		Caption: "a photo",
		Date:    1700000000,
	}}

	got := toMessage(u)

	asserts.Equal(t, got.Content, "a photo", "Content falls back to caption")
	asserts.Equal(t, got.AuthorName, "Bob", "AuthorName falls back to first name")
	asserts.Equal(t, got.ReplyToID, "", "no reply")
}

func TestToMessageNilSender(t *testing.T) {
	// toMessage must not panic on a sender-less message; UserID/AuthorName stay empty.
	got := toMessage(&models.Update{Message: &models.Message{
		ID: 1, Chat: models.Chat{ID: 100}, Text: "hi", Date: 1700000000,
	}})

	asserts.Equal(t, got.UserID, "", "no sender yields empty UserID")
	asserts.Equal(t, got.AuthorName, "", "no sender yields empty AuthorName")
	asserts.Equal(t, got.ChannelID, "100", "chat id still mapped")
}

func TestTelegramMentions(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		From: &models.User{ID: 1},
		Chat: models.Chat{ID: 1},
		Text: "hi Bob",
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeTextMention, User: &models.User{ID: 99}},
			{Type: models.MessageEntityTypeMention},                                 // @username — no id, skipped
			{Type: models.MessageEntityTypeTextMention, User: &models.User{ID: 99}}, // repeat, deduped
		},
	}}
	got := toMessage(u)
	asserts.Equal(t, strings.Join(got.MentionedUserIDs, ","), "99", "text_mention id only, deduped")
}

func TestTelegramMentionsFromCaption(t *testing.T) {
	// A media message has empty Text, so mentions come from CaptionEntities even
	// if Entities is non-empty — mirroring how Content falls back to Caption.
	u := &models.Update{Message: &models.Message{
		From:            &models.User{ID: 1},
		Chat:            models.Chat{ID: 1},
		Caption:         "see Bob",
		Entities:        []models.MessageEntity{{Type: models.MessageEntityTypeMention}},
		CaptionEntities: []models.MessageEntity{{Type: models.MessageEntityTypeTextMention, User: &models.User{ID: 77}}},
	}}
	got := toMessage(u)
	asserts.Equal(t, strings.Join(got.MentionedUserIDs, ","), "77", "caption text_mention used when Text is empty")
}

func TestChatID(t *testing.T) {
	t.Run("numeric", func(t *testing.T) {
		got, ok := chatID("12345").(int64)
		asserts.True(t, ok, "numeric channel id should become an int64")
		asserts.Equal(t, got, int64(12345), "parsed chat id")
	})

	t.Run("negativeSupergroup", func(t *testing.T) {
		got, ok := chatID("-1001234567890").(int64)
		asserts.True(t, ok, "negative channel id should become an int64")
		asserts.Equal(t, got, int64(-1001234567890), "parsed negative chat id")
	})

	t.Run("username", func(t *testing.T) {
		got, ok := chatID("@channelname").(string)
		asserts.True(t, ok, "non-numeric channel id should pass through as a string")
		asserts.Equal(t, got, "@channelname", "username passed through unchanged")
	})
}

func TestAttachmentsFromMessage(t *testing.T) {
	t.Run("nil message", func(t *testing.T) {
		asserts.Equal(t, len(attachmentsFromMessage(nil)), 0, "nil message yields no attachments")
	})

	t.Run("photo uses largest size and has no URL", func(t *testing.T) {
		att := attachmentsFromMessage(&models.Message{Photo: []models.PhotoSize{
			{FileID: "small", Width: 90, Height: 90},
			{FileID: "large", Width: 800, Height: 800},
		}})

		asserts.Equal(t, len(att), 1, "one attachment for a photo")
		asserts.True(t, att[0].IsImage, "photo is an image")
		asserts.Equal(t, att[0].URL, "", "Telegram delivers a FileID, not a URL")
		size, ok := att[0].ExtraData.(models.PhotoSize)
		asserts.True(t, ok, "ExtraData carries the PhotoSize")
		asserts.Equal(t, size.FileID, "large", "largest photo size is selected")
	})

	t.Run("image document", func(t *testing.T) {
		att := attachmentsFromMessage(&models.Message{Document: &models.Document{
			FileID: "d1", MimeType: "image/png",
		}})

		asserts.Equal(t, len(att), 1, "one attachment for a document")
		asserts.True(t, att[0].IsImage, "image/* document is an image")
		asserts.Equal(t, att[0].URL, "", "document has no URL")
	})

	t.Run("non-image document", func(t *testing.T) {
		att := attachmentsFromMessage(&models.Message{Document: &models.Document{
			FileID: "d2", MimeType: "application/pdf",
		}})

		asserts.Equal(t, len(att), 1, "one attachment for a document")
		asserts.False(t, att[0].IsImage, "pdf document is not an image")
	})

	t.Run("photo and document together", func(t *testing.T) {
		att := attachmentsFromMessage(&models.Message{
			Photo:    []models.PhotoSize{{FileID: "p"}},
			Document: &models.Document{FileID: "d", MimeType: "image/jpeg"},
		})

		asserts.Equal(t, len(att), 2, "photo and document each yield an attachment")
	})
}

// The adapter must satisfy the optional core capability so the unified
// (*core.Bot).ResolveAttachmentURL routes to it (guards the pointer-receiver
// method-set trap, not just a green compile elsewhere).
var _ core.AttachmentResolver = (*adapter)(nil)

func TestFileIDOf(t *testing.T) {
	asserts.Equal(t, fileIDOf(slog.Default(), models.PhotoSize{FileID: "p"}), "p", "photo size value yields its FileID")
	asserts.Equal(t, fileIDOf(slog.Default(), &models.PhotoSize{FileID: "pp"}), "pp", "photo size pointer also yields its FileID")
	asserts.Equal(t, fileIDOf(slog.Default(), &models.Document{FileID: "d"}), "d", "document pointer yields its FileID")
	asserts.Equal(t, fileIDOf(slog.Default(), (*models.Document)(nil)), "", "nil document pointer is guarded")
	asserts.Equal(t, fileIDOf(slog.Default(), nil), "", "nil ExtraData yields no FileID")
}

func TestFileIDOf_UnhandledTypeWarns(t *testing.T) {
	logs := captureLog(t)

	got := fileIDOf(slog.Default(), models.Video{FileID: "v"})

	asserts.Equal(t, got, "", "an unhandled media type yields no file id")
	asserts.True(t, strings.Contains(logs.String(), "unexpected type"),
		"an unhandled ExtraData type is surfaced, not silently dropped")
}

func TestBot_ResolveAttachmentURL_RoutesToTelegram(t *testing.T) {
	t.Setenv(EnvSuppressURLWarning, "1")
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_path":"photos/file_1.jpg"}}`))
	})
	b := core.New(core.TelegramBotType, a)

	url, err := b.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: models.PhotoSize{FileID: "f"}})

	asserts.NoError(t, err, "unified resolve routes through the Telegram resolver")
	want := a.client.FileDownloadLink(&models.File{FilePath: "photos/file_1.jpg"})
	asserts.Equal(t, url, want, "the unified method delegates to the resolver, not the att.URL passthrough")
}

func TestResolveAttachmentURL(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		if got := r.FormValue("file_id"); got != "large" {
			http.Error(w, "unexpected file_id: "+got, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"large","file_path":"photos/file_1.jpg"}}`))
	})
	url, err := a.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: models.PhotoSize{FileID: "large"}})

	asserts.NoError(t, err, "getFile succeeds against the stub server")
	want := a.client.FileDownloadLink(&models.File{FilePath: "photos/file_1.jpg"})
	asserts.Equal(t, url, want, "URL is the download link for the resolved file_path")
}

func TestResolveAttachmentURL_GetFileError(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`))
	})
	url, err := a.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: models.PhotoSize{FileID: "huge"}})

	asserts.Error(t, err, "a getFile failure is surfaced")
	asserts.Equal(t, url, "", "no URL is returned on error")
}

func TestResolveAttachmentURL_NoFileID(t *testing.T) {
	a := newStubAdapter(t, 0, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("getFile must not be called when the attachment has no file id")
	})
	url, err := a.ResolveAttachmentURL(context.Background(), core.Attachment{})

	asserts.NoError(t, err, "an attachment without a file id is not an error")
	asserts.Equal(t, url, "", "no file id yields an empty URL")
}

// captureLog redirects the default logger to a buffer for the test, restoring it
// afterward, so a test can assert on what ResolveAttachmentURL logged.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	})
	return &buf
}

func resolvePhoto(t *testing.T) error {
	t.Helper()
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_path":"photos/file_1.jpg"}}`))
	})
	_, err := a.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: models.PhotoSize{FileID: "f"}})
	return err
}

func TestResolveAttachmentURL_WarnsTokenInURL(t *testing.T) {
	t.Setenv(EnvSuppressURLWarning, "") // neutralize any ambient opt-out
	logs := captureLog(t)

	asserts.NoError(t, resolvePhoto(t), "getFile succeeds against the stub server")
	asserts.True(t, strings.Contains(logs.String(), "embeds the bot token"),
		"a successful resolve warns that the URL carries the token")
}

// TestResolveAttachmentURL_RoutesWarningToInjectedLogger proves the logger
// stored at Connect (here set directly) carries the resolve warning, rather
// than always falling back to slog.Default.
func TestResolveAttachmentURL_RoutesWarningToInjectedLogger(t *testing.T) {
	t.Setenv(EnvSuppressURLWarning, "")
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_path":"photos/file_1.jpg"}}`))
	})
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))

	_, err := a.ResolveAttachmentURL(context.Background(),
		core.Attachment{ExtraData: models.PhotoSize{FileID: "f"}})

	asserts.NoError(t, err, "getFile succeeds against the stub server")
	asserts.True(t, strings.Contains(buf.String(), "embeds the bot token"),
		"the injected logger receives the token-in-URL warning")
}

func TestResolveAttachmentURL_SuppressesWarning(t *testing.T) {
	t.Setenv(EnvSuppressURLWarning, "1")
	logs := captureLog(t)

	asserts.NoError(t, resolvePhoto(t), "getFile succeeds against the stub server")
	asserts.Equal(t, logs.String(), "", "the warning is silenced when the env var is set")
}

func TestSend(t *testing.T) {
	// The Bot API client posts parameters as multipart/form-data.
	var gotChatID, gotText string
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			http.Error(w, "unexpected method", http.StatusNotFound)
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		gotChatID = r.FormValue("chat_id")
		gotText = r.FormValue("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":555,"type":"private"}}}`))
	})

	err := a.Send(context.Background(), "555", "hi there", core.SendOptions{})

	asserts.NoError(t, err, "Send should succeed against a 200 OK API reply")
	asserts.Equal(t, gotChatID, "555", "numeric channel id sent as chat_id")
	asserts.Equal(t, gotText, "hi there", "text forwarded to the API")
}

func TestSend_Threading(t *testing.T) {
	// The Bot API encodes reply_parameters as a JSON form field.
	replyCapture := func(got *string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseMultipartForm(1 << 20)
			*got = r.FormValue("reply_parameters")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":555,"type":"private"}}}`))
		}
	}

	t.Run("InReplyToRepliesToMessageID", func(t *testing.T) {
		var gotReply string
		a := newStubAdapter(t, 0, replyCapture(&gotReply))

		err := a.Send(context.Background(), "555", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "555", ID: "42"}})

		asserts.NoError(t, err, "Send")
		asserts.True(t, strings.Contains(gotReply, `"message_id":42`), "reply_parameters carries the message id")
	})

	t.Run("WithThreadIDRepliesToRawID", func(t *testing.T) {
		var gotReply string
		a := newStubAdapter(t, 0, replyCapture(&gotReply))

		err := a.Send(context.Background(), "555", "hi", core.SendOptions{ThreadID: "42"})

		asserts.NoError(t, err, "Send")
		asserts.True(t, strings.Contains(gotReply, `"message_id":42`), "reply_parameters carries the raw ThreadID")
	})

	t.Run("NonNumericReplyToDegradesToPlainSend", func(t *testing.T) {
		var gotReply string
		a := newStubAdapter(t, 0, replyCapture(&gotReply))

		err := a.Send(context.Background(), "555", "hi",
			core.SendOptions{ReplyTo: &core.Message{ChannelID: "555", ID: "not-a-number"}})

		asserts.NoError(t, err, "a non-numeric derived id degrades to a plain send")
		asserts.Equal(t, gotReply, "", "no reply_parameters when the derived id is non-numeric")
	})

	t.Run("ThreadIDWinsOverReplyTo", func(t *testing.T) {
		var gotReply string
		a := newStubAdapter(t, 0, replyCapture(&gotReply))

		err := a.Send(context.Background(), "555", "hi",
			core.SendOptions{ThreadID: "43", ReplyTo: &core.Message{ChannelID: "555", ID: "42"}})

		asserts.NoError(t, err, "Send")
		asserts.True(t, strings.Contains(gotReply, `"message_id":43`), "explicit ThreadID wins over ReplyTo.ID")
	})

	t.Run("ExplicitNonPositiveThreadIDErrors", func(t *testing.T) {
		a := newStubAdapter(t, 0, replyCapture(new(string)))

		for _, id := range []string{"not-a-number", "0", "-1"} {
			err := a.Send(context.Background(), "555", "hi", core.SendOptions{ThreadID: id})
			asserts.Error(t, err, "an explicit ThreadID that is not a positive message id must fail loudly")
		}
	})
}

func TestSend_SurfacesError(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
	})

	err := a.Send(context.Background(), "555", "hi", core.SendOptions{})

	asserts.ErrorIs(t, err, bot.ErrorForbidden, "Send should surface the Telegram Forbidden error")
}

func TestDisconnect(t *testing.T) {
	asserts.NoError(t, (&adapter{}).Disconnect(), "Disconnect is a no-op and safe before Connect")
}

func TestConnect_StartsAndStops(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(err error) { done <- err },
	}

	ctx, cancel := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")
	cancel()

	select {
	case err := <-done:
		asserts.True(t, errors.Is(err, context.Canceled), "loop reports the context cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not called after the context was canceled")
	}
}

// TestConnect_PollsWithFullAllowedUpdates pins the allowed_updates the poll
// loop sends on getUpdates. Setting allowed_updates replaces Telegram's server
// default (and persists server-side), so the list must stay the full default
// set plus message_reaction: message_reaction must be present (it is excluded
// from the default, and OnReaction dies without it), and callback_query is a
// canary for the rest of the default set — if it disappears, the list was
// narrowed and raw-client RegisterHandler consumers silently lose updates.
func TestConnect_PollsWithFullAllowedUpdates(t *testing.T) {
	allowed := make(chan string, 1)
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_ = r.ParseMultipartForm(1 << 20)
			select {
			case allowed <- r.FormValue("allowed_updates"):
			default:
			}
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(error) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")

	select {
	case got := <-allowed:
		asserts.True(t, strings.Contains(got, `"message_reaction"`),
			"getUpdates requests message_reaction (excluded from the server default): "+got)
		asserts.True(t, strings.Contains(got, `"callback_query"`),
			"getUpdates keeps the server-default set (callback_query canary): "+got)
	case <-time.After(2 * time.Second):
		t.Fatal("no getUpdates request reached the stub server")
	}
}

// TestConnect_FirstReusesNewClientThenFreshPerReconnect proves the first Connect
// runs the poll loop on the New-time client (so a pre-Connect RegisterHandler on
// Client(bot) survives), while every reconnect builds a fresh *bot.Bot (so a
// canceled run's buffered updates die with its client instead of being drained,
// and dispatched, by the successor connection).
func TestConnect_FirstReusesNewClientThenFreshPerReconnect(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	newTime := a.client

	base := a.newClient
	var built []*bot.Bot
	a.newClient = func() (*bot.Bot, error) {
		c, err := base()
		built = append(built, c)
		return c, err
	}

	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(error) {},
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx1, deps), "first Connect should not fail")
	asserts.True(t, a.currentClient() == newTime, "first Connect reuses the New-time client")
	asserts.Equal(t, len(built), 0, "first Connect builds no fresh client")
	cancel1()

	ctx2, cancel2 := context.WithCancel(context.Background())
	asserts.NoError(t, a.Connect(ctx2, deps), "reconnect should not fail")
	asserts.Equal(t, len(built), 1, "reconnect builds exactly one fresh client")
	asserts.True(t, a.currentClient() != newTime, "reconnect swaps in a distinct *bot.Bot")
	cancel2()
}

// TestConnect_ReusesNewTimeClientForRegisteredHandlers is the I2 behavioral proof:
// a raw handler registered on Client(bot) BEFORE Connect must fire, which only
// happens if the first Connect runs the poll loop on that same New-time client
// rather than discarding it for a fresh one.
func TestConnect_ReusesNewTimeClientForRegisteredHandlers(t *testing.T) {
	var served atomic.Bool
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") && !served.Swap(true) {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{` +
				`"message_id":1,"date":1,"chat":{"id":100,"type":"private"},` +
				`"from":{"id":7,"is_bot":false,"first_name":"U"},"text":"ping"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	fired := make(chan struct{}, 1)
	a.client.RegisterHandler(bot.HandlerTypeMessageText, "ping", bot.MatchTypeExact,
		func(context.Context, *bot.Bot, *models.Update) {
			select {
			case fired <- struct{}{}:
			default:
			}
		})

	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(error) {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("a handler registered before Connect never fired: the poll loop ran on a different client")
	}
}

// TestConnect_GetMeProbeFailsFast proves an invalid token surfaces as an error
// from Run (via Done) instead of the poll loop silently retrying a 401 forever.
// The probe runs off the Connect path — Connect stays non-blocking, since
// core.Bot holds its mutex across adapter.Connect — so the error arrives on Done,
// not from Connect's return.
func TestConnect_GetMeProbeFailsFast(t *testing.T) {
	a := newStubAdapterRaw(t, 0, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	done := make(chan error, 1)
	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(err error) { done <- err },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	asserts.NoError(t, a.Connect(ctx, deps), "Connect returns without blocking on the probe")
	select {
	case err := <-done:
		asserts.Error(t, err, "an invalid token surfaces via Run/Done, not an infinite silent 401 retry")
	case <-time.After(2 * time.Second):
		t.Fatal("the getMe probe failure was never reported via Done")
	}
}

// chanLogHandler is a race-free slog.Handler for observing log records emitted
// from the poll-loop goroutine: it forwards each record's message on a channel
// (dropping if full) so a test can await it without sharing a buffer.
type chanLogHandler struct{ ch chan string }

func (h chanLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h chanLogHandler) Handle(_ context.Context, r slog.Record) error {
	select {
	case h.ch <- r.Message:
	default:
	}
	return nil
}
func (h chanLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h chanLogHandler) WithGroup(string) slog.Handler      { return h }

// TestConnect_PollErrorRoutedToLogger proves a getUpdates failure in the poll
// loop reaches the logger injected at Connect, rather than go-telegram's default
// handler writing to the stdlib log.
func TestConnect_PollErrorRoutedToLogger(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	logs := make(chan string, 8)
	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(error) {},
		Logger:   slog.New(chanLogHandler{ch: logs}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail (getMe is answered)")

	select {
	case msg := <-logs:
		asserts.True(t, strings.Contains(msg, "poll error"),
			"a poll-loop failure is routed to the injected logger: "+msg)
	case <-time.After(2 * time.Second):
		t.Fatal("poll error was not routed to the injected logger")
	}
}

func TestConnect_DispatchesUpdate(t *testing.T) {
	var served atomic.Bool
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") && !served.Swap(true) {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{` +
				`"message_id":1,"date":1,"chat":{"id":100,"type":"private"},` +
				`"from":{"id":7,"is_bot":false,"first_name":"U"},"text":"ping"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	dispatched := make(chan *core.Message, 1)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { dispatched <- m },
		Done:     func(error) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")

	select {
	case m := <-dispatched:
		asserts.Equal(t, m.UserID, "7", "dispatched message user id")
		asserts.Equal(t, m.ChannelID, "100", "dispatched message chat id")
		asserts.Equal(t, m.Content, "ping", "dispatched message content")
	case <-time.After(2 * time.Second):
		t.Fatal("update was not dispatched through the poll loop")
	}
}

// The library fans updates out to handler goroutines, so arrival order is not asserted.
func TestConnect_DispatchesMultipleUpdates(t *testing.T) {
	var served atomic.Bool
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") && !served.Swap(true) {
			_, _ = w.Write([]byte(`{"ok":true,"result":[` +
				`{"update_id":1,"message":{"message_id":1,"date":1,"chat":{"id":100,"type":"private"},` +
				`"from":{"id":7,"is_bot":false,"first_name":"A"},"text":"one"}},` +
				`{"update_id":2,"message":{"message_id":2,"date":1,"chat":{"id":101,"type":"private"},` +
				`"from":{"id":8,"is_bot":false,"first_name":"B"},"text":"two"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	dispatched := make(chan *core.Message, 2)
	deps := core.AdapterDeps{
		Dispatch: func(_ context.Context, m *core.Message) { dispatched <- m },
		Done:     func(error) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")

	got := make(map[string]string, 2)
	for i := 0; i < 2; i++ {
		select {
		case m := <-dispatched:
			got[m.ChannelID] = m.Content
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 updates dispatched", i)
		}
	}
	asserts.Equal(t, got["100"], "one", "first update dispatched")
	asserts.Equal(t, got["101"], "two", "second update dispatched")
}
