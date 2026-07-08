package signal

import (
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
)

func TestNew(t *testing.T) {
	bot, err := New(Config{BaseURL: "http://127.0.0.1:8080", Number: "+15550001"})

	asserts.NoError(t, err, "New with full config should succeed")
	asserts.NotNil(t, bot, "bot should be initialized")
	asserts.Equal(t, bot.BotType, botbooter.SignalBotType, "bot type should be Signal")
}

func TestNew_MissingBaseURL(t *testing.T) {
	_, err := New(Config{})
	asserts.ErrorIs(t, err, ErrMissingConfig, "New without BaseURL should fail")
}

func TestRawMessage_NotSignal(t *testing.T) {
	_, ok := RawMessage(&botbooter.Message{Raw: 42})
	asserts.False(t, ok, "RawMessage should report non-signal Raw")
}
