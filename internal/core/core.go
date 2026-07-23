// Package core holds botbooter's platform-agnostic engine: the Bot type, its
// command/middleware dispatch, and the connection lifecycle.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrUnknownBotType is returned by Bot methods when the Bot has no adapter.
var ErrUnknownBotType = errors.New("botbooter: unknown bot type")

// ErrAlreadyConnected is returned by Connect when the Bot is already connected.
var ErrAlreadyConnected = errors.New("botbooter: already connected")

// ErrNilMessage is returned by Bot methods handed a nil *Message argument.
var ErrNilMessage = errors.New("botbooter: nil message")

// BotType identifies the messaging platform a Bot is connected to.
type BotType int

// The supported bot types.
const (
	SlackBotType BotType = iota
	DiscordBotType
	CLIBotType
	TelegramBotType
	WhatsAppBotType
	TeamsBotType
	WhatsMeowBotType
	GitHubBotType
)

// String returns the lowercase platform name for t, or "BotType(n)" for an
// unknown value.
func (t BotType) String() string {
	switch t {
	case SlackBotType:
		return "slack"
	case DiscordBotType:
		return "discord"
	case CLIBotType:
		return "cli"
	case TelegramBotType:
		return "telegram"
	case WhatsAppBotType:
		return "whatsapp"
	case TeamsBotType:
		return "teams"
	case WhatsMeowBotType:
		return "whatsmeow"
	case GitHubBotType:
		return "github"
	default:
		return fmt.Sprintf("BotType(%d)", int(t))
	}
}

// Message is a platform-agnostic incoming message handed to command handlers.
// UserID, ChannelID and Content are always set; the remaining normalized fields
// are best-effort per platform. Raw carries the originating platform's untouched
// event; read it with the matching typed accessor (e.g. discord.RawEvent).
type Message struct {
	ID         string
	UserID     string
	AuthorName string
	ChannelID  string
	Content    string
	Timestamp  time.Time
	ReplyToID  string
	// MentionedUserIDs lists each mentioned user once, in first-mention order.
	// Whether the bot's own mention appears follows Content: Teams strips the
	// bot's mention from Content and so excludes its ID here; Slack and Discord
	// keep it in both.
	MentionedUserIDs []string

	Raw any
}

// Reaction is a platform-agnostic emoji reaction added to a message, handed to
// handlers registered with [Bot.OnReaction]. UserID, ChannelID and MessageID are
// always set; MessageID identifies the reacted message and is the reply target for
// [Bot.ReplyToMessage]. Emoji renders as-is when sent back in a message on its
// origin platform: a unicode character on most platforms, Slack's colon-wrapped
// shortname (":thumbsup:", covering custom workspace emojis), Discord's
// "<:name:id>" markup for its custom emojis. It is NOT normalized across
// platforms — compare per platform. AuthorName is best-effort and inline-only —
// empty on platforms whose reaction payload carries only a user id. Raw carries
// the originating platform event untouched; read it with the matching typed
// accessor (e.g. slack.RawReaction) to recover the platform's original values,
// such as the unwrapped emoji name (slack.RawReaction(r).Reaction gives
// "thumbsup", discord.RawReaction(r).Emoji.Name the bare custom-emoji name).
type Reaction struct {
	Emoji      string
	UserID     string
	AuthorName string
	ChannelID  string
	MessageID  string

	Raw any
}

// ReactionHandler handles an emoji reaction dispatched to [Bot.OnReaction].
type ReactionHandler func(ctx context.Context, b *Bot, r *Reaction)

// CommandHandler handles a dispatched message for a matched command.
type CommandHandler func(ctx context.Context, b *Bot, m *Message)

// Middleware wraps message dispatch; it must call next to continue the chain.
type Middleware func(ctx context.Context, b *Bot, m *Message, next CommandHandler)

