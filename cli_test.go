package botbooter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe buffer: the CLI adapter writes to its output
// from a background goroutine while the test reads from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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

func TestInitAsCLIBot(t *testing.T) {
	t.Run("DefaultsToStdIO", func(t *testing.T) {
		bot := InitAsCLIBot(nil, nil)

		assertEqual(t, bot.BotType, CLIBotType, "bot type")
		assertNotNil(t, bot.cliIn, "default input should be set")
		assertNotNil(t, bot.cliOut, "default output should be set")
	})

	t.Run("CustomIO", func(t *testing.T) {
		bot := InitAsCLIBot(strings.NewReader("x"), &syncBuffer{})

		assertEqual(t, bot.BotType, CLIBotType, "bot type")
	})
}

func TestCLIBot_Run_ProcessesInput(t *testing.T) {
	in := strings.NewReader("echo hello\nping\n")
	out := &syncBuffer{}
	bot := InitAsCLIBot(in, out)

	mustAddHandler(t, bot, "^echo ", func(ctx context.Context, b *Bot, m *Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})
	unknown := 0
	bot.SetUnknownCommandHandler(func(ctx context.Context, b *Bot, m *Message) {
		unknown++
	})

	err := bot.Run(context.Background())

	assertNoError(t, err, "Run should return nil when input reaches EOF")
	assertEqual(t, out.String(), "hello\n", "echoed output")
	assertEqual(t, unknown, 1, "one unknown command (ping)")
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

func TestParseCLIMessage_Attachments(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "pic.png")
	writeFile(t, png, append(pngMagic, []byte("rest of image")...))
	txt := filepath.Join(dir, "notes.txt")
	writeFile(t, txt, []byte("just text"))

	msg := parseCLIMessage("echo " + png + " " + txt + " plainword")

	assertEqual(t, len(msg.Attachments), 2, "two file attachments (plainword ignored)")
	byURL := map[string]Attachment{}
	for _, a := range msg.Attachments {
		byURL[a.URL] = a
	}
	assertTrue(t, byURL[png].IsImage, "png should be detected as an image")
	assertFalse(t, byURL[txt].IsImage, "txt should not be an image")
}

func TestParseCLIMessage_NoFiles(t *testing.T) {
	msg := parseCLIMessage("echo hello world")

	assertEqual(t, len(msg.Attachments), 0, "plain words yield no attachments")
	assertEqual(t, msg.Text, "echo hello world", "text preserved")
}

func TestParseCLIMessage_DirectoryIgnored(t *testing.T) {
	msg := parseCLIMessage("look " + t.TempDir())

	assertEqual(t, len(msg.Attachments), 0, "directories are not attachments")
}

func TestCLIBot_GetAttachments_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "cat.png")
	writeFile(t, png, append(pngMagic, []byte("...")...))

	bot := InitAsCLIBot(strings.NewReader("echo "+png+"\n"), &syncBuffer{})
	var got []Attachment
	mustAddHandler(t, bot, "^echo ", func(ctx context.Context, b *Bot, m *Message) {
		atts, err := b.GetAttachments(m)
		assertNoError(t, err, "GetAttachments")
		got = atts
	})

	assertNoError(t, bot.Run(context.Background()), "Run")
	assertEqual(t, len(got), 1, "one attachment delivered through Run")
	assertTrue(t, got[0].IsImage, "image detected end-to-end")
}

func TestCLIBot_Run_PropagatesReadError(t *testing.T) {
	sentinel := errors.New("read failed")
	bot := InitAsCLIBot(errReader{err: sentinel}, &syncBuffer{})

	err := bot.Run(context.Background())

	assertErrorIs(t, err, sentinel, "Run should surface the input read error")
}

func TestCLIBot_Run_ContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	bot := InitAsCLIBot(pr, &syncBuffer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()

	cancel()

	select {
	case err := <-done:
		assertNoError(t, err, "Run should return after context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
