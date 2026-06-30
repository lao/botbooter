// Package whatsapp exposes the WhatsApp (Meta Cloud API) constructor, the
// raw-message accessor, and the Config/Message/Media types for botbooter. Import
// it for a WhatsApp bot; the adapter speaks the Cloud API over plain HTTP, so a
// WhatsApp-only binary pulls in no third-party platform SDK — it never compiles
// discordgo, slack-go or go-telegram.
package whatsapp

import (
	"github.com/lao/botbooter"
	waint "github.com/lao/botbooter/internal/whatsapp"
)

// Config configures a WhatsApp Cloud API bot.
type Config = waint.Config

// Message is the parsed payload of a WhatsApp webhook message.
type Message = waint.Message

// Media is a media object attached to a WhatsApp message.
type Media = waint.Media

// ErrMissingConfig is returned by [New] when a required [Config] field is empty.
var ErrMissingConfig = waint.ErrMissingConfig

// New creates a WhatsApp bot backed by the Meta Cloud API. It runs an inbound
// webhook HTTP server at cfg.Addr, so put a TLS-terminating proxy in front and
// register the public HTTPS URL in Meta's webhook settings. It returns
// [ErrMissingConfig] if a required config field is missing.
func New(cfg Config) (*botbooter.Bot, error) {
	return waint.New(cfg)
}

// RawMessage returns the parsed WhatsApp message carried on m, reporting whether
// m originated from WhatsApp.
func RawMessage(m *botbooter.Message) (*Message, bool) {
	return waint.RawMessage(m)
}