// Command pairs a regular-expression Pattern with the Handler to run on a match.
type Command struct {
	Pattern string
	Handler CommandHandler

	re *regexp.Regexp
}

func (c *Command) match(content string) bool {
	// re is always non-nil: AddHandler is the only path that appends to Bot.commands.
	return c.re.MatchString(content)
}

// Attachment is a platform-agnostic file attached to a message.
type Attachment struct {
	IsImage   bool
	URL       string
	ExtraData any
}

// Adapter is the platform-specific half of a Bot. The Bot drives it through this
// interface, so the core has no compile-time dependency on any platform.
type Adapter interface {
	Connect(ctx context.Context, deps AdapterDeps) error
	Disconnect() error
	Send(ctx context.Context, channelID, text string, opts SendOptions) error
	Attachments(m *Message) ([]Attachment, error)
}

// AttachmentResolver is an optional capability an Adapter may implement to turn
// an Attachment into a downloadable URL; adapters whose Attachment.URL is already
// usable ride the passthrough in [Bot.ResolveAttachmentURL].
type AttachmentResolver interface {
	ResolveAttachmentURL(ctx context.Context, att Attachment) (string, error)
}

// ThreadedSender is an OPTIONAL capability an Adapter may implement to post a
// reply nested on a specific message. Like [AttachmentResolver] it is deliberately
// NOT part of the mandatory Adapter interface: adapters that implement it get
// threaded replies via [Bot.ReplyToMessage]; those that do not fall back to a
// plain channel message.
type ThreadedSender interface {
	SendThreaded(ctx context.Context, channelID, replyToID, text string) error
}

// SendOptions is the resolved set of per-send modifiers an Adapter reads off a
// Send call. Its zero value means "a plain channel message". A threading anchor
// is platform-specific, so each adapter derives its own from these fields:
//   - ReplyTo: reply anchored on this whole message; the adapter picks the
//     correct native anchor (Slack thread_ts from ReplyToID; Discord/Telegram/
//     WhatsApp the replied-to/quoted message id).
//   - ThreadID: a raw native anchor supplied by the caller, used verbatim. It
//     wins over ReplyTo when both are set. On Slack it is a thread_ts; on
//     Discord/Telegram/WhatsApp a reply/quote message id (NOT a Discord
//     thread-channel id).
type SendOptions struct {
	ReplyTo  *Message
	ThreadID string
}

// SendOption modifies a SendOptions. Construct them with [InReplyTo] /
// [WithThreadID] and pass them to [Bot.SendMessageContext] / [Bot.SendMessage].
type SendOption func(*SendOptions)

// InReplyTo anchors the send on m so the adapter posts into m's thread or
// reply-chain, deriving the correct per-platform anchor itself.
func InReplyTo(m *Message) SendOption { return func(o *SendOptions) { o.ReplyTo = m } }

// WithThreadID anchors the send on a raw native id the adapter uses verbatim.
// It takes precedence over [InReplyTo]. The caller owns platform-correctness.
func WithThreadID(id string) SendOption { return func(o *SendOptions) { o.ThreadID = id } }

