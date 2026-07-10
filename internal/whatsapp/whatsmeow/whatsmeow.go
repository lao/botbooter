// Package whatsmeow is the WhatsApp Web flavor of botbooter's WhatsApp adapter,
// built on the whatsmeow library (go.mau.fi/whatsmeow). It implements
// core.Adapter. (The Meta Cloud API webhook flavor lives in the sibling
// internal/whatsapp/cloud package.)
//
// Unlike the Cloud API flavor, it speaks the WhatsApp Web multidevice protocol
// directly: it links to a phone by QR code (exactly like WhatsApp Web), holds a
// persistent websocket to WhatsApp's servers, and persists the linked session in
// a local SQLite database. No Meta Business account, webhook server or public
// HTTPS URL is required.
//
// The adapter mirrors the dial-out Discord adapter: Connect opens (or first
// links) the session and registers an event handler, the session runs in the
// background until the run context is canceled, and Disconnect tears it down.
// On first run the device store is empty, so Connect surfaces pairing QR codes
// through Config.QRCallback (which by default prints a scannable QR to stderr);
// once linked, the session is reused from the store on subsequent runs.
package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/mdp/qrterminal/v3"
	wm "go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver

	"github.com/lao/botbooter/internal/core"
)

const (
	defaultDBPath = "botbooter-whatsapp-meow.db"
	// sqliteDialect is both the database/sql driver name (registered by the
	// blank import of modernc.org/sqlite) and the dialect whatsmeow's dbutil
	// resolves from the "sqlite" prefix.
	sqliteDialect = "sqlite"
	// dsnFormat enables foreign keys and a busy timeout using modernc's pragma
	// query syntax (mattn's "?_foreign_keys=on" form is not understood).
	dsnFormat = "file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
)

// ErrLoggedOut is reported through Run when the linked session is invalidated
// (for example, unlinked from the phone). Reconnecting cannot recover from it,
// so the session must be re-paired. Check for it with errors.Is.
var ErrLoggedOut = errors.New("whatsmeow: logged out, session must be re-linked")

// ErrNotDownloadable is returned by Download when the attachment does not carry
// a whatsmeow downloadable media payload.
var ErrNotDownloadable = errors.New("whatsmeow: attachment is not downloadable media")

// Config configures a whatsmeow-backed WhatsApp bot. The zero value is usable:
// it stores the session in "botbooter-whatsapp-meow.db" in the working directory and
// prints pairing QR codes to stderr.
type Config struct {
	// DBPath is the path to the SQLite file holding the linked session and
	// crypto keys; it is created if missing. Defaults to
	// "botbooter-whatsapp-meow.db". Ignored when Client or Container is set.
	DBPath string

	// QRCallback is invoked with each raw pairing code emitted during first-time
	// linking; render it as a QR for the user to scan from WhatsApp > Linked
	// devices. When nil, a scannable QR is printed to stderr.
	QRCallback func(code string)

	// Logger is the whatsmeow logger for the client and store. When nil, a no-op
	// logger is used; pass waLog.Stdout("Client", "INFO", true) to debug.
	Logger waLog.Logger

	// Client, when set, is used as-is and DBPath, Container and Logger are
	// ignored. Use it to bring a fully configured whatsmeow client.
	Client *wm.Client

	// Container, when set, supplies the session store (for example Postgres or a
	// shared SQLite container) instead of opening DBPath; its first device is
	// used. Ignored when Client is set.
	Container *sqlstore.Container
}

// adapter is the whatsmeow implementation of core.Adapter.
type adapter struct {
	client     *wm.Client
	qrCallback func(code string)

	mu         sync.Mutex
	handlerID  uint32
	handlerSet bool
	// ownContainer is non-nil only when New opened the SQLite store itself, in
	// which case Disconnect closes it. A caller-supplied Container or Client is
	// left for the caller to close.
	ownContainer *sqlstore.Container
}

// New creates a WhatsApp Web bot from cfg. It opens (or reuses) the session
// store and builds a whatsmeow client, returning an error if the store cannot
// be opened. The websocket is not dialed until the bot connects.
//
// When New opened the store itself (neither Client nor Container set), the bot
// is single-run: Disconnect — which Run performs on shutdown — closes the
// store, so construct a fresh bot for each run instead of reconnecting.
func New(cfg Config) (*core.Bot, error) {
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return core.New(core.WhatsMeowBotType, a), nil
}

