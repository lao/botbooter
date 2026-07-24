package slack

import (
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{AppToken: "app", BotToken: "bot"})
	asserts.NoError(t, err, "New")
	asserts.Equal(t, bot.BotType, botbooter.SlackBotType, "bot type")
	asserts.NotNil(t, Client(bot), "client")
	asserts.NotNil(t, SocketClient(bot), "socket client")
}

func TestRawEvent(t *testing.T) {
	want := &slackevents.MessageEvent{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})
	asserts.True(t, ok, "raw present")
	asserts.Equal(t, got, want, "raw event")
}

func TestRawReaction(t *testing.T) {
	t.Run("SlackRaw", func(t *testing.T) {
		want := &slackevents.ReactionAddedEvent{}
		got, ok := RawReaction(&botbooter.Reaction{Raw: want})
		asserts.True(t, ok, "raw present")
		asserts.Equal(t, got, want, "raw reaction")
	})

	t.Run("WrongRaw", func(t *testing.T) {
		got, ok := RawReaction(&botbooter.Reaction{Raw: "not a slack reaction"})
		asserts.False(t, ok, "raw absent")
		asserts.Equal(t, got, (*slackevents.ReactionAddedEvent)(nil), "nil reaction")
	})
}
