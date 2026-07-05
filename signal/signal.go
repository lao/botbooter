// Package signal exposes the Signal constructor, the raw-message accessor, and
// the Config/Message/Attachment types for botbooter. Import it for a Signal
// bot; the adapter speaks JSON-RPC 2.0 to a signal-cli daemon
// (`signal-cli daemon --tcp <addr>`) over plain TCP, so a Signal-only binary
// pulls in no third-party platform SDK — it never compiles discordgo, slack-go
// or go-telegram. The daemon socket is unauthenticated: bind it to localhost
// or a private network only.
package signal

import (
	"github.com/lao/botbooter"
	sigint "github.com/lao/botbooter/internal/signal"
)

// Config configures a Signal bot backed by a signal-cli daemon.
type Config = sigint.Config

// Message is the parsed payload of a signal-cli receive envelope.
type Message = sigint.Message

// Attachment is a file attached to a Signal message, delivered by id.
type Attachment = sigint.Attachment

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = sigint.ErrMissingConfig

// ErrNotConnected is returned by Send when the bot has no live daemon connection.
var ErrNotConnected = sigint.ErrNotConnected

// New creates a Signal bot that talks to the signal-cli daemon at cfg.Address.
// The daemon is dialed when the bot connects, not here. It returns
// [ErrMissingConfig] if a required config field is missing.
func New(cfg Config) (*botbooter.Bot, error) {
	return sigint.New(cfg)
}

// RawMessage returns the parsed Signal message carried on m, reporting whether
// m originated from Signal.
func RawMessage(m *botbooter.Message) (*Message, bool) {
	return sigint.RawMessage(m)
}