// newAdapter resolves cfg into a whatsmeow client and applies defaults. When
// neither Client nor Container is supplied it opens a pure-Go SQLite store at
// DBPath and uses its first device.
func newAdapter(cfg Config) (*adapter, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = waLog.Noop
	}

	qr := cfg.QRCallback
	if qr == nil {
		qr = func(code string) { qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stderr) }
	}

	if cfg.Client != nil {
		return &adapter{client: cfg.Client, qrCallback: qr}, nil
	}

	container := cfg.Container
	var ownContainer *sqlstore.Container
	var ownDBPath string
	if container == nil {
		ownDBPath = cfg.DBPath
		if ownDBPath == "" {
			ownDBPath = defaultDBPath
		}
		c, err := sqlstore.New(context.Background(), sqliteDialect, fmt.Sprintf(dsnFormat, ownDBPath), logger)
		if err != nil {
			return nil, fmt.Errorf("whatsmeow: open store: %w", err)
		}
		container = c
		ownContainer = c
	}

	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if ownContainer != nil {
			_ = ownContainer.Close()
		}
		return nil, fmt.Errorf("whatsmeow: get device: %w", err)
	}

	// The store we created holds the linked session and crypto keys — a
	// credential. Restrict it (and its journal sidecars) to the owner. Best
	// effort: SQLite's default 0644 is the concern, but a chmod failure must not
	// block startup.
	if ownDBPath != "" {
		for _, p := range []string{ownDBPath, ownDBPath + "-wal", ownDBPath + "-shm"} {
			_ = os.Chmod(p, 0o600)
		}
	}

	return &adapter{
		client:       wm.NewClient(device, logger),
		qrCallback:   qr,
		ownContainer: ownContainer,
	}, nil
}

// Connect registers the event handler and dials the websocket, returning once
// the connection is established. A never-linked device (empty store) is paired
// first: QR codes are streamed to the callback while the session links. The
// client then runs in the background until ctx is canceled, mirroring Discord.
func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// whatsmeow can deliver more than one terminal event for a single connection
	// (a pairing failure and a logout can both fire, and both the event handler
	// and the QR channel observe a logout during pairing), but core's run loop
	// reads the connection-ended signal exactly once — a surplus send would block
	// a whatsmeow dispatch goroutine. Collapse them with a fresh per-connection
	// once-guard, mirroring core's own per-connection teardown Once.
	var doneOnce sync.Once
	reportDone := func(err error) { doneOnce.Do(func() { deps.Done(err) }) }

	id := a.client.AddEventHandler(func(evt any) { a.onEvent(ctx, evt, deps, reportDone) })
	a.mu.Lock()
	a.handlerID = id
	a.handlerSet = true
	a.mu.Unlock()

	// A device with no stored ID has never been linked; the QR channel must be
	// acquired before Connect so the pairing codes are not missed.
	var qrChan <-chan wm.QRChannelItem
	if a.client.Store.ID == nil {
		ch, err := a.client.GetQRChannel(ctx)
		if err != nil {
			a.removeHandler()
			return fmt.Errorf("whatsmeow: qr channel: %w", err)
		}
		qrChan = ch
	}
	if err := a.client.Connect(); err != nil {
		a.removeHandler()
		return err
	}
	if qrChan != nil {
		go a.pumpQR(qrChan, reportDone)
	}

	go func() {
		<-ctx.Done()
		_ = deps.Disconnect()
	}()

	return nil
}

// pumpQR forwards pairing codes to the callback and reports a terminal pairing
// outcome through reportDone. whatsmeow closes the channel once pairing
// succeeds, errors or times out.
func (a *adapter) pumpQR(qrChan <-chan wm.QRChannelItem, reportDone func(error)) {
	for item := range qrChan {
		switch {
		case item.Event == wm.QRChannelEventCode:
			a.qrCallback(item.Code)
		case item == wm.QRChannelScannedWithoutMultidevice:
			// Non-terminal: the phone scanned with multidevice disabled. The
			// channel stays open and emits a fresh code, so keep waiting rather
			// than ending the run loop.
			continue
		case item == wm.QRChannelSuccess:
			return // linked; the client stays connected
		case item.Event == wm.QRChannelEventError:
			reportDone(fmt.Errorf("whatsmeow: pairing failed: %w", item.Error))
			return
		default:
			// timeout, client-outdated, unexpected-state: all terminal.
			reportDone(fmt.Errorf("whatsmeow: pairing did not complete: %s", item.Event))
			return
		}
	}
}

// onEvent handles the whatsmeow events the adapter cares about: incoming
// messages are dispatched, and a logout ends the run loop via reportDone.
func (a *adapter) onEvent(ctx context.Context, evt any, deps core.AdapterDeps, reportDone func(error)) {
	switch v := evt.(type) {
	case *events.Message:
		a.onMessage(ctx, v, deps)
	case *events.LoggedOut:
		reportDone(ErrLoggedOut)
	}
}

// onMessage converts an incoming message into a platform-agnostic Message and
// dispatches it. It drops the bot's own messages (reply-loop guard), broadcast
// and status traffic, and non-conversational events (reactions, receipts,
// revokes, poll updates) that carry neither text nor media.
func (a *adapter) onMessage(ctx context.Context, v *events.Message, deps core.AdapterDeps) {
	if v.Info.IsFromMe || v.Info.Chat.Server == types.BroadcastServer {
		return
	}
	text := messageText(v)
	if _, hasMedia := mediaAttachment(v.Message); text == "" && !hasMedia {
		return
	}
	deps.Dispatch(ctx, &core.Message{
		ID:         v.Info.ID,
		UserID:     v.Info.Sender.String(),
		AuthorName: v.Info.PushName,
		ChannelID:  v.Info.Chat.String(),
		Content:    text,
		Timestamp:  v.Info.Timestamp,
		Raw:        v,
	})
}

