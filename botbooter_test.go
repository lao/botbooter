package botbooter

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/slack-go/slack/slackevents"
)

// Test helpers to reduce duplication and improve readability

func assertEqual(t *testing.T, got, expected interface{}, message string) {
	t.Helper()
	if got != expected {
		t.Errorf("%s: got %v, expected %v", message, got, expected)
	}
}

func assertNotNil(t *testing.T, got interface{}, message string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected non-nil, got nil", message)
	}
}

func assertNil(t *testing.T, got interface{}, message string) {
	t.Helper()
	if got != nil {
		// Special handling for slices - check if empty
		switch v := got.(type) {
		case []Attachment:
			if len(v) != 0 {
				t.Errorf("%s: expected nil or empty slice, got %v", message, got)
			}
		default:
			t.Errorf("%s: expected nil, got %v", message, got)
		}
	}
}

func assertError(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error, got nil", message)
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

func TestBot_Connect(t *testing.T) {
	t.Run("SlackBot", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType
		done := make(chan error, 1)

		// Act
		go func() {
			done <- bot.Connect()
		}()

		// Assert
		// Give it a moment to try to connect (it will fail with fake token)
		time.Sleep(200 * time.Millisecond)
		// Note: This will fail to connect with fake credentials, but we're testing
		// that the connection flow works without panicking
	})

	t.Run("DiscordBot", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")

		// Act
		err := bot.Connect()

		// Assert
		assertError(t, err, "Connect with fake Discord token should fail")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		// Arrange
		bot := &Bot{
			BotType: BotType(999), // Invalid bot type
		}
		expectedError := "unknown bot type"

		// Act
		err := bot.Connect()

		// Assert
		assertError(t, err, "Connect with unknown bot type should fail")
		assertEqual(t, err.Error(), expectedError, "Error message for unknown bot type")
	})
}

func TestBot_Disconnect(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")

		// Act
		err := bot.Disconnect()

		// Assert
		assertNoError(t, err, "Disconnect Discord bot should not fail")
	})

	t.Run("SlackBot", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType

		// Act
		err := bot.Disconnect()

		// Assert
		assertNoError(t, err, "Disconnect Slack bot should not fail")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		// Arrange
		bot := &Bot{
			BotType: BotType(999), // Invalid bot type
		}
		expectedError := "unknown bot type"

		// Act
		err := bot.Disconnect()

		// Assert
		assertError(t, err, "Disconnect with unknown bot type should fail")
		assertEqual(t, err.Error(), expectedError, "Error message for unknown bot type")
	})
}

func TestBot_SendMessage(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		channelID := "channel123"
		message := "test message"

		// Act
		err := bot.SendMessage(channelID, message)

		// Assert
		// We expect an error because we're not actually connected
		assertError(t, err, "SendMessage without connection should fail")
	})

	t.Run("SlackBot", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType
		channelID := "channel123"
		message := "test message"

		// Act
		err := bot.SendMessage(channelID, message)

		// Assert
		// We expect an error because we're not actually connected
		assertError(t, err, "SendMessage without connection should fail")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		// Arrange
		bot := &Bot{
			BotType: BotType(999), // Invalid bot type
		}
		channelID := "channel123"
		message := "test message"
		expectedError := "unknown bot type"

		// Act
		err := bot.SendMessage(channelID, message)

		// Assert
		assertError(t, err, "SendMessage with unknown bot type should fail")
		assertEqual(t, err.Error(), expectedError, "Error message for unknown bot type")
	})
}

