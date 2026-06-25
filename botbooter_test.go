package botbooter

import (
	"context"
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/slack-go/slack/slackevents"
)

// Test helpers to reduce duplication and improve readability.

func assertEqual[T comparable](t *testing.T, got, expected T, message string) {
	t.Helper()
	if got != expected {
		t.Errorf("%s: got %v, expected %v", message, got, expected)
	}
}

func assertNotNil(t *testing.T, got any, message string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected non-nil, got nil", message)
	}
}

func assertError(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error, got nil", message)
	}
}

func assertErrorIs(t *testing.T, err, target error, message string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want errors.Is %v", message, err, target)
	}
}

func assertNoError(t *testing.T, err error, message string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: expected no error, got %v", message, err)
	}
}

func assertTrue(t *testing.T, got bool, message string) {
	t.Helper()
	if !got {
		t.Errorf("%s: expected true, got false", message)
	}
}

func assertFalse(t *testing.T, got bool, message string) {
	t.Helper()
	if got {
		t.Errorf("%s: expected false, got true", message)
	}
}

// newDiscordBot constructs a Discord bot for tests, failing the test if
// construction errors.
func newDiscordBot(t *testing.T) *Bot {
	t.Helper()
	bot, err := InitAsDiscordBot("test_token")
	assertNoError(t, err, "InitAsDiscordBot")
	assertNotNil(t, bot, "bot should be initialized")
	return bot
}

func TestBotType_String(t *testing.T) {
	assertEqual(t, SlackBotType.String(), "slack", "Slack string")
	assertEqual(t, DiscordBotType.String(), "discord", "Discord string")
	assertEqual(t, CLIBotType.String(), "cli", "CLI string")
	assertEqual(t, BotType(999).String(), "BotType(999)", "unknown string")
}

func TestBot_Connect(t *testing.T) {
	t.Run("DiscordBotWithFakeToken", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.Connect(context.Background())

		assertError(t, err, "Connect with fake Discord token should fail")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		bot := &Bot{BotType: BotType(999)}

		err := bot.Connect(context.Background())

		assertErrorIs(t, err, ErrUnknownBotType, "Connect with unknown bot type")
	})

	t.Run("AlreadyConnected", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		bot := InitAsCLIBot(emptyReader{}, &syncBuffer{})

		assertNoError(t, bot.Connect(ctx), "first Connect")
		err := bot.Connect(ctx)
		assertErrorIs(t, err, ErrAlreadyConnected, "second Connect")

		assertNoError(t, bot.Disconnect(), "Disconnect")
	})
}

func TestBot_Disconnect(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.Disconnect()

		assertNoError(t, err, "Disconnect Discord bot should not fail")
	})

	t.Run("SlackBot", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")

		err := bot.Disconnect()

		assertNoError(t, err, "Disconnect Slack bot should not fail")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		bot := &Bot{BotType: BotType(999)}

		err := bot.Disconnect()

		assertErrorIs(t, err, ErrUnknownBotType, "Disconnect with unknown bot type")
	})
}

func TestBot_SendMessage(t *testing.T) {
	t.Run("DiscordBotNotConnected", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.SendMessage("channel123", "test message")

		assertError(t, err, "SendMessage without connection should fail")
	})

	t.Run("SlackBotNotConnected", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")

		err := bot.SendMessage("channel123", "test message")

		assertError(t, err, "SendMessage without connection should fail")
	})

	t.Run("CLIBot", func(t *testing.T) {
		out := &syncBuffer{}
		bot := InitAsCLIBot(emptyReader{}, out)

		err := bot.SendMessage("ignored", "hello world")

		assertNoError(t, err, "CLI SendMessage should not fail")
		assertEqual(t, out.String(), "hello world\n", "CLI output")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		bot := &Bot{BotType: BotType(999)}

		err := bot.SendMessage("channel123", "test message")

		assertErrorIs(t, err, ErrUnknownBotType, "SendMessage with unknown bot type")
	})
}

func TestBot_GetAttachments(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		bot := newDiscordBot(t)
		message := &Message{
			DiscordData: &discordgo.MessageCreate{
				Message: &discordgo.Message{
					Attachments: []*discordgo.MessageAttachment{
						{URL: "https://example.com/image.png", Width: 100, Height: 100},
					},
				},
			},
		}

		attachments, err := bot.GetAttachments(message)

		assertNoError(t, err, "GetAttachments for Discord bot should not fail")
		assertEqual(t, len(attachments), 1, "Number of attachments")
		assertTrue(t, attachments[0].IsImage, "Attachment should be an image")
		assertEqual(t, attachments[0].URL, "https://example.com/image.png", "Attachment URL")
	})

	t.Run("DiscordBotNilData", func(t *testing.T) {
		bot := newDiscordBot(t)

		attachments, err := bot.GetAttachments(&Message{})

		assertNoError(t, err, "GetAttachments with nil Discord data should not fail")
		assertEqual(t, len(attachments), 0, "no attachments")
	})

	t.Run("SlackBot", func(t *testing.T) {
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		message := &Message{
			SlackData: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{Mimetype: "image/png", URLPrivate: "https://example.com/image.png"},
				},
			},
		}

		attachments, err := bot.GetAttachments(message)

		assertNoError(t, err, "GetAttachments for Slack bot should not fail")
		assertEqual(t, len(attachments), 1, "Number of attachments")
		assertTrue(t, attachments[0].IsImage, "Attachment should be an image")
		assertEqual(t, attachments[0].URL, "https://example.com/image.png", "Attachment URL")
	})

	t.Run("CLIBot", func(t *testing.T) {
		bot := InitAsCLIBot(emptyReader{}, &syncBuffer{})

		attachments, err := bot.GetAttachments(&Message{Content: "hi"})

		assertNoError(t, err, "GetAttachments for CLI bot should not fail")
		assertEqual(t, len(attachments), 0, "CLI has no attachments")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		bot := &Bot{BotType: BotType(999)}

		attachments, err := bot.GetAttachments(&Message{})

		assertErrorIs(t, err, ErrUnknownBotType, "GetAttachments with unknown bot type")
		assertEqual(t, len(attachments), 0, "Attachments should be empty for unknown bot type")
	})
}