// messageText extracts the readable text of a message: a plain conversation
// body, an extended-text body, or a media caption, in that order. It returns ""
// when the message carries no text.
func messageText(v *events.Message) string {
	msg := v.Message
	if msg == nil {
		return ""
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption()
	default:
		return ""
	}
}

// Disconnect removes the event handler, closes the websocket and, when New
// opened the SQLite store, closes it. Closing the store makes the bot
// single-run — a later Connect would find the device store closed — so build a
// fresh bot with New instead of reconnecting this one. It is safe to call when
// never connected and is idempotent.
func (a *adapter) Disconnect() error {
	a.removeHandler()
	a.client.Disconnect()

	a.mu.Lock()
	container := a.ownContainer
	a.ownContainer = nil
	a.mu.Unlock()
	if container != nil {
		return container.Close()
	}
	return nil
}

// removeHandler unregisters the event handler exactly once.
func (a *adapter) removeHandler() {
	a.mu.Lock()
	id, set := a.handlerID, a.handlerSet
	a.handlerSet = false
	a.mu.Unlock()
	if set {
		a.client.RemoveEventHandler(id)
	}
}

// Send delivers a plain-text message to channelID, which must be a WhatsApp JID
// (for example "123456789@s.whatsapp.net" or a group JID). The chat JID of an
// incoming Message is in Message.ChannelID, so handlers can reply with it.
// Threading options are ignored: WhatsApp chats are flat, so like the CLI
// adapter a reply is just a plain send (quoted replies are a possible
// follow-up).
func (a *adapter) Send(ctx context.Context, channelID, text string, _ core.SendOptions) error {
	jid, err := types.ParseJID(channelID)
	if err != nil {
		return fmt.Errorf("whatsmeow: invalid channel id %q: %w", channelID, err)
	}
	_, err = a.client.SendMessage(ctx, jid, &waProto.Message{Conversation: proto.String(text)})
	return err
}

// Attachments returns the media attached to the message's WhatsApp event. The
// bytes are end-to-end encrypted and delivered out of band, so Attachment.URL
// is empty and ExtraData holds the media-specific protobuf (for example
// *waE2E.ImageMessage), which implements whatsmeow.DownloadableMessage.
// Download the bytes with [Download] (or Client(b).Download directly).
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	v, ok := RawMessage(m)
	if !ok {
		return nil, nil
	}
	att, ok := mediaAttachment(v.Message)
	if !ok {
		return nil, nil
	}
	return []core.Attachment{att}, nil
}

// mediaAttachment converts the media-bearing sub-message of msg, if any, into a
// platform-agnostic attachment. The bool is false when msg is nil or carries no
// media. ExtraData is the media protobuf (a whatsmeow.DownloadableMessage).
func mediaAttachment(msg *waProto.Message) (core.Attachment, bool) {
	if msg == nil {
		return core.Attachment{}, false
	}
	switch {
	case msg.GetImageMessage() != nil:
		return core.Attachment{IsImage: true, ExtraData: msg.GetImageMessage()}, true
	case msg.GetStickerMessage() != nil:
		return core.Attachment{IsImage: true, ExtraData: msg.GetStickerMessage()}, true
	case msg.GetVideoMessage() != nil:
		return core.Attachment{ExtraData: msg.GetVideoMessage()}, true
	case msg.GetAudioMessage() != nil:
		return core.Attachment{ExtraData: msg.GetAudioMessage()}, true
	case msg.GetDocumentMessage() != nil:
		return core.Attachment{ExtraData: msg.GetDocumentMessage()}, true
	default:
		return core.Attachment{}, false
	}
}

// RawMessage returns the raw whatsmeow message event carried on m, reporting
// whether m originated from the WhatsApp Web (whatsmeow) flavor.
func RawMessage(m *core.Message) (*events.Message, bool) {
	v, ok := m.Raw.(*events.Message)
	return v, ok
}

// Client returns the whatsmeow client backing b, or nil if b is not a WhatsApp
// Web (whatsmeow) bot.
func Client(b *core.Bot) *wm.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

// Download fetches and decrypts the end-to-end-encrypted bytes of att, which
// must come from this adapter's Attachments (its ExtraData is the downloadable
// media protobuf). It returns [core.ErrUnknownBotType] if b is not a WhatsApp
// Web (whatsmeow) bot and [ErrNotDownloadable] if att carries no downloadable
// media.
func Download(ctx context.Context, b *core.Bot, att core.Attachment) ([]byte, error) {
	a, ok := core.AdapterAs[*adapter](b)
	if !ok {
		return nil, core.ErrUnknownBotType
	}
	msg, ok := att.ExtraData.(wm.DownloadableMessage)
	if !ok {
		return nil, ErrNotDownloadable
	}
	return a.client.Download(ctx, msg)
}