func TestBot_GetAttachments(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "test message",
			DiscordData: &discordgo.MessageCreate{
				Message: &discordgo.Message{
					Attachments: []*discordgo.MessageAttachment{
						{
							URL:    "https://example.com/image.png",
							Width:  100,
							Height: 100,
						},
					},
				},
			},
		}
		expectedURL := "https://example.com/image.png"

		// Act
		attachments, err := bot.GetAttachments(message)

		// Assert
		assertNoError(t, err, "GetAttachments for Discord bot should not fail")
		assertEqual(t, len(attachments), 1, "Number of attachments")
		assertTrue(t, attachments[0].IsImage, "Attachment should be an image")
		assertEqual(t, attachments[0].URL, expectedURL, "Attachment URL")
	})

	t.Run("SlackBot", func(t *testing.T) {
		// Arrange
		bot := InitAsSlackBot("xapp-test", "xoxb-test")
		bot.BotType = SlackBotType
		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "test message",
			SlackData: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{
						Mimetype:   "image/png",
						URLPrivate: "https://example.com/image.png",
					},
				},
			},
		}
		expectedURL := "https://example.com/image.png"

		// Act
		attachments, err := bot.GetAttachments(message)

		// Assert
		assertNoError(t, err, "GetAttachments for Slack bot should not fail")
		assertEqual(t, len(attachments), 1, "Number of attachments")
		assertTrue(t, attachments[0].IsImage, "Attachment should be an image")
		assertEqual(t, attachments[0].URL, expectedURL, "Attachment URL")
	})

	t.Run("UnknownBotType", func(t *testing.T) {
		// Arrange
		bot := &Bot{
			BotType: BotType(999), // Invalid bot type
		}
		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "test message",
		}
		expectedError := "unknown bot type"

		// Act
		attachments, err := bot.GetAttachments(message)

		// Assert
		assertError(t, err, "GetAttachments with unknown bot type should fail")
		assertNil(t, attachments, "Attachments should be nil for unknown bot type")
		assertEqual(t, err.Error(), expectedError, "Error message for unknown bot type")
	})
}

func TestBot_AddHandler(t *testing.T) {
	// Arrange
	bot := InitAsDiscordBot("test_token")
	handler := Command{
		Pattern: "^hello$",
		Handler: func(bot *Bot, message *Message) {},
	}
	initialCount := len(bot.Commands)
	expectedPattern := "^hello$"

	// Act
	bot.AddHandler(handler)

	// Assert
	assertEqual(t, len(bot.Commands), initialCount+1, "Number of commands after adding handler")
	assertEqual(t, bot.Commands[0].Pattern, expectedPattern, "Handler pattern")
}

func TestBot_SetUnknownCommandHandler(t *testing.T) {
	// Arrange
	bot := InitAsDiscordBot("test_token")
	handler := func(bot *Bot, message *Message) {}

	// Act
	bot.SetUnknownCommandHandler(handler)

	// Assert
	assertNotNil(t, bot.UnknownCommandHandler, "UnknownCommandHandler should be set")
}

func TestBot_AddMiddleware(t *testing.T) {
	// Arrange
	bot := InitAsDiscordBot("test_token")
	middleware := func(bot *Bot, message *Message, next CommandHandler) {
		next(bot, message)
	}
	initialCount := len(bot.Middlewares)

	// Act
	bot.AddMiddleware(middleware)

	// Assert
	assertEqual(t, len(bot.Middlewares), initialCount+1, "Number of middlewares after adding")
}

