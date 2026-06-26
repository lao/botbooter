package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func TestNew_BotType(t *testing.T) {
	bot := New(strings.NewReader("x"), io.Discard)

	asserts.Equal(t, bot.BotType, core.CLIBotType, "bot type")
}

func TestNewAdapter_DefaultsToStdIO(t *testing.T) {
	a := newAdapter(nil, nil)

	asserts.NotNil(t, a.in, "default input should be set")
	asserts.NotNil(t, a.out, "default output should be set")
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseMessage_Attachments(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "pic.png")
	writeFile(t, png, append(pngMagic, []byte("rest of image")...))
	txt := filepath.Join(dir, "notes.txt")
	writeFile(t, txt, []byte("just text"))

	msg := parseMessage("echo " + png + " " + txt + " plainword")

	asserts.Equal(t, len(msg.Attachments), 2, "two file attachments (plainword ignored)")
	byURL := map[string]core.Attachment{}
	for _, a := range msg.Attachments {
		byURL[a.URL] = a
	}
	asserts.True(t, byURL[png].IsImage, "png should be detected as an image")
	asserts.False(t, byURL[txt].IsImage, "txt should not be an image")
}

func TestParseMessage_NoFiles(t *testing.T) {
	msg := parseMessage("echo hello world")

	asserts.Equal(t, len(msg.Attachments), 0, "plain words yield no attachments")
	asserts.Equal(t, msg.Text, "echo hello world", "text preserved")
}

func TestParseMessage_DirectoryIgnored(t *testing.T) {
	msg := parseMessage("look " + t.TempDir())

	asserts.Equal(t, len(msg.Attachments), 0, "directories are not attachments")
}
