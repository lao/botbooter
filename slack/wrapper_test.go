package slack

import (
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot := New("app", "bot")
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