func TestBot_handleMessageWithCommand(t *testing.T) {
	t.Run("MatchingCommand", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		handlerCalled := false
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)
		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "hello",
		}

		// Act
		bot.handleMessageWithCommand(message)

		// Assert
		assertTrue(t, handlerCalled, "Handler should be called for matching command")
	})

	t.Run("NoMatchingCommand", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		handlerCalled := false
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)

		unknownHandlerCalled := false
		unknownHandler := func(bot *Bot, message *Message) {
			unknownHandlerCalled = true
		}
		bot.SetUnknownCommandHandler(unknownHandler)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "goodbye",
		}

		// Act
		bot.handleMessageWithCommand(message)

		// Assert
		assertFalse(t, handlerCalled, "Handler should not be called for non-matching command")
		assertTrue(t, unknownHandlerCalled, "Unknown handler should be called")
	})

	t.Run("WithMiddleware", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		middlewareCalled := false
		middleware := func(bot *Bot, message *Message, next CommandHandler) {
			middlewareCalled = true
			next(bot, message)
		}
		bot.AddMiddleware(middleware)

		handlerCalled := false
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				handlerCalled = true
			},
		}
		bot.AddHandler(handler)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "hello",
		}

		// Act
		bot.handleMessageWithCommand(message)

		// Assert
		assertTrue(t, middlewareCalled, "Middleware should be called")
		assertTrue(t, handlerCalled, "Handler should be called after middleware")
	})

	t.Run("MultipleMiddlewares", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		var callOrder []int

		middleware1 := func(bot *Bot, message *Message, next CommandHandler) {
			callOrder = append(callOrder, 1)
			next(bot, message)
		}
		middleware2 := func(bot *Bot, message *Message, next CommandHandler) {
			callOrder = append(callOrder, 2)
			next(bot, message)
		}

		bot.AddMiddleware(middleware1)
		bot.AddMiddleware(middleware2)

		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				callOrder = append(callOrder, 3)
			},
		}
		bot.AddHandler(handler)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "hello",
		}

		// Act
		bot.handleMessageWithCommand(message)

		// Assert
		assertEqual(t, len(callOrder), 3, "Number of calls")
		assertEqual(t, callOrder[0], 1, "First middleware should be called first")
		assertEqual(t, callOrder[1], 2, "Second middleware should be called second")
		assertEqual(t, callOrder[2], 3, "Handler should be called last")
	})

	t.Run("InvalidRegex", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		handler := Command{
			Pattern: "[invalid(", // Invalid regex
			Handler: func(bot *Bot, message *Message) {},
		}
		bot.AddHandler(handler)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "test",
		}

		// Act & Assert
		// Should not panic, just skip the invalid command
		bot.handleMessageWithCommand(message)
	})

	t.Run("NoUnknownHandler", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		handler := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {},
		}
		bot.AddHandler(handler)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "goodbye",
		}

		// Act & Assert
		// Should not panic even without an unknown command handler
		bot.handleMessageWithCommand(message)
	})

	t.Run("MultipleCommands", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		firstHandlerCalled := false
		secondHandlerCalled := false

		handler1 := Command{
			Pattern: "^hello$",
			Handler: func(bot *Bot, message *Message) {
				firstHandlerCalled = true
			},
		}

		handler2 := Command{
			Pattern: "^goodbye$",
			Handler: func(bot *Bot, message *Message) {
				secondHandlerCalled = true
			},
		}

		bot.AddHandler(handler1)
		bot.AddHandler(handler2)

		message := &Message{
			UserID:    "user123",
			ChannelID: "channel123",
			Content:   "goodbye",
		}

		// Act
		bot.handleMessageWithCommand(message)

		// Assert
		assertFalse(t, firstHandlerCalled, "First handler should not be called")
		assertTrue(t, secondHandlerCalled, "Second handler should be called")
	})
}

func TestBot_StartListening(t *testing.T) {
	t.Run("SuccessfulShutdown", func(t *testing.T) {
		// Arrange
		bot := InitAsDiscordBot("test_token")
		started := make(chan struct{})
		finished := make(chan struct{})

		// Act
		go func() {
			close(started)
			bot.StartListening()
			close(finished)
		}()

		// Wait for goroutine to start
		<-started
		time.Sleep(100 * time.Millisecond)

		// Send interrupt signal
		process, _ := os.FindProcess(os.Getpid())
		process.Signal(syscall.SIGTERM)

		// Assert
		select {
		case <-finished:
			// Successfully finished - bot shut down gracefully
		case <-time.After(1 * time.Second):
			t.Error("Timeout waiting for bot to shut down")
		}
	})

	t.Run("DisconnectError", func(t *testing.T) {
		// Arrange - Create a bot with invalid type that will cause disconnect to fail
		bot := &Bot{
			BotType: BotType(999), // Invalid bot type causes disconnect to fail
		}
		started := make(chan struct{})
		finished := make(chan struct{})

		// Act
		go func() {
			close(started)
			bot.StartListening()
			close(finished)
		}()

		// Wait for goroutine to start
		<-started
		time.Sleep(100 * time.Millisecond)

		// Send interrupt signal
		process, _ := os.FindProcess(os.Getpid())
		process.Signal(syscall.SIGTERM)

		// Assert
		select {
		case <-finished:
			// Successfully finished - error was logged but bot still shut down
		case <-time.After(1 * time.Second):
			t.Error("Timeout waiting for bot to shut down")
		}
	})
}
