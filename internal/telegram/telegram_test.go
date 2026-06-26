package telegram

import (
	"context"
	"errors"
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

// newCaptureAdapter bypasses New and the network so onUpdate can be tested directly.
func newCaptureAdapter(selfID int64, got **core.Message) *adapter {
	a := &adapter{selfID: selfID}
	deps := captureDeps(got)
	a.deps.Store(&deps)
	return a
}

// newStubAdapter wires an adapter to an httptest Bot API server, mirroring New without real network I/O.
func newStubAdapter(t *testing.T, selfID int64, handler http.HandlerFunc) *adapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	a := &adapter{selfID: selfID}
	tg, err := bot.New("123:test-token",
		bot.WithDefaultHandler(a.onUpdate),
		bot.WithServerURL(srv.URL),
		bot.WithSkipGetMe(),
	)
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
	asserts.NotNil(t, b.TelegramBot, "Telegram client escape hatch should be set")
	asserts.Equal(t, b.TelegramBot.ID(), int64(123456), "self id parsed from token prefix")
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
		a := newCaptureAdapter(selfID, &got)

		u := userMessage("hello", "")
		a.onUpdate(context.Background(), nil, u)

		asserts.NotNil(t, got, "user message should be dispatched")
		asserts.Equal(t, got.UserID, "7", "message user id")
		asserts.Equal(t, got.ChannelID, "100", "message chat id")
		asserts.Equal(t, got.Content, "hello", "message content")
		asserts.True(t, got.TelegramData == u, "raw update should be carried on TelegramData")
	})

	t.Run("CaptionUsedWhenTextEmpty", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		a.onUpdate(context.Background(), nil, userMessage("", "a caption"))

		asserts.NotNil(t, got, "captioned message should be dispatched")
		asserts.Equal(t, got.Content, "a caption", "caption used as content when text is empty")
	})

	t.Run("PhotoOnlyDispatchedWithEmptyContent", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		u := &models.Update{Message: &models.Message{
			From:  &models.User{ID: 7},
			Chat:  models.Chat{ID: 100},
			Photo: []models.PhotoSize{{FileID: "f1"}},
		}}
		a.onUpdate(context.Background(), nil, u)

		asserts.NotNil(t, got, "photo-only message should still be dispatched (pass-through)")
		asserts.Equal(t, got.Content, "", "photo-only message has empty content")
		asserts.True(t, got.TelegramData == u, "raw update carried so handlers can read the photo")
	})

	t.Run("NoMessageIgnored", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		a.onUpdate(context.Background(), nil, &models.Update{})

		asserts.True(t, got == nil, "update without a message should be ignored")
	})

	t.Run("NoSenderIgnored", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		a.onUpdate(context.Background(), nil, &models.Update{Message: &models.Message{Text: "hi"}})

		asserts.True(t, got == nil, "message without a sender should be ignored")
	})

	t.Run("OtherBotIgnored", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		a.onUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			From: &models.User{ID: 9, IsBot: true}, Chat: models.Chat{ID: 1}, Text: "hi",
		}})

		asserts.True(t, got == nil, "another bot's message should be ignored")
	})

	t.Run("OwnMessageIgnored", func(t *testing.T) {
		var got *core.Message
		a := newCaptureAdapter(selfID, &got)

		a.onUpdate(context.Background(), nil, &models.Update{Message: &models.Message{
			From: &models.User{ID: selfID}, Chat: models.Chat{ID: 1}, Text: "hi",
		}})

		asserts.True(t, got == nil, "the bot's own message should be ignored")
	})
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
