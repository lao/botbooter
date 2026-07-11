// Package whatsmeow is the WhatsApp Web flavor of botbooter's WhatsApp
// support, built on the whatsmeow library (go.mau.fi/whatsmeow). It links to a
// phone by QR code exactly like WhatsApp Web, holds a persistent websocket to
// WhatsApp's servers, and persists the linked session in a local SQLite
// database — no Meta Business account, webhook server or public HTTPS URL is
// required. The trade-off versus the Cloud API flavor (botbooter/whatsapp/cloud)
// is that it drives a personal/linked WhatsApp account over the unofficial Web
// protocol rather than the official Business platform.
//
// On first run the session store is empty: Connect streams pairing QR codes
// through Config.QRCallback (by default a scannable QR is printed to stderr);
// scan one from WhatsApp > Linked devices. Once linked, later runs reuse the
// stored session. Incoming media is end-to-end encrypted, so attachments carry
// no URL — fetch the bytes with [Download].
package whatsmeow

import (
	"context"

	wm "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/lao/botbooter"
	wmint "github.com/lao/botbooter/internal/whatsapp/whatsmeow"
)

// Config configures a whatsmeow-backed WhatsApp bot. The zero value is usable:
// it stores the session in "botbooter-whatsapp-meow.db" in the working directory and
// prints pairing QR codes to stderr.
type Config = wmint.Config

// ErrLoggedOut is reported through Run when the linked session is invalidated
// (for example, unlinked from the phone). Reconnecting cannot recover from it,
// so the session must be re-paired. Check for it with errors.Is.
var ErrLoggedOut = wmint.ErrLoggedOut

// ErrNotDownloadable is returned by [Download] when the attachment does not
// carry a whatsmeow downloadable media payload.
var ErrNotDownloadable = wmint.ErrNotDownloadable

// New creates a WhatsApp bot that speaks the WhatsApp Web multidevice protocol
// via whatsmeow. It opens (or reuses) the session store and builds the client,
// returning an error if the store cannot be opened; the websocket is not dialed
// until the bot connects.
//
// When New opened the store itself (neither Config.Client nor Config.Container
// set), the bot is single-run: shutdown closes the store, so construct a fresh
// bot for each run instead of reconnecting the same one.
func New(cfg Config) (*botbooter.Bot, error) {
	return wmint.New(cfg)
}

// RawMessage returns the raw whatsmeow message event carried on m, reporting
// whether m originated from the WhatsApp Web (whatsmeow) flavor.
func RawMessage(m *botbooter.Message) (*events.Message, bool) {
	return wmint.RawMessage(m)
}

// RawReaction returns the raw whatsmeow message event carrying the reaction on
// r, reporting whether r originated from the WhatsApp Web (whatsmeow) flavor.
// Its Message.GetReactionMessage() holds the reaction payload.
func RawReaction(r *botbooter.Reaction) (*events.Message, bool) {
	return wmint.RawReaction(r)
}

// Client returns the whatsmeow client backing b, or nil if b is not a WhatsApp
// Web (whatsmeow) bot. Use it for platform-specific operations the portable API
// does not cover.
func Client(b *botbooter.Bot) *wm.Client {
	return wmint.Client(b)
}

// Download fetches and decrypts the end-to-end-encrypted bytes of att, which
// must come from a whatsmeow bot's attachments. It returns
// [botbooter.ErrUnknownBotType] if b is not a WhatsApp Web (whatsmeow) bot and
// [ErrNotDownloadable] if att carries no downloadable media.
func Download(ctx context.Context, b *botbooter.Bot, att botbooter.Attachment) ([]byte, error) {
	return wmint.Download(ctx, b, att)
}
