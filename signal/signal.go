// Package signal exposes the Signal constructor, the raw-message accessor, and
// the Config/Message/Attachment types for botbooter. Import it for a Signal
// bot; the adapter talks to a signal-cli-rest-api container
// (https://github.com/bbernhard/signal-cli-rest-api) running in json-rpc mode
// — inbound messages over the container's receive WebSocket, replies over
// plain REST — so a Signal-only binary pulls in no platform SDK (only the
// gorilla/websocket transport library). The container's API is
// unauthenticated: bind it to localhost or a private network only.
//
// The adapter does not reconnect: losing the receive socket — a container
// restart, say — ends [botbooter.Bot.Run] with an error, unlike the Slack,
// Discord, Telegram and whatsmeow adapters, whose SDKs re-dial internally. Run
// the bot under a supervisor that restarts the process, or re-Run it yourself.
//
// The package name collides with the standard library's os/signal, so a file
// that needs both must alias one of them (see _examples/reactions/main.go).
package signal

import (
	"github.com/lao/botbooter"
	sigint "github.com/lao/botbooter/internal/signal"
)

// Config configures a Signal bot backed by a signal-cli-rest-api container.
type Config = sigint.Config

// Message is the parsed payload of a signal-cli receive envelope.
type Message = sigint.Message

// Attachment is a file attached to a Signal message, delivered by id and
// served by the container at /v1/attachments/{id}.
type Attachment = sigint.Attachment

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = sigint.ErrMissingConfig

// New creates a Signal bot that talks to the signal-cli-rest-api container at
// cfg.BaseURL. The container is dialed when the bot connects, not here. It
// returns [ErrMissingConfig] if a required config field is missing.
func New(cfg Config) (*botbooter.Bot, error) {
	return sigint.New(cfg)
}

// RawMessage returns the parsed Signal message carried on m, reporting whether
// m originated from Signal.
func RawMessage(m *botbooter.Message) (*Message, bool) {
	return sigint.RawMessage(m)
}
