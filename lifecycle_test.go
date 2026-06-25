package botbooter

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestCommand_matchFallback(t *testing.T) {
	// A Command constructed directly (not via AddHandler) has no precompiled
	// regexp and must fall back to compiling on the fly.
	c := Command{Pattern: "^hi$"}
	assertTrue(t, c.match("hi"), "fallback should match")
	assertFalse(t, c.match("bye"), "fallback should not match")

	bad := Command{Pattern: "[invalid("}
	assertFalse(t, bad.match("anything"), "invalid pattern should not match")
}

func TestConnectSlack_StartsAndStops(t *testing.T) {
	if os.Getenv("BOTBOOTER_SLACK_NETWORK_TEST") == "" {
		t.Skip("set BOTBOOTER_SLACK_NETWORK_TEST=1 to run; performs real Slack network I/O via RunContext")
	}
	bot := InitAsSlackBot("xapp-test", "xoxb-test")
	ctx, cancel := context.WithCancel(context.Background())

	assertNoError(t, bot.Connect(ctx), "Connect Slack should start the loop")

	cancel()
	assertNoError(t, bot.Disconnect(), "Disconnect Slack should not fail")
}

func TestDisconnect_DiscordNilSession(t *testing.T) {
	bot := &Bot{BotType: DiscordBotType}

	assertNoError(t, bot.Disconnect(), "Disconnect with nil session should not fail")
}

func TestOnDiscordMessage(t *testing.T) {
	newSession := func(botUserID string) *discordgo.Session {
		s := &discordgo.Session{State: discordgo.NewState()}
		s.State.User = &discordgo.User{ID: botUserID}
		return s
	}

	t.Run("UserMessageDispatched", func(t *testing.T) {
		bot := newDiscordBot(t)
		var got *Message
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			got = m
		})

		bot.onDiscordMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{
				Author:    &discordgo.User{ID: "user-1"},
				ChannelID: "chan-1",
				Content:   "hello",
			},
		})

		assertNotNil(t, got, "handler should be called for user message")
		assertEqual(t, got.UserID, "user-1", "message user")
		assertEqual(t, got.ChannelID, "chan-1", "message channel")
	})

	t.Run("OwnMessageIgnored", func(t *testing.T) {
		bot := newDiscordBot(t)
		called := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			called = true
		})

		bot.onDiscordMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Author: &discordgo.User{ID: "bot-id"}, Content: "hi"},
		})

		assertFalse(t, called, "bot's own message should be ignored")
	})

	t.Run("NilAuthorIgnored", func(t *testing.T) {
		bot := newDiscordBot(t)
		called := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			called = true
		})

		bot.onDiscordMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Content: "hi"},
		})

		assertFalse(t, called, "message without author should be ignored")
	})

	t.Run("OtherBotIgnored", func(t *testing.T) {
		bot := newDiscordBot(t)
		called := false
		mustAddHandler(t, bot, ".*", func(ctx context.Context, b *Bot, m *Message) {
			called = true
		})

		bot.onDiscordMessage(context.Background(), newSession("bot-id"), &discordgo.MessageCreate{
			Message: &discordgo.Message{Author: &discordgo.User{ID: "other-bot", Bot: true}, Content: "hi"},
		})

		assertFalse(t, called, "another bot's message should be ignored")
	})
}

func TestBot_HandleFunc(t *testing.T) {
	bot := newDiscordBot(t)
	called := false
	assertNoError(t, bot.HandleFunc("^hi$", func(ctx context.Context, b *Bot, m *Message) {
		called = true
	}), "HandleFunc with valid pattern")

	bot.dispatch(context.Background(), &Message{Content: "hi"})
	assertTrue(t, called, "HandleFunc handler should be called")

	assertError(t, bot.HandleFunc("[bad(", noopHandler), "HandleFunc with invalid pattern should fail")
}

func TestBot_dispatch_RecoversFromPanic(t *testing.T) {
	bot := newDiscordBot(t)
	mustAddHandler(t, bot, "^boom$", func(ctx context.Context, b *Bot, m *Message) {
		panic("handler exploded")
	})

	// dispatch must recover; the call should return normally rather than
	// propagating the panic and crashing the event loop.
	bot.dispatch(context.Background(), &Message{Content: "boom"})
}

func TestBot_Start_GracefulShutdownOnSignal(t *testing.T) {
	// Guard: register our own SIGTERM handler first so that even if the signal
	// somehow arrives before Start's signal.NotifyContext registers, the
	// default disposition (terminate the process) cannot kill the test binary.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	// A pipe that never reaches EOF keeps the CLI loop blocked so Start waits
	// on the signal-bound context rather than returning early.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	bot := InitAsCLIBot(pr, &syncBuffer{})

	done := make(chan error, 1)
	go func() { done <- bot.Start() }()

	// Give Start time to register the signal handler before we signal.
	time.Sleep(150 * time.Millisecond)
	proc, err := os.FindProcess(os.Getpid())
	assertNoError(t, err, "FindProcess")
	assertNoError(t, proc.Signal(syscall.SIGTERM), "send SIGTERM")

	select {
	case err := <-done:
		assertNoError(t, err, "Start should return after SIGTERM")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not shut down after SIGTERM")
	}
}
