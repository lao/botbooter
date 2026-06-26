package core

import (
	"context"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

func noopHandler(ctx context.Context, b *Bot, m *Message) {}

func mustAddHandler(t *testing.T, bot *Bot, pattern string, handler CommandHandler) {
	t.Helper()
	asserts.NoError(t, bot.AddHandler(Command{Pattern: pattern, Handler: handler}), "AddHandler "+pattern)
}

func TestBotType_String(t *testing.T) {
	asserts.Equal(t, SlackBotType.String(), "slack", "Slack string")
	asserts.Equal(t, DiscordBotType.String(), "discord", "Discord string")
	asserts.Equal(t, CLIBotType.String(), "cli", "CLI string")
	asserts.Equal(t, BotType(999).String(), "BotType(999)", "unknown string")
}

func TestCommand_matchFallback(t *testing.T) {
	// A Command constructed directly (not via AddHandler) has no precompiled
	// regexp and must fall back to compiling on the fly.
	c := Command{Pattern: "^hi$"}
	asserts.True(t, c.match("hi"), "fallback should match")
	asserts.False(t, c.match("bye"), "fallback should not match")

	bad := Command{Pattern: "[invalid("}
	asserts.False(t, bad.match("anything"), "invalid pattern should not match")
}

func TestBot_AddHandler(t *testing.T) {
	t.Run("ValidPattern", func(t *testing.T) {
		bot := &Bot{}

		err := bot.AddHandler(Command{Pattern: "^hello$", Handler: noopHandler})

		asserts.NoError(t, err, "AddHandler with valid pattern")
		asserts.Equal(t, len(bot.commands), 1, "Number of commands after adding handler")
		asserts.Equal(t, bot.commands[0].Pattern, "^hello$", "Handler pattern")
		asserts.NotNil(t, bot.commands[0].re, "pattern should be precompiled")
	})

	t.Run("InvalidPattern", func(t *testing.T) {
		bot := &Bot{}

		err := bot.AddHandler(Command{Pattern: "[invalid(", Handler: noopHandler})

		asserts.Error(t, err, "AddHandler with invalid pattern should fail")
		asserts.Equal(t, len(bot.commands), 0, "invalid command should not be added")
	})
}

func TestBot_HandleFunc(t *testing.T) {
	bot := &Bot{}
	called := false
	asserts.NoError(t, bot.HandleFunc("^hi$", func(ctx context.Context, b *Bot, m *Message) {
		called = true
	}), "HandleFunc with valid pattern")

	bot.dispatch(context.Background(), &Message{Content: "hi"})
	asserts.True(t, called, "HandleFunc handler should be called")

	asserts.Error(t, bot.HandleFunc("[bad(", noopHandler), "HandleFunc with invalid pattern should fail")
}

func TestBot_SetUnknownCommandHandler(t *testing.T) {
	bot := &Bot{}

	bot.SetUnknownCommandHandler(noopHandler)

	asserts.NotNil(t, bot.unknownCommandHandler, "UnknownCommandHandler should be set")
}

func TestBot_AddMiddleware(t *testing.T) {
	bot := &Bot{}

	bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
		next(ctx, b, m)
	})

	asserts.Equal(t, len(bot.middlewares), 1, "Number of middlewares after adding")
}

func TestBot_dispatch(t *testing.T) {
	t.Run("MatchingCommand", func(t *testing.T) {
		bot := &Bot{}
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.True(t, handlerCalled, "Handler should be called for matching command")
	})

	t.Run("NoMatchingCommand", func(t *testing.T) {
		bot := &Bot{}
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})
		unknownCalled := false
		bot.SetUnknownCommandHandler(func(ctx context.Context, b *Bot, m *Message) {
			unknownCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		asserts.False(t, handlerCalled, "Handler should not be called for non-matching command")
		asserts.True(t, unknownCalled, "Unknown handler should be called")
	})

	t.Run("WithMiddleware", func(t *testing.T) {
		bot := &Bot{}
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

		asserts.True(t, middlewareCalled, "Middleware should be called")
		asserts.True(t, handlerCalled, "Handler should be called after middleware")
	})

	t.Run("MultipleMiddlewares", func(t *testing.T) {
		bot := &Bot{}
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

		asserts.Equal(t, len(callOrder), 3, "Number of calls")
		asserts.Equal(t, callOrder[0], 1, "First middleware should be called first")
		asserts.Equal(t, callOrder[1], 2, "Second middleware should be called second")
		asserts.Equal(t, callOrder[2], 3, "Handler should be called last")
	})

	t.Run("MiddlewareShortCircuit", func(t *testing.T) {
		bot := &Bot{}
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			// Intentionally does not call next.
		})
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.False(t, handlerCalled, "Handler should not run when middleware short-circuits")
	})

	t.Run("NoUnknownHandler", func(t *testing.T) {
		bot := &Bot{}
		mustAddHandler(t, bot, "^hello$", noopHandler)

		// Should not panic without an unknown command handler.
		bot.dispatch(context.Background(), &Message{Content: "goodbye"})
	})

	t.Run("FirstMatchWins", func(t *testing.T) {
		bot := &Bot{}
		firstCalled, secondCalled := false, false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			firstCalled = true
		})
		mustAddHandler(t, bot, "^goodbye$", func(ctx context.Context, b *Bot, m *Message) {
			secondCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		asserts.False(t, firstCalled, "First handler should not be called")
		asserts.True(t, secondCalled, "Second handler should be called")
	})
}

func TestBot_dispatch_RecoversFromPanic(t *testing.T) {
	bot := &Bot{}
	mustAddHandler(t, bot, "^boom$", func(ctx context.Context, b *Bot, m *Message) {
		panic("handler exploded")
	})

	// dispatch must recover; the call should return normally rather than
	// propagating the panic and crashing the event loop.
	bot.dispatch(context.Background(), &Message{Content: "boom"})
}

// An unknown bot type has no adapter, so every lifecycle method reports
// ErrUnknownBotType rather than acting on a platform.

func TestBot_Connect_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.Connect(context.Background())

	asserts.ErrorIs(t, err, ErrUnknownBotType, "Connect with unknown bot type")
}

func TestBot_Disconnect_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.Disconnect()

	asserts.ErrorIs(t, err, ErrUnknownBotType, "Disconnect with unknown bot type")
}

func TestBot_SendMessage_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.SendMessage("channel123", "test message")

	asserts.ErrorIs(t, err, ErrUnknownBotType, "SendMessage with unknown bot type")
}

func TestBot_GetAttachments_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	attachments, err := bot.GetAttachments(&Message{})

	asserts.ErrorIs(t, err, ErrUnknownBotType, "GetAttachments with unknown bot type")
	asserts.Equal(t, len(attachments), 0, "Attachments should be empty for unknown bot type")
}