func TestBot_AddHandler(t *testing.T) {
	t.Run("ValidPattern", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.AddHandler(Command{Pattern: "^hello$", Handler: noopHandler})

		assertNoError(t, err, "AddHandler with valid pattern")
		assertEqual(t, len(bot.commands), 1, "Number of commands after adding handler")
		assertEqual(t, bot.commands[0].Pattern, "^hello$", "Handler pattern")
		assertNotNil(t, bot.commands[0].re, "pattern should be precompiled")
	})

	t.Run("InvalidPattern", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.AddHandler(Command{Pattern: "[invalid(", Handler: noopHandler})

		assertError(t, err, "AddHandler with invalid pattern should fail")
		assertEqual(t, len(bot.commands), 0, "invalid command should not be added")
	})
}

func TestBot_SetUnknownCommandHandler(t *testing.T) {
	bot := newDiscordBot(t)

	bot.SetUnknownCommandHandler(noopHandler)

	assertNotNil(t, bot.unknownCommandHandler, "UnknownCommandHandler should be set")
}

func TestBot_AddMiddleware(t *testing.T) {
	bot := newDiscordBot(t)

	bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
		next(ctx, b, m)
	})

	assertEqual(t, len(bot.middlewares), 1, "Number of middlewares after adding")
}

func TestBot_dispatch(t *testing.T) {
	t.Run("MatchingCommand", func(t *testing.T) {
		bot := newDiscordBot(t)
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		assertTrue(t, handlerCalled, "Handler should be called for matching command")
	})

	t.Run("NoMatchingCommand", func(t *testing.T) {
		bot := newDiscordBot(t)
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})
		unknownCalled := false
		bot.SetUnknownCommandHandler(func(ctx context.Context, b *Bot, m *Message) {
			unknownCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		assertFalse(t, handlerCalled, "Handler should not be called for non-matching command")
		assertTrue(t, unknownCalled, "Unknown handler should be called")
	})

	t.Run("WithMiddleware", func(t *testing.T) {
		bot := newDiscordBot(t)
		middlewareCalled := false
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			middlewareCalled = true
			next(ctx, b, m)
		})
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		assertTrue(t, middlewareCalled, "Middleware should be called")
		assertTrue(t, handlerCalled, "Handler should be called after middleware")
	})

	t.Run("MultipleMiddlewares", func(t *testing.T) {
		bot := newDiscordBot(t)
		var callOrder []int
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			callOrder = append(callOrder, 1)
			next(ctx, b, m)
		})
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			callOrder = append(callOrder, 2)
			next(ctx, b, m)
		})
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			callOrder = append(callOrder, 3)
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		assertEqual(t, len(callOrder), 3, "Number of calls")
		assertEqual(t, callOrder[0], 1, "First middleware should be called first")
		assertEqual(t, callOrder[1], 2, "Second middleware should be called second")
		assertEqual(t, callOrder[2], 3, "Handler should be called last")
	})

	t.Run("MiddlewareShortCircuit", func(t *testing.T) {
		bot := newDiscordBot(t)
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			// Intentionally does not call next.
		})
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		assertFalse(t, handlerCalled, "Handler should not run when middleware short-circuits")
	})

	t.Run("NoUnknownHandler", func(t *testing.T) {
		bot := newDiscordBot(t)
		mustAddHandler(t, bot, "^hello$", noopHandler)

		// Should not panic without an unknown command handler.
		bot.dispatch(context.Background(), &Message{Content: "goodbye"})
	})

	t.Run("FirstMatchWins", func(t *testing.T) {
		bot := newDiscordBot(t)
		firstCalled, secondCalled := false, false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			firstCalled = true
		})
		mustAddHandler(t, bot, "^goodbye$", func(ctx context.Context, b *Bot, m *Message) {
			secondCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		assertFalse(t, firstCalled, "First handler should not be called")
		assertTrue(t, secondCalled, "Second handler should be called")
	})
}

func noopHandler(ctx context.Context, b *Bot, m *Message) {}

func mustAddHandler(t *testing.T, bot *Bot, pattern string, handler CommandHandler) {
	t.Helper()
	assertNoError(t, bot.AddHandler(Command{Pattern: pattern, Handler: handler}), "AddHandler "+pattern)
}
