package github

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{Token: "ghp_x", WebhookSecret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, bot.BotType, botbooter.GitHubBotType, "bot type")
}

func TestNewMissingConfig(t *testing.T) {
	_, err := New(Config{})

	asserts.ErrorIs(t, err, ErrMissingConfig, "missing config")
}

func TestNewAmbiguousAuth(t *testing.T) {
	_, err := New(Config{Token: "t", AppID: 1, InstallationID: 2, PrivateKey: []byte("k"),
		WebhookSecret: "s", Addr: ":0"})

	asserts.ErrorIs(t, err, ErrAmbiguousAuth, "both auth modes")
}

func TestClient(t *testing.T) {
	bot, err := New(Config{Token: "ghp_x", WebhookSecret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitHub bot")
	asserts.NotNil(t, Client(bot), "client for a GitHub bot")
}

func TestAddr(t *testing.T) {
	bot, err := New(Config{Token: "ghp_x", WebhookSecret: "s", Addr: "127.0.0.1:0"})

	asserts.NoError(t, err, "new GitHub bot")
	asserts.Equal(t, Addr(bot), "", "Addr is empty before Connect")
}

func TestRawEvent(t *testing.T) {
	want := &Message{}
	got, ok := RawEvent(&botbooter.Message{Raw: want})

	asserts.True(t, ok, "raw event present")
	asserts.Equal(t, got, want, "raw event")
}

func TestNewBadReactionConfig(t *testing.T) {
	_, err := New(Config{Token: "ghp_x", WebhookSecret: "s", Addr: "127.0.0.1:0",
		ReactionPollRepos: []string{"not-owner-slash-name"}})

	asserts.ErrorIs(t, err, ErrBadReactionConfig, "malformed poll repo")
}

func TestRawReaction(t *testing.T) {
	want := &ReactionPayload{}
	got, ok := RawReaction(&botbooter.Reaction{Raw: want})
	asserts.True(t, ok, "raw reaction present")
	asserts.Equal(t, got, want, "raw reaction")

	_, ok = RawReaction(&botbooter.Reaction{Raw: "not a github reaction"})
	asserts.False(t, ok, "foreign raw payload rejected")
}
