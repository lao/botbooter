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

func TestRawReactionUpdate(t *testing.T) {
	t.Run("Present", func(t *testing.T) {
		want := &models.MessageReactionUpdated{MessageID: 55}
		got, ok := RawReactionUpdate(&botbooter.Reaction{Raw: &models.Update{MessageReaction: want}})
		asserts.True(t, ok, "reaction raw present")
		asserts.Equal(t, got, want, "raw reaction update")
	})

	t.Run("WrongRaw", func(t *testing.T) {
		got, ok := RawReactionUpdate(&botbooter.Reaction{Raw: "not a reaction update"})
		asserts.False(t, ok, "wrong raw type")
		asserts.True(t, got == nil, "nil update")
	})
}