// resolveSendOptions folds opts into a single SendOptions, skipping nil options
// so a conditionally-built option slice can carry a nil entry without panicking.
func resolveSendOptions(opts ...SendOption) SendOptions {
	var o SendOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// AdapterDeps is the set of callbacks an Adapter uses to talk back to the Bot,
// plus the Bot's logger so adapter diagnostics route through the same sink.
// DispatchReaction is optional: adapters on platforms without reaction events
// simply never call it. HasReactionHandlers reports whether any OnReaction
// handler was registered before Connect, so an adapter whose reaction ingress
// costs something (the GitHub poller pays API requests per cycle) can skip the
// work nobody consumes; push-based adapters may ignore it — dispatching to
// zero handlers is free. Handlers registered after Connect do not flip it: the
// snapshot follows the register-before-Connect contract.
type AdapterDeps struct {
	Dispatch            func(ctx context.Context, m *Message)
	DispatchReaction    func(ctx context.Context, r *Reaction)
	HasReactionHandlers bool
	// Done reports that this connection's event loop terminated. Call it AT MOST
	// ONCE per connection: it writes to a size-1 buffered channel, so a redundant
	// Done is at best silently dropped and at worst blocks the caller forever
	// (when Run was woken by ctx/Disconnect and never drains the channel). An
	// adapter that can emit several terminal events (whatsmeow) must collapse them
	// into a single Done — e.g. behind a per-connection sync.Once.
	Done       func(err error)
	Disconnect func() error
	Logger     *slog.Logger // always non-nil
}

// Bot is the platform-agnostic chat bot. Register handlers, middleware, and flows
// (HandleFlow — and the Flow builder calls behind it) before Connect; after that,
// Connect/Run/Disconnect/Send are safe to call concurrently. Registering, or
// mutating a registered Flow, after Connect races the dispatch goroutine.
type Bot struct {
	BotType BotType

	adapter Adapter

	commands              []Command
	unknownCommandHandler CommandHandler
	middlewares           []Middleware
	reactionHandlers      []ReactionHandler
	setupErrs             []error // registration errors, surfaced by Connect
	logger                *slog.Logger

	conversations *conversationManager
	flows         map[string]*Flow

	// dispatchChain is the middleware chain wrapped around the terminal
	// flow/command handler. Connect composes it once per connection from the
	// current middleware snapshot and reuses it for every message; a reconnect
	// re-snapshots, so a Bot reused across runs picks up middleware registered
	// between runs. Middleware is register-before-Connect, so the snapshot is
	// stable during a run; the inner handler still reads live Bot state (commands,
	// flows) on each call. Stored atomically so a straggler dispatch from a prior
	// connection can read it while a reconnect stores a fresh chain, race-free.
	dispatchChain atomic.Pointer[CommandHandler]

	mu   sync.Mutex
	conn *connection
}

// connection holds a single connection's lifecycle state. Connect creates a
// fresh one and Disconnect drops it, so nothing from a prior connection can
// leak into a successor across disconnect→reconnect races.
type connection struct {
	cancel   context.CancelFunc
	done     chan error    // adapter reports event-loop termination here
	runDone  chan struct{} // closed exactly once when this connection tears down
	once     sync.Once
	discOnce sync.Once // scopes adapter.Disconnect to exactly one true teardown
	discErr  error     // adapter.Disconnect result, recorded once and shared by all true callers
	adapter  Adapter
	// sweeperDone is closed by the conversation sweeper goroutine when it exits
	// (nil when no sweeper started for this connection). It is a test-observable
	// hook for asserting the background goroutine is gone after teardown; the
	// production teardown path does not wait on it.
	sweeperDone <-chan struct{}
}

// teardown cancels the run context and closes runDone exactly once. It runs the
// adapter's Disconnect only when disconnectAdapter is true; a superseded
// connection passes false so it can never disconnect the shared adapter a newer
// connection now owns.
//
// The adapter call sits under its own discOnce, NOT the runDone once: c.cancel
// wakes adapter ctx-watchers whose deps.Disconnect arrives as a superseded
// (false) teardown, and if that consumed a shared once first it would swallow
// the true caller's adapter Disconnect entirely. A false teardown never touches
// discOnce, while concurrent true callers (b.conn is uninstalled only after
// teardown returns, so more than one caller can observe itself current)
// collapse into one adapter Disconnect — the losers block until the winner
// finishes and share its error via discErr.
func (c *connection) teardown(disconnectAdapter bool) error {
	c.cancel()
	c.once.Do(func() { close(c.runDone) })
	if !disconnectAdapter {
		return nil
	}
	if c.adapter == nil {
		return ErrUnknownBotType
	}
	c.discOnce.Do(func() { c.discErr = c.adapter.Disconnect() })
	return c.discErr
}

// New creates a Bot of the given type backed by adapter.
func New(botType BotType, adapter Adapter) *Bot {
	return &Bot{
		BotType:       botType,
		adapter:       adapter,
		conversations: newConversationManager(),
		flows:         make(map[string]*Flow),
	}
}

// AdapterAs returns the Bot's adapter as T, reporting whether it is that type.
// Adapter packages use it to recover their concrete adapter from a *Bot.
func AdapterAs[T any](b *Bot) (T, bool) {
	a, ok := b.adapter.(T)
	return a, ok
}

// AddHandler registers cmd, compiling its Pattern. An invalid pattern is
// recorded rather than returned — it surfaces from Connect (and Run) and is
// also logged at record time (so a registration after Connect is not silently
// dropped), and the command is not registered. Commands are matched in
// registration order, first match wins.
func (b *Bot) AddHandler(cmd Command) {
	re, err := regexp.Compile(cmd.Pattern)
	if err != nil {
		b.log().Error("botbooter: invalid command pattern", "pattern", cmd.Pattern, "error", err)
		b.setupErrs = append(b.setupErrs, fmt.Errorf("botbooter: invalid command pattern %q: %w", cmd.Pattern, err))
		return
	}
	cmd.re = re
	b.commands = append(b.commands, cmd)
}

// HandleFunc is a convenience wrapper around AddHandler.
func (b *Bot) HandleFunc(pattern string, handler CommandHandler) {
	b.AddHandler(Command{Pattern: pattern, Handler: handler})
}

// SetUnknownCommandHandler sets the handler invoked when a message matches no
// registered command; if unset, unmatched messages are ignored.
func (b *Bot) SetUnknownCommandHandler(handler CommandHandler) {
	b.unknownCommandHandler = handler
}

// AddMiddleware appends middleware to the dispatch chain, run in registration order.
func (b *Bot) AddMiddleware(middleware Middleware) {
	b.middlewares = append(b.middlewares, middleware)
}

// OnReaction registers h to run whenever a user adds an emoji reaction, on the
// platforms that surface reaction events (Slack, Discord, Telegram, WhatsApp,
// GitHub). Handlers are not regex-matched — branch on Reaction.Emoji inside the
// handler — and, unlike message dispatch, reactions bypass the Middleware chain.
// Register before Connect: adapters whose reaction ingress costs something
// decide at Connect whether to run it ([AdapterDeps].HasReactionHandlers) — on
// GitHub, a handler registered only after Connect means no reaction poller
// starts and OnReaction never fires for that connection.
//
// Cross-bot reaction loops are handler-owned on Slack: its reaction_added event
// carries no bot flag, so the adapter cannot drop another bot's reaction the way
// Telegram/Discord/GitHub do. A handler that emits a reply (e.g. ReplyToMessage)
// in response to a reaction can therefore ping-pong with another auto-reacting
// bot — guard such handlers (e.g. by author, emoji, or a rate limit).
func (b *Bot) OnReaction(h ReactionHandler) {
	b.reactionHandlers = append(b.reactionHandlers, h)
}

// dispatchReaction runs every registered reaction handler. Each handler call is
// recovered individually, so a panic in one handler neither skips the others nor
// takes down the adapter's event loop.
func (b *Bot) dispatchReaction(ctx context.Context, r *Reaction) {
	for _, h := range b.reactionHandlers {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					b.log().Error("botbooter: recovered from panic while handling reaction", "panic", rec)
				}
			}()
			h(ctx, b, r)
		}()
	}
}

