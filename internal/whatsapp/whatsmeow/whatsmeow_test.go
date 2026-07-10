package whatsmeow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	wm "go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// jid builds a regular user JID for tests, e.g. jid("123") -> 123@s.whatsapp.net.
func jid(user string) types.JID { return types.NewJID(user, types.DefaultUserServer) }

// textEvent builds an incoming text-message event.
func textEvent(text string, fromMe bool) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: jid("123"), Sender: jid("456"), IsFromMe: fromMe},
		},
		Message: &waProto.Message{Conversation: proto.String(text)},
	}
}

func TestMessageText(t *testing.T) {
	cases := []struct {
		name string
		msg  *waProto.Message
		want string
	}{
		{"Conversation", &waProto.Message{Conversation: proto.String("hi")}, "hi"},
		{"ExtendedText", &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("yo")}}, "yo"},
		{"ImageCaption", &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("pic")}}, "pic"},
		{"VideoCaption", &waProto.Message{VideoMessage: &waProto.VideoMessage{Caption: proto.String("vid")}}, "vid"},
		{"DocumentCaption", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{Caption: proto.String("doc")}}, "doc"},
		{"Empty", &waProto.Message{}, ""},
		{"Nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := messageText(&events.Message{Message: tc.msg})
			asserts.Equal(t, got, tc.want, "messageText")
		})
	}
}

func TestOnMessage(t *testing.T) {
	t.Run("DispatchesIncoming", func(t *testing.T) {
		var got *core.Message
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, m *core.Message) { got = m }}

		(&adapter{}).onMessage(context.Background(), textEvent("ping", false), deps)

		asserts.NotNil(t, got, "dispatched message")
		asserts.Equal(t, got.Content, "ping", "content")
		asserts.Equal(t, got.UserID, "456@s.whatsapp.net", "user id is sender JID")
		asserts.Equal(t, got.ChannelID, "123@s.whatsapp.net", "channel id is chat JID")
		raw, ok := RawMessage(got)
		asserts.True(t, ok, "raw event preserved")
		asserts.NotNil(t, raw, "raw event non-nil")
	})

	t.Run("DropsOwnMessage", func(t *testing.T) {
		dispatched := false
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, _ *core.Message) { dispatched = true }}

		(&adapter{}).onMessage(context.Background(), textEvent("loop", true), deps)

		asserts.False(t, dispatched, "own message must not be dispatched")
	})

	t.Run("DropsEmptySystemMessage", func(t *testing.T) {
		dispatched := false
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, _ *core.Message) { dispatched = true }}
		// A reaction/receipt/revoke arrives as an events.Message with no text
		// and no media.
		ev := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: jid("123"), Sender: jid("456")}},
			Message: &waProto.Message{},
		}

		(&adapter{}).onMessage(context.Background(), ev, deps)

		asserts.False(t, dispatched, "empty/system event must not be dispatched")
	})

	t.Run("DropsNewsletter", func(t *testing.T) {
		dispatched := false
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, _ *core.Message) { dispatched = true }}
		ev := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("123", types.NewsletterServer), Sender: jid("456")}},
			Message: &waProto.Message{Conversation: proto.String("channel post")},
		}

		(&adapter{}).onMessage(context.Background(), ev, deps)

		asserts.False(t, dispatched, "newsletter/channel post must not be dispatched")
	})

	t.Run("DropsBroadcast", func(t *testing.T) {
		dispatched := false
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, _ *core.Message) { dispatched = true }}
		ev := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("status", types.BroadcastServer), Sender: jid("456")}},
			Message: &waProto.Message{Conversation: proto.String("status update")},
		}

		(&adapter{}).onMessage(context.Background(), ev, deps)

		asserts.False(t, dispatched, "broadcast/status must not be dispatched")
	})

	t.Run("DispatchesMediaWithoutCaption", func(t *testing.T) {
		var got *core.Message
		deps := core.AdapterDeps{Dispatch: func(_ context.Context, m *core.Message) { got = m }}
		ev := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: jid("123"), Sender: jid("456")}},
			Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{}},
		}

		(&adapter{}).onMessage(context.Background(), ev, deps)

		asserts.NotNil(t, got, "captionless media is still dispatched")
		asserts.Equal(t, got.Content, "", "no caption yields empty content")
	})
}

