package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot := New(strings.NewReader(""), io.Discard)
	asserts.Equal(t, bot.BotType, botbooter.CLIBotType, "bot type")
}

func TestRawData(t *testing.T) {
	want := &botbooter.CLIMessage{Text: "hi"}
	got, ok := RawData(&botbooter.Message{Raw: want})
	asserts.True(t, ok, "raw present")
	asserts.Equal(t, got, want, "raw data")

	_, ok = RawData(&botbooter.Message{})
	asserts.False(t, ok, "no raw on bare message")
}