// SetLogger routes the Bot's and its adapter's diagnostics (panic recovery,
// shutdown warnings, webhook rejections) through logger instead of
// [slog.Default]. Like handler registration, call it before Connect.
func (b *Bot) SetLogger(logger *slog.Logger) {
	b.logger = logger
}

// log returns the configured logger, falling back to slog.Default().
func (b *Bot) log() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// Connect starts the adapter's event loop and returns without blocking. It
// returns ErrAlreadyConnected if a connection is already active, ErrUnknownBotType
// if the Bot has no adapter, every registration error recorded by AddHandler
// (joined, one per invalid pattern), or any error from the adapter's own Connect.
func (b *Bot) Connect(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return ErrAlreadyConnected
	}
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	if err := errors.Join(b.setupErrs...); err != nil {
		return err
	}
	// Compose the dispatch chain from the current middleware snapshot, redone on
	// every Connect so a Bot reused across a reconnect reflects middleware
	// registered between runs. Register-before-Connect keeps it stable during a run.
	chain := b.composeDispatchChain()
	b.dispatchChain.Store(&chain)
	runCtx, cancel := context.WithCancel(ctx)
	c := &connection{
		cancel:  cancel,
		done:    make(chan error, 1),
		runDone: make(chan struct{}),
		adapter: b.adapter,
	}

	// The callbacks capture THIS connection (c), so a lingering goroutine from a
	// prior connection writes into its own dead channel and never touches the
	// shared adapter.
	deps := AdapterDeps{
		Dispatch:            b.dispatch,
		DispatchReaction:    b.dispatchReaction,
		HasReactionHandlers: len(b.reactionHandlers) > 0,
		Done:                func(err error) { c.done <- err },
		Disconnect:          func() error { return b.disconnectConn(c) },
		Logger:              b.log(),
	}

	// adapter.Connect is non-blocking by contract. Holding b.mu across it — and
	// installing b.conn only on success — serializes a concurrent Disconnect
	// behind this lock instead of racing a half-connected adapter.
	if err := b.adapter.Connect(runCtx, deps); err != nil {
		cancel()
		return err
	}
	b.conn = c

	// Start the conversation sweeper on the per-connection run context so it stops
	// when this connection is torn down and a reconnect installs a fresh one. The
	// in-memory flow state itself is per-Bot and survives a transient reconnect;
	// only background sweeping pauses for that window (lazy expiry still applies).
	// Gated on registered flows so a bot that never calls HandleFlow spins up no
	// background goroutine.
	if b.conversations != nil && len(b.flows) > 0 {
		c.sweeperDone = b.conversations.startSweeper(runCtx, defaultSweepInterval, b.log())
	}
	return nil
}

