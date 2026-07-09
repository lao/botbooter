// Package cloud is the Meta Cloud API flavor of botbooter's WhatsApp support:
// it exposes the webhook-based constructor, the raw-message accessor, and the
// Config/Message/Media types. Import it for a WhatsApp bot backed by a Meta
// Business account; the adapter speaks the Cloud API over plain HTTP, so a
// WhatsApp-only binary pulls in no third-party platform SDK — it never compiles
// discordgo, slack-go, go-telegram or whatsmeow. For the QR-linked WhatsApp Web
// flavor (no Meta account or webhook needed), see botbooter/whatsapp/whatsmeow.
package cloud

import (
	"github.com/lao/botbooter"
	waint "github.com/lao/botbooter/internal/whatsapp/cloud"
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

// Addr returns the address b's webhook listener is bound to (host:port), or ""
// if b is not a WhatsApp bot or is not connected. Use it to recover the
// OS-assigned port after passing cfg.Addr ":0".
func Addr(b *botbooter.Bot) string {
	return waint.Addr(b)
}
