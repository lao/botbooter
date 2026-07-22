// Package teams exposes the Microsoft Teams (Azure Bot Framework) constructor,
// the raw-message accessor, and the Config/Message types for botbooter. Import
// it for a Teams bot; the adapter speaks the Bot Connector REST API over plain
// HTTP, so a Teams-only binary pulls in no third-party platform SDK — it never
// compiles discordgo, slack-go or go-telegram.
package teams

import (
	"github.com/lao/botbooter"
	teamsint "github.com/lao/botbooter/internal/teams"
)

// Config configures a Microsoft Teams (Azure Bot Framework) bot.
type Config = teamsint.Config

// Message is the parsed payload of an inbound Bot Framework Activity.
type Message = teamsint.Message

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = teamsint.ErrMissingConfig

// ErrUnknownConversation is returned by a Teams bot's Send when the target
// conversation has not been seen on an inbound Activity (or was evicted), so no
// serviceUrl is known. Branch it with errors.Is.
var ErrUnknownConversation = teamsint.ErrUnknownConversation

// New creates a Microsoft Teams bot backed by the Azure Bot Framework. It runs
// an inbound webhook HTTP server at cfg.Addr, so put a TLS-terminating proxy in
// front and register the public HTTPS URL as your Azure Bot resource's messaging
// endpoint. It returns [ErrMissingConfig] if a required config field is missing.
func New(cfg Config) (*botbooter.Bot, error) {
	return teamsint.New(cfg)
}

// RawMessage returns the parsed Teams message carried on m, reporting whether m
// originated from Teams.
func RawMessage(m *botbooter.Message) (*Message, bool) {
	return teamsint.RawMessage(m)
}

// Addr returns the address b's webhook listener is bound to (host:port), or ""
// if b is not a Teams bot or is not connected. Use it to recover the OS-assigned
// port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return teamsint.Addr(b)
}