// disconnectConn tears down connection c. If c is still the installed
// connection the adapter is disconnected; if c has been superseded by a
// reconnect, only c's own runCtx/runDone are settled and the adapter — now
// owned by the newer connection — is left untouched.
//
// b.conn is cleared only AFTER teardown returns, so a Connect racing a
// slow adapter Disconnect (drains can take seconds) gets ErrAlreadyConnected
// instead of starting a second live session on the shared adapter. Concurrent
// teardowns of the same connection serialize on its sync.Once: the losers
// block inside teardown until the winner's adapter Disconnect finishes.
func (b *Bot) disconnectConn(c *connection) error {
	b.mu.Lock()
	current := b.conn == c
	b.mu.Unlock()

	err := c.teardown(current)

	if current {
		b.mu.Lock()
		if b.conn == c {
			b.conn = nil
		}
		b.mu.Unlock()
	}
	return err
}

// Disconnect tears down the active connection: it cancels the run context and
// runs the adapter's Disconnect exactly once. It is safe to call when not
// connected, returning ErrUnknownBotType only if the Bot has no adapter.
func (b *Bot) Disconnect() error {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()

	if c == nil {
		if b.adapter == nil {
			return ErrUnknownBotType
		}
		return nil
	}
	return b.disconnectConn(c)
}

