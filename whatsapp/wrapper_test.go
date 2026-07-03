package whatsapp

import (
	"context"
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// foreignAdapter is a non-WhatsApp core.Adapter used to exercise Addr's
// wrong-type fallback branch.
type foreignAdapter struct{}

func (foreignAdapter) Connect(context.Context, core.AdapterDeps) error      { return nil }
func (foreignAdapter) Disconnect() error                                    { return nil }
func (foreignAdapter) Send(context.Context, string, string) error           { return nil }
func (foreignAdapter) Attachments(*core.Message) ([]core.Attachment, error) { return nil, nil }

func TestNew(t *testing.T) {
	bot, err := New(Config{Token: "t", PhoneNumberID: "p", AppSecret: "s", VerifyToken: "v", Addr: ":0"})
	asserts.NoError(t, err, "new whatsapp bot")
	asserts.Equal(t, bot.BotType, botbooter.WhatsAppBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})
	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestRawMessage(t *testing.T) {
	want := &Message{From: "1234"}
	got, ok := RawMessage(&botbooter.Message{Raw: want})
	asserts.True(t, ok, "raw present")
	asserts.Equal(t, got, want, "raw message")
}

func TestRawReaction(t *testing.T) {
	t.Run("Present", func(t *testing.T) {
		want := &Message{From: "1234", Reaction: &ReactionInfo{}}
		got, ok := RawReaction(&botbooter.Reaction{Raw: want})
		asserts.True(t, ok, "raw present")
		asserts.Equal(t, got, want, "raw reaction")
	})
	t.Run("WrongType", func(t *testing.T) {
		got, ok := RawReaction(&botbooter.Reaction{Raw: "not a whatsapp message"})
		asserts.False(t, ok, "raw absent")
		if got != nil {
			t.Fatalf("expected nil message, got %v", got)
		}
	})
}

func TestAddr(t *testing.T) {
	t.Run("WrongType", func(t *testing.T) {
		bot := core.New(botbooter.CLIBotType, foreignAdapter{})
		asserts.Equal(t, Addr(bot), "", "wrong-type addr")
	})
	t.Run("NotConnected", func(t *testing.T) {
		bot, err := New(Config{Token: "t", PhoneNumberID: "p", AppSecret: "s", VerifyToken: "v", Addr: ":0"})
		asserts.NoError(t, err, "new whatsapp bot")
		asserts.Equal(t, Addr(bot), "", "not-connected addr")
	})
}
