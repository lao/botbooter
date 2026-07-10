package cloud

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

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
