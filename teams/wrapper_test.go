package teams

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{AppID: "app-id", AppPassword: "secret", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new Teams bot")
	asserts.Equal(t, bot.BotType, botbooter.TeamsBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestRawMessage(t *testing.T) {
	want := &Message{From: "user-1"}
	got, ok := RawMessage(&botbooter.Message{Raw: want})

	asserts.True(t, ok, "raw message present")
	asserts.Equal(t, got, want, "raw message")
}
