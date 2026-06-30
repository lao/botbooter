package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New("test_token")
	asserts.NoError(t, err, "new discord bot")
	asserts.Equal(t, bot.BotType, botbooter.DiscordBotType, "bot type")
	asserts.NotNil(t, Session(bot), "session")
}

func TestRawEvent(t *testing.T) {
	want := &discordgo.MessageCreate{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})
	asserts.True(t, ok, "raw present")
	asserts.Equal(t, got, want, "raw event")
}
