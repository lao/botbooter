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

func TestRawReaction(t *testing.T) {
	t.Run("Discord", func(t *testing.T) {
		want := &discordgo.MessageReactionAdd{}
		got, ok := RawReaction(&botbooter.Reaction{Raw: want})
		asserts.True(t, ok, "raw present")
		asserts.Equal(t, got, want, "raw reaction")
	})

	t.Run("Nil", func(t *testing.T) {
		got, ok := RawReaction(&botbooter.Reaction{})
		asserts.False(t, ok, "raw absent")
		asserts.True(t, got == nil, "nil raw reaction")
	})

	t.Run("OtherType", func(t *testing.T) {
		got, ok := RawReaction(&botbooter.Reaction{Raw: "not a reaction"})
		asserts.False(t, ok, "raw not a discord reaction")
		asserts.True(t, got == nil, "nil raw reaction")
	})
}
