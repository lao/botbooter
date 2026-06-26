package botbooter_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/slack-go/slack/slackevents"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

// syncBuffer is a concurrency-safe buffer: the CLI adapter writes to its output
// from a background goroutine while the test reads from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// emptyReader is an io.Reader that is immediately at EOF.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// errReader is an io.Reader that always fails with a fixed error.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// newDiscordBot constructs a Discord bot for tests, failing the test if
// construction errors.
func newDiscordBot(t *testing.T) *botbooter.Bot {
	t.Helper()
	bot, err := botbooter.InitAsDiscordBot("test_token")
	asserts.NoError(t, err, "InitAsDiscordBot")
	asserts.NotNil(t, bot, "bot should be initialized")
	return bot
}

func mustAddHandler(t *testing.T, bot *botbooter.Bot, pattern string, handler botbooter.CommandHandler) {
	t.Helper()
	asserts.NoError(t, bot.AddHandler(botbooter.Command{Pattern: pattern, Handler: handler}), "AddHandler "+pattern)
}

// pngMagic is the 8-byte PNG signature; http.DetectContentType reports
// image/png for any content beginning with it.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBot_Connect(t *testing.T) {
	t.Run("DiscordBotWithFakeToken", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.Connect(context.Background())

		asserts.Error(t, err, "Connect with fake Discord token should fail")
	})

	t.Run("AlreadyConnected", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		bot := botbooter.InitAsCLIBot(emptyReader{}, &syncBuffer{})

		asserts.NoError(t, bot.Connect(ctx), "first Connect")
		err := bot.Connect(ctx)
		asserts.ErrorIs(t, err, botbooter.ErrAlreadyConnected, "second Connect")

		asserts.NoError(t, bot.Disconnect(), "Disconnect")
	})
}

func TestBot_Disconnect(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.Disconnect()

		asserts.NoError(t, err, "Disconnect Discord bot should not fail")
	})

	t.Run("SlackBot", func(t *testing.T) {
		bot := botbooter.InitAsSlackBot("xapp-test", "xoxb-test")

		err := bot.Disconnect()

		asserts.NoError(t, err, "Disconnect Slack bot should not fail")
	})
}

func TestBot_SendMessage(t *testing.T) {
	t.Run("DiscordBotNotConnected", func(t *testing.T) {
		bot := newDiscordBot(t)

		err := bot.SendMessage("channel123", "test message")

		asserts.Error(t, err, "SendMessage without connection should fail")
	})

	t.Run("SlackBotNotConnected", func(t *testing.T) {
		bot := botbooter.InitAsSlackBot("xapp-test", "xoxb-test")

		err := bot.SendMessage("channel123", "test message")

		asserts.Error(t, err, "SendMessage without connection should fail")
	})

	t.Run("CLIBot", func(t *testing.T) {
		out := &syncBuffer{}
		bot := botbooter.InitAsCLIBot(emptyReader{}, out)

		err := bot.SendMessage("ignored", "hello world")

		asserts.NoError(t, err, "CLI SendMessage should not fail")
		asserts.Equal(t, out.String(), "hello world\n", "CLI output")
	})
}

func TestBot_GetAttachments(t *testing.T) {
	t.Run("DiscordBot", func(t *testing.T) {
		bot := newDiscordBot(t)
		message := &botbooter.Message{
			DiscordData: &discordgo.MessageCreate{
				Message: &discordgo.Message{
					Attachments: []*discordgo.MessageAttachment{
						{URL: "https://example.com/image.png", Width: 100, Height: 100},
					},
				},
			},
		}

		attachments, err := bot.GetAttachments(message)

		asserts.NoError(t, err, "GetAttachments for Discord bot should not fail")
		asserts.Equal(t, len(attachments), 1, "Number of attachments")
		asserts.True(t, attachments[0].IsImage, "Attachment should be an image")
		asserts.Equal(t, attachments[0].URL, "https://example.com/image.png", "Attachment URL")
	})

	t.Run("DiscordBotNilData", func(t *testing.T) {
		bot := newDiscordBot(t)

		attachments, err := bot.GetAttachments(&botbooter.Message{})

		asserts.NoError(t, err, "GetAttachments with nil Discord data should not fail")
		asserts.Equal(t, len(attachments), 0, "no attachments")
	})

	t.Run("SlackBot", func(t *testing.T) {
		bot := botbooter.InitAsSlackBot("xapp-test", "xoxb-test")
		message := &botbooter.Message{
			SlackData: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{Mimetype: "image/png", URLPrivate: "https://example.com/image.png"},
				},
			},
		}

		attachments, err := bot.GetAttachments(message)

		asserts.NoError(t, err, "GetAttachments for Slack bot should not fail")
		asserts.Equal(t, len(attachments), 1, "Number of attachments")
		asserts.True(t, attachments[0].IsImage, "Attachment should be an image")
		asserts.Equal(t, attachments[0].URL, "https://example.com/image.png", "Attachment URL")
	})

	t.Run("CLIBot", func(t *testing.T) {
		bot := botbooter.InitAsCLIBot(emptyReader{}, &syncBuffer{})

		attachments, err := bot.GetAttachments(&botbooter.Message{Content: "hi"})

		asserts.NoError(t, err, "GetAttachments for CLI bot should not fail")
		asserts.Equal(t, len(attachments), 0, "CLI has no attachments")
	})
}

