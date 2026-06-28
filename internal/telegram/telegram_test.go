package telegram

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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

// newStubAdapter wires an adapter to an httptest Bot API server, mirroring New without real network I/O.
func newStubAdapter(t *testing.T, selfID int64, handler http.HandlerFunc) *adapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	a := &adapter{selfID: selfID}
	a.newClient = func() (*bot.Bot, error) {
		return bot.New("123:test-token",
			bot.WithDefaultHandler(a.onUpdate),
			bot.WithServerURL(srv.URL),
			bot.WithSkipGetMe(),
		)
	}
	tg, err := a.newClient()
	asserts.NoError(t, err, "bot.New for stub server")
	a.client = tg
	return a
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
			{Type: models.MessageEntityTypeMention}, // @username — no id, skipped
		},
	}}
	got := toMessage(u)
	asserts.Equal(t, strings.Join(got.MentionedUserIDs, ","), "99", "text_mention id only")
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

func TestFileIDOf(t *testing.T) {
	asserts.Equal(t, fileIDOf(models.PhotoSize{FileID: "p"}), "p", "photo size value yields its FileID")
	asserts.Equal(t, fileIDOf(&models.PhotoSize{FileID: "pp"}), "pp", "photo size pointer also yields its FileID")
	asserts.Equal(t, fileIDOf(&models.Document{FileID: "d"}), "d", "document pointer yields its FileID")
	asserts.Equal(t, fileIDOf((*models.Document)(nil)), "", "nil document pointer is guarded")
	asserts.Equal(t, fileIDOf(nil), "", "nil ExtraData yields no FileID")
	asserts.Equal(t, fileIDOf("unrelated"), "", "unrecognized ExtraData yields no FileID")
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
	b := core.New(core.TelegramBotType, a)

	url, err := ResolveAttachmentURL(context.Background(), b,
		core.Attachment{ExtraData: models.PhotoSize{FileID: "large"}})

	asserts.NoError(t, err, "getFile succeeds against the stub server")
	want := a.client.FileDownloadLink(&models.File{FilePath: "photos/file_1.jpg"})
	asserts.Equal(t, url, want, "URL is the download link for the resolved file_path")
}

func TestResolveAttachmentURL_GetFileError(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`))
	})
	b := core.New(core.TelegramBotType, a)

	url, err := ResolveAttachmentURL(context.Background(), b,
		core.Attachment{ExtraData: models.PhotoSize{FileID: "huge"}})

	asserts.Error(t, err, "a getFile failure is surfaced")
	asserts.Equal(t, url, "", "no URL is returned on error")
}

func TestResolveAttachmentURL_NotTelegram(t *testing.T) {
	b := core.New(core.CLIBotType, nil)

	url, err := ResolveAttachmentURL(context.Background(), b,
		core.Attachment{ExtraData: models.PhotoSize{FileID: "x"}})

	asserts.ErrorIs(t, err, ErrNotTelegramBot, "a non-Telegram bot is rejected")
	asserts.Equal(t, url, "", "no URL for a non-Telegram bot")
}

func TestResolveAttachmentURL_NoFileID(t *testing.T) {
	a := newStubAdapter(t, 0, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("getFile must not be called when the attachment has no file id")
	})
	b := core.New(core.TelegramBotType, a)

	url, err := ResolveAttachmentURL(context.Background(), b, core.Attachment{})

	asserts.NoError(t, err, "an attachment without a file id is not an error")
	asserts.Equal(t, url, "", "no file id yields an empty URL")
}

// captureLog redirects the default logger to a buffer for the test, restoring it
// afterward, so a test can assert on what ResolveAttachmentURL logged.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return &buf
}

func resolvePhoto(t *testing.T) error {
	t.Helper()
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f","file_path":"photos/file_1.jpg"}}`))
	})
	b := core.New(core.TelegramBotType, a)
	_, err := ResolveAttachmentURL(context.Background(), b,
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

	err := a.Send(context.Background(), "555", "hi there")

	asserts.NoError(t, err, "Send should succeed against a 200 OK API reply")
	asserts.Equal(t, gotChatID, "555", "numeric channel id sent as chat_id")
	asserts.Equal(t, gotText, "hi there", "text forwarded to the API")
}

func TestSend_SurfacesError(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
	})

	err := a.Send(context.Background(), "555", "hi")

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

// TestConnect_FreshClientPerConnection proves each Connect builds its own *bot.Bot,
// so a canceled run's buffered updates die with its client instead of being drained,
// and dispatched, by the next connection.
func TestConnect_FreshClientPerConnection(t *testing.T) {
	a := newStubAdapter(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})

	base := a.newClient
	var clients []*bot.Bot
	a.newClient = func() (*bot.Bot, error) {
		c, err := base()
		clients = append(clients, c)
		return c, err
	}

	deps := core.AdapterDeps{
		Dispatch: func(context.Context, *core.Message) {},
		Done:     func(error) {},
	}

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		asserts.NoError(t, a.Connect(ctx, deps), "Connect should not fail")
		cancel()
	}

	asserts.Equal(t, len(clients), 2, "each Connect builds its own client")
	asserts.True(t, clients[0] != clients[1], "connections use distinct *bot.Bot instances")
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