func TestOnEventLoggedOut(t *testing.T) {
	called := false
	var doneErr error
	report := func(err error) { called = true; doneErr = err }

	(&adapter{}).onEvent(context.Background(), &events.LoggedOut{}, core.AdapterDeps{}, report)

	asserts.True(t, called, "Done called on logout")
	asserts.ErrorIs(t, doneErr, ErrLoggedOut, "logout surfaces ErrLoggedOut")
}

func TestAttachments(t *testing.T) {
	a := &adapter{}
	msg := func(m *waProto.Message) *core.Message {
		return &core.Message{Raw: &events.Message{Message: m}}
	}

	t.Run("Image", func(t *testing.T) {
		got, err := a.Attachments(msg(&waProto.Message{ImageMessage: &waProto.ImageMessage{Mimetype: proto.String("image/jpeg")}}))
		asserts.NoError(t, err, "attachments")
		asserts.Equal(t, len(got), 1, "one attachment")
		asserts.True(t, got[0].IsImage, "image flagged")
		asserts.Equal(t, got[0].URL, "", "url empty (encrypted media)")
		asserts.NotNil(t, got[0].ExtraData, "media proto in ExtraData")
	})

	t.Run("Document", func(t *testing.T) {
		got, err := a.Attachments(msg(&waProto.Message{DocumentMessage: &waProto.DocumentMessage{}}))
		asserts.NoError(t, err, "attachments")
		asserts.Equal(t, len(got), 1, "one attachment")
		asserts.False(t, got[0].IsImage, "document not an image")
	})

	t.Run("None", func(t *testing.T) {
		got, err := a.Attachments(msg(&waProto.Message{Conversation: proto.String("hi")}))
		asserts.NoError(t, err, "attachments")
		asserts.Equal(t, len(got), 0, "no attachments for text")
	})

	t.Run("NilData", func(t *testing.T) {
		got, err := a.Attachments(&core.Message{})
		asserts.NoError(t, err, "attachments")
		asserts.Equal(t, len(got), 0, "no attachments without whatsmeow data")
	})
}

// TestPumpQR feeds pre-baked QR-channel items through pumpQR and checks which
// codes reach the callback and which outcomes end the run loop. whatsmeow
// closes the channel after a terminal item (or on context cancellation).
func TestPumpQR(t *testing.T) {
	run := func(items ...wm.QRChannelItem) (codes []string, done []error) {
		ch := make(chan wm.QRChannelItem, len(items))
		for _, it := range items {
			ch <- it
		}
		close(ch)
		a := &adapter{qrCallback: func(code string) { codes = append(codes, code) }}
		a.pumpQR(ch, func(err error) { done = append(done, err) })
		return codes, done
	}

	code := func(c string) wm.QRChannelItem {
		return wm.QRChannelItem{Event: wm.QRChannelEventCode, Code: c}
	}

	t.Run("CodesThenSuccess", func(t *testing.T) {
		codes, done := run(code("a"), code("b"), wm.QRChannelSuccess)
		asserts.Equal(t, len(codes), 2, "both codes forwarded")
		asserts.Equal(t, codes[0], "a", "first code")
		asserts.Equal(t, codes[1], "b", "second code")
		asserts.Equal(t, len(done), 0, "success must not end the run loop")
	})

	t.Run("ScannedWithoutMultideviceIsNotTerminal", func(t *testing.T) {
		codes, done := run(wm.QRChannelScannedWithoutMultidevice, code("fresh"), wm.QRChannelSuccess)
		asserts.Equal(t, len(codes), 1, "fresh code after non-multidevice scan forwarded")
		asserts.Equal(t, len(done), 0, "pairing continued to success")
	})

	t.Run("PairingErrorReportsDone", func(t *testing.T) {
		sentinel := errors.New("boom")
		_, done := run(wm.QRChannelItem{Event: wm.QRChannelEventError, Error: sentinel})
		asserts.Equal(t, len(done), 1, "error ends the run loop exactly once")
		asserts.ErrorIs(t, done[0], sentinel, "pairing error wrapped")
	})

	t.Run("TimeoutReportsDone", func(t *testing.T) {
		_, done := run(wm.QRChannelTimeout)
		asserts.Equal(t, len(done), 1, "timeout ends the run loop")
		asserts.Error(t, done[0], "timeout surfaces an error")
	})

	t.Run("ChannelClosedWithoutTerminal", func(t *testing.T) {
		// Context cancellation closes the channel with no terminal item; core's
		// Run is already ending via ctx, so pumpQR must not report done.
		_, done := run(code("a"))
		asserts.Equal(t, len(done), 0, "plain close must not end the run loop")
	})
}