// Run connects the Bot and blocks until ctx is canceled, the event loop ends,
// or Disconnect is called from elsewhere, then disconnects. A clean shutdown
// (ctx cancellation or a local Disconnect) returns nil rather than ctx.Err(),
// so callers can safely do log.Fatal(bot.Run(ctx)).
func (b *Bot) Run(ctx context.Context) error {
	if err := b.Connect(ctx); err != nil {
		return err
	}

	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	// A concurrent Disconnect already tore the connection down.
	if c == nil {
		return nil
	}

	var loopErr error
	select {
	case <-ctx.Done():
	case <-c.runDone: // a Disconnect from another goroutine woke us
	case loopErr = <-c.done:
	}

	// Tear down the connection Run OWNS, not whatever b.conn currently points at.
	// If a reconnect superseded c while Run was blocked, b.Disconnect() would tear
	// down the live successor; disconnectConn(c) instead scopes teardown to c and
	// degrades to a no-op adapter teardown when c is no longer installed.
	disconnectErr := b.disconnectConn(c)

	// Graceful shutdown (ctx canceled, or a local Disconnect canceling runCtx)
	// surfaces as loopErr echoing the canceling context's error; swallow it so
	// log.Fatal(bot.Run(ctx)) doesn't exit non-zero on a clean Ctrl-C.
	if errors.Is(loopErr, context.Canceled) ||
		(ctx.Err() != nil && errors.Is(loopErr, ctx.Err())) {
		loopErr = nil
	}

	if disconnectErr != nil {
		if ctx.Err() != nil {
			b.log().Error("botbooter: error disconnecting during shutdown", "error", disconnectErr)
		} else if loopErr == nil {
			loopErr = disconnectErr
		}
	}
	return loopErr
}

// Start runs the Bot until the process receives an interrupt or SIGTERM.
func (b *Bot) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return b.Run(ctx)
}

// SendMessage sends text to channelID using a background context. Prefer
// SendMessageContext from within a handler so the send honors shutdown and
// cancellation; SendMessage's background context outlives Run's teardown.
func (b *Bot) SendMessage(channelID, text string, opts ...SendOption) error {
	return b.SendMessageContext(context.Background(), channelID, text, opts...)
}

// SendMessageContext sends text to channelID, honoring ctx for cancellation.
// Pass [InReplyTo] or [WithThreadID] to thread the message onto an inbound one;
// with no options it is a plain channel message.
func (b *Bot) SendMessageContext(ctx context.Context, channelID, text string, opts ...SendOption) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	return b.adapter.Send(ctx, channelID, text, resolveSendOptions(opts...))
}

// ReplyToMessage posts text as a reply nested on the message identified by
// replyToID in channelID. If the Bot's adapter implements [ThreadedSender] the
// call is delegated; otherwise it falls back to a plain [Bot.SendMessageContext],
// so the reply still reaches the channel, just not threaded. It returns
// [ErrUnknownBotType] if the Bot has no adapter.
func (b *Bot) ReplyToMessage(ctx context.Context, channelID, replyToID, text string) error {
	if b.adapter == nil {
		return ErrUnknownBotType
	}
	if s, ok := b.adapter.(ThreadedSender); ok {
		return s.SendThreaded(ctx, channelID, replyToID, text)
	}
	return b.adapter.Send(ctx, channelID, text, SendOptions{})
}

// Reply is convenience sugar for replying into the thread or reply-chain of the
// inbound message m — it is exactly SendMessageContext(ctx, m.ChannelID, text,
// InReplyTo(m)). Each adapter derives its own platform-specific anchor; see
// [SendOptions]. It returns ErrNilMessage if m is nil, or ErrUnknownBotType if
// the Bot has no adapter.
func (b *Bot) Reply(ctx context.Context, m *Message, text string) error {
	if m == nil {
		return ErrNilMessage
	}
	// A nil adapter is caught by the delegated SendMessageContext.
	return b.SendMessageContext(ctx, m.ChannelID, text, InReplyTo(m))
}

