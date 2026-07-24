package gitlab

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{Token: "glpat-x", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitLab bot")
	asserts.Equal(t, bot.BotType, botbooter.GitLabBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestClient(t *testing.T) {
	bot, err := New(Config{Token: "glpat-x", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitLab bot")
	asserts.NotNil(t, Client(bot), "client for a GitLab bot")
}

func TestAddr(t *testing.T) {
	bot, err := New(Config{Token: "glpat-x", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitLab bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}

func TestRawEvent(t *testing.T) {
	want := &Message{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})

	asserts.True(t, ok, "raw event present")
	asserts.Equal(t, got, want, "raw event")

	_, ok = RawEvent(&botbooter.Message{Raw: "not a gitlab event"})
	asserts.False(t, ok, "foreign raw payload rejected")
}