func TestSendInvalidJID(t *testing.T) {
	// A real (never-connected) adapter: ParseJID rejects the non-numeric agent,
	// and even if parsing were loosened, SendMessage on a disconnected client
	// errors rather than panicking.
	a, err := newAdapter(Config{DBPath: filepath.Join(t.TempDir(), "wa.db")})
	asserts.NoError(t, err, "newAdapter")
	t.Cleanup(func() { _ = a.Disconnect() })

	err = a.Send(context.Background(), "12.ab@s.whatsapp.net", "hi", core.SendOptions{})
	asserts.Error(t, err, "invalid JID must error")
}

func TestNew(t *testing.T) {
	db := filepath.Join(t.TempDir(), "wa.db")

	bot, err := New(Config{DBPath: db})
	asserts.NoError(t, err, "New opens the store")
	asserts.Equal(t, bot.BotType, core.WhatsMeowBotType, "bot type")
	asserts.NotNil(t, Client(bot), "client escape hatch resolves")

	// The session store is a credential: it must not be group/world accessible.
	info, err := os.Stat(db)
	asserts.NoError(t, err, "stat store file")
	asserts.Equal(t, info.Mode().Perm()&0o077, os.FileMode(0), "store restricted to owner")
}

func TestNewAdapterDefaultDBPath(t *testing.T) {
	// An empty DBPath falls back to the default filename in the working
	// directory, with the same owner-only permissions as an explicit path.
	t.Chdir(t.TempDir())

	a, err := newAdapter(Config{})
	asserts.NoError(t, err, "newAdapter with zero Config")
	t.Cleanup(func() { _ = a.Disconnect() })

	info, err := os.Stat(defaultDBPath)
	asserts.NoError(t, err, "store created at "+defaultDBPath)
	asserts.Equal(t, info.Mode().Perm()&0o077, os.FileMode(0), "default store restricted to owner")
}

func TestNewAdapterDBPathURIChars(t *testing.T) {
	// SQLite parses the DSN as a URI, so '?', '#' and '%' in the path must be
	// escaped or the filename is truncated and the pragmas are swallowed.
	db := filepath.Join(t.TempDir(), "wa?x#1%2.db")

	a, err := newAdapter(Config{DBPath: db})
	asserts.NoError(t, err, "newAdapter with URI metacharacters in DBPath")
	t.Cleanup(func() { _ = a.Disconnect() })

	_, err = os.Stat(db)
	asserts.NoError(t, err, "store created at the literal path")
}

func TestClientNotWhatsMeowBot(t *testing.T) {
	asserts.True(t, Client(&core.Bot{}) == nil, "nil for a bot without a whatsmeow adapter")
}

func TestDownloadErrors(t *testing.T) {
	t.Run("NotWhatsMeowBot", func(t *testing.T) {
		_, err := Download(context.Background(), &core.Bot{}, core.Attachment{})
		asserts.ErrorIs(t, err, core.ErrUnknownBotType, "foreign bot rejected")
	})

	t.Run("NotDownloadable", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "wa.db")
		bot, err := New(Config{DBPath: db})
		asserts.NoError(t, err, "New")
		_, err = Download(context.Background(), bot, core.Attachment{ExtraData: "not media"})
		asserts.ErrorIs(t, err, ErrNotDownloadable, "non-media attachment rejected")
	})
}

func TestAdapterDisconnect(t *testing.T) {
	db := filepath.Join(t.TempDir(), "wa.db")
	a, err := newAdapter(Config{DBPath: db})
	asserts.NoError(t, err, "newAdapter")
	asserts.NotNil(t, a.client, "client built")

	// Safe and store-closing when never connected, and idempotent.
	asserts.NoError(t, a.Disconnect(), "first disconnect closes the owned store")
	asserts.NoError(t, a.Disconnect(), "disconnect is idempotent")
}