func TestCLIBot_Run_ProcessesInput(t *testing.T) {
	in := strings.NewReader("echo hello\nping\n")
	out := &syncBuffer{}
	bot := botbooter.InitAsCLIBot(in, out)

	mustAddHandler(t, bot, "^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})
	unknown := 0
	bot.SetUnknownCommandHandler(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		unknown++
	})

	err := bot.Run(context.Background())

	asserts.NoError(t, err, "Run should return nil when input reaches EOF")
	asserts.Equal(t, out.String(), "hello\n", "echoed output")
	asserts.Equal(t, unknown, 1, "one unknown command (ping)")
}

func TestCLIBot_GetAttachments_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "cat.png")
	writeFile(t, png, append(pngMagic, []byte("...")...))

	bot := botbooter.InitAsCLIBot(strings.NewReader("echo "+png+"\n"), &syncBuffer{})
	var got []botbooter.Attachment
	mustAddHandler(t, bot, "^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		atts, err := b.GetAttachments(m)
		asserts.NoError(t, err, "GetAttachments")
		got = atts
	})

	asserts.NoError(t, bot.Run(context.Background()), "Run")
	asserts.Equal(t, len(got), 1, "one attachment delivered through Run")
	asserts.True(t, got[0].IsImage, "image detected end-to-end")
}

func TestCLIBot_Run_PropagatesReadError(t *testing.T) {
	sentinel := errors.New("read failed")
	bot := botbooter.InitAsCLIBot(errReader{err: sentinel}, &syncBuffer{})

	err := bot.Run(context.Background())

	asserts.ErrorIs(t, err, sentinel, "Run should surface the input read error")
}

func TestCLIBot_Run_ContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	bot := botbooter.InitAsCLIBot(pr, &syncBuffer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		asserts.NoError(t, err, "Run should return after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestConnectSlack_StartsAndStops(t *testing.T) {
	if os.Getenv("BOTBOOTER_SLACK_NETWORK_TEST") == "" {
		t.Skip("set BOTBOOTER_SLACK_NETWORK_TEST=1 to run; performs real Slack network I/O via RunContext")
	}
	bot := botbooter.InitAsSlackBot("xapp-test", "xoxb-test")
	ctx, cancel := context.WithCancel(context.Background())

	asserts.NoError(t, bot.Connect(ctx), "Connect Slack should start the loop")

	cancel()
	asserts.NoError(t, bot.Disconnect(), "Disconnect Slack should not fail")
}

func TestBot_ReconnectAndConcurrentDisconnect(t *testing.T) {
	// A pipe that never reaches EOF keeps the CLI reader blocked so the
	// connection stays alive until we disconnect it.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	bot := botbooter.InitAsCLIBot(pr, &syncBuffer{})

	for i := 0; i < 20; i++ {
		// Reconnecting after a clean disconnect must succeed: each Connect
		// installs its own stop closure rather than reusing a shared one.
		asserts.NoError(t, bot.Connect(context.Background()), "reconnect should succeed after disconnect")

		// Several goroutines race to tear down the same connection. Only one
		// performs the teardown, and the per-connection stop state must be
		// accessed without a data race (verified under go test -race).
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = bot.Disconnect()
			}()
		}
		wg.Wait()
	}
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
	bot := botbooter.InitAsCLIBot(pr, &syncBuffer{})

	done := make(chan error, 1)
	go func() { done <- bot.Start() }()

	// Give Start time to register the signal handler before we signal.
	time.Sleep(150 * time.Millisecond)
	proc, err := os.FindProcess(os.Getpid())
	asserts.NoError(t, err, "FindProcess")
	asserts.NoError(t, proc.Signal(syscall.SIGTERM), "send SIGTERM")

	select {
	case err := <-done:
		asserts.NoError(t, err, "Start should return after SIGTERM")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not shut down after SIGTERM")
	}
}
