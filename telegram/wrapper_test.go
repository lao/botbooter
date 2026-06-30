package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New("123456:test-token")
	asserts.NoError(t, err, "new telegram bot")
	asserts.Equal(t, bot.BotType, botbooter.TelegramBotType, "bot type")
	asserts.NotNil(t, Client(bot), "client")
}

func TestRawUpdate(t *testing.T) {
	want := &models.Update{}
	got, ok := RawUpdate(&botbooter.Message{Raw: want})
	asserts.True(t, ok, "raw present")
	asserts.Equal(t, got, want, "raw update")
}
