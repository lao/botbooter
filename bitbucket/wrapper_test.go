package bitbucket

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{Email: "e@x", APIToken: "tok", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new Bitbucket bot")
	asserts.Equal(t, bot.BotType, botbooter.BitbucketBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestNewAmbiguousAuth(t *testing.T) {
	_, err := New(Config{Email: "e", APIToken: "t", AccessToken: "a", Secret: "s", Addr: ":0"})

	asserts.ErrorIs(t, err, ErrAmbiguousAuth, "both auth modes")
}

func TestCloudClient(t *testing.T) {
	bot, err := New(Config{Email: "e@x", APIToken: "tok", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new Bitbucket bot")
	asserts.NotNil(t, CloudClient(bot), "cloud client for a Cloud bot")
}

func TestAddr(t *testing.T) {
	bot, err := New(Config{Email: "e@x", APIToken: "tok", Secret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new Bitbucket bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}

func TestRawEvent(t *testing.T) {
	want := &Message{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})

	asserts.True(t, ok, "raw event present")
	asserts.Equal(t, got, want, "raw event")

	_, ok = RawEvent(&botbooter.Message{Raw: "not a bitbucket event"})
	asserts.False(t, ok, "foreign raw payload rejected")
}