// GetAttachments returns the platform-agnostic attachments of message. It
// returns ErrNilMessage if message is nil, or ErrUnknownBotType if the Bot has
// no adapter.
func (b *Bot) GetAttachments(message *Message) ([]Attachment, error) {
	if message == nil {
		return nil, ErrNilMessage
	}
	if b.adapter == nil {
		return nil, ErrUnknownBotType
	}
	return b.adapter.Attachments(message)
}

// ResolveAttachmentURL returns a downloadable URL for att. If the adapter
// implements [AttachmentResolver] the call is delegated; otherwise att.URL is
// returned verbatim. It returns [ErrUnknownBotType] if the Bot has no adapter.
// An empty string with a nil error means "not resolvable", not a failure.
//
// The result is consumed differently per platform:
//   - Discord: a signed CDN link (~24h); plain GET, consume promptly.
//   - Slack: not directly fetchable — download via the Slack Web API client
//     (SlackClient(b).GetFileContext), which injects the bot token.
//   - Telegram: a plain GET on a secret, ~1h URL that embeds the bot token —
//     never log or cache it. Each resolve logs a warning, suppressible via
//     BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING.
//   - WhatsApp: GET with an "Authorization: Bearer <token>" header (the Cloud
//     API token used to send). Short-lived; consume promptly.
//   - Teams: a pre-authorized link carrying a short-lived token — consume
//     promptly, never log or cache. Inline images may need an Authorization
//     header this adapter does not yet supply. The URL is sender-influenced
//     (it rides in on the inbound Activity), so treat it as an SSRF hazard:
//     validate the host/scheme before fetching, never let it target an internal
//     address.
//   - CLI: a local filesystem path (open with os.Open), not an HTTP URL.
func (b *Bot) ResolveAttachmentURL(ctx context.Context, att Attachment) (string, error) {
	if b.adapter == nil {
		return "", ErrUnknownBotType
	}
	if r, ok := b.adapter.(AttachmentResolver); ok {
		return r.ResolveAttachmentURL(ctx, att)
	}
	return att.URL, nil
}

// dispatch routes message through the middleware chain to the first matching
// command. A panic in any handler or middleware is recovered and logged so it
// cannot take down the event loop. The middleware chain is composed once per
// connection (in Connect) and reused, since middleware is registered before Connect.
func (b *Bot) dispatch(ctx context.Context, message *Message) {
	defer func() {
		if r := recover(); r != nil {
			b.log().Error("botbooter: recovered from panic while handling message", "panic", r)
		}
	}()

	if p := b.dispatchChain.Load(); p != nil {
		(*p)(ctx, b, message)
		return
	}
	// No connection was ever established (e.g. a unit test calling dispatch
	// directly); compose on the fly from the current middleware.
	b.composeDispatchChain()(ctx, b, message)
}

// composeDispatchChain wraps the terminal flow/command handler in the registered
// middleware, inner-to-outer, so registration order equals execution order. The
// returned handler closes over the middleware snapshot but reads live Bot state
// (flows, commands, unknownCommandHandler) on every call, so composing it once is
// equivalent to rebuilding it per message.
func (b *Bot) composeDispatchChain() CommandHandler {
	handler := func(ctx context.Context, bot *Bot, message *Message) {
		// An active flow for this conversation consumes the message before the
		// command table; advance reports false (and we fall through) when no flow
		// is active or its state outlived its registration.
		if len(bot.flows) > 0 && bot.conversations != nil && bot.conversations.advance(ctx, bot, message) {
			return
		}
		for i := range bot.commands {
			if bot.commands[i].match(message.Content) {
				bot.commands[i].Handler(ctx, bot, message)
				return
			}
		}
		if bot.unknownCommandHandler != nil {
			bot.unknownCommandHandler(ctx, bot, message)
		}
	}

	final := handler
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		middleware := b.middlewares[i]
		next := final
		final = func(ctx context.Context, bot *Bot, message *Message) {
			middleware(ctx, bot, message, next)
		}
	}
	return final
}
