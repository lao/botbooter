// Package slack is the Slack adapter for botbooter, connecting via Socket Mode.
package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/core"
)

// dispatchBuffer bounds the per-connection dispatch queue; a full queue applies
// backpressure to the event pump rather than dropping events.
const dispatchBuffer = 64

// dispatchDrainTimeout bounds how long Disconnect waits for queued handlers to
// finish before canceling stragglers. A var so tests can shrink it.
var dispatchDrainTimeout = 5 * time.Second

type adapter struct {
	client *slackapi.Client
	socket *socketmode.Client

	// Per-connection dispatch state, guarded by mu. A dedicated dispatcher
	// goroutine drains queue in order so a slow handler cannot freeze the event
	// pump, and Disconnect waits on drained so in-flight work is not abandoned.
	mu             sync.Mutex
	queue          chan func()
	drained        chan struct{}
	detachedCancel context.CancelFunc
}

// startDispatcher launches the in-order dispatch goroutine for one connection
// and records its handles for Disconnect to drain. cancel aborts the detached
// dispatch context after the drain deadline.
func (a *adapter) startDispatcher(cancel context.CancelFunc) chan<- func() {
	queue := make(chan func(), dispatchBuffer)
	drained := make(chan struct{})
	a.mu.Lock()
	a.queue = queue
	a.drained = drained
	a.detachedCancel = cancel
	a.mu.Unlock()
	go func() {
		defer close(drained)
		for fn := range queue {
			fn()
		}
	}()
	return queue
}

// Config configures a Slack bot connecting via Socket Mode.
type Config struct {
	// AppToken is the app-level token (starts "xapp-") that authorizes Socket Mode.
	AppToken string
	// BotToken is the bot-user OAuth token (starts "xoxb-") for Web API calls.
	BotToken string
}

// ErrMissingConfig is returned by New when a required Config field is empty.
var ErrMissingConfig = errors.New("slack: missing required config field")

// New creates a Slack bot that connects via Socket Mode. It returns
// ErrMissingConfig if a required Config field is missing.
func New(cfg Config) (*core.Bot, error) {
	if cfg.AppToken == "" || cfg.BotToken == "" {
		return nil, fmt.Errorf("%w: AppToken and BotToken are required", ErrMissingConfig)
	}
	client := slackapi.New(
		cfg.BotToken,
		slackapi.OptionAppLevelToken(cfg.AppToken),
	)
	socket := socketmode.New(client)

	return core.New(core.SlackBotType, &adapter{client: client, socket: socket}), nil
}

func (a *adapter) Connect(ctx context.Context, deps core.AdapterDeps) error {
	// A detached, cancelable context parents all dispatch: WithoutCancel lets a
	// queued handler finish during the shutdown drain, WithCancel lets Disconnect
	// abort stragglers after the deadline.
	detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))
	queue := a.startDispatcher(detachedCancel)

	// Event pump: Ack promptly and enqueue dispatch in order. Closing queue on
	// exit lets the dispatcher drain the remainder and signal drained.
	go func() {
		defer close(queue)
		a.pumpEvents(ctx, detachedCtx, a.socket.Events, queue, deps)
	}()

	go func() {
		deps.Done(a.socket.RunContext(ctx))
	}()

	return nil
}

// pumpEvents drains events until ctx is canceled or the channel closes,
// acking each and enqueuing its dispatch (on dispatchCtx) in order. A full
// queue applies backpressure; a cancellation during that wait exits promptly.
func (a *adapter) pumpEvents(ctx, dispatchCtx context.Context, events <-chan socketmode.Event, queue chan<- func(), deps core.AdapterDeps) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			fn := a.prepareDispatch(dispatchCtx, evt, deps)
			if fn == nil {
				continue
			}
			select {
			case queue <- fn:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Disconnect drains queued dispatch before returning so an acked event is not
// abandoned at shutdown: core has already canceled runCtx, so the pump is
// exiting and will close the queue; the dispatcher then finishes the remainder
// on the detached context. A handler that ignores cancellation past the drain
// deadline leaks its dispatcher goroutine.
func (a *adapter) Disconnect() error {
	a.mu.Lock()
	drained := a.drained
	cancel := a.detachedCancel
	a.mu.Unlock()
	if drained == nil {
		return nil
	}

	var err error
	select {
	case <-drained:
	case <-time.After(dispatchDrainTimeout):
		err = errors.New("slack: dispatch drain timed out")
	}
	if cancel != nil {
		cancel()
	}

	// Clear the fields only if a reconnect has not installed a newer dispatcher
	// (identity-compare on drained), mirroring the webhook siblings.
	a.mu.Lock()
	if a.drained == drained {
		a.queue = nil
		a.drained = nil
		a.detachedCancel = nil
	}
	a.mu.Unlock()
	return err
}

// Send posts text to channelID via the Web API. With a threading option it posts
// into a thread: the resolved thread_ts is opts.ThreadID when set, else the
// inbound message's ReplyToID (its ThreadTimeStamp). A top-level channel message
// has no ReplyToID, so InReplyTo yields no thread_ts and a plain top-level reply
// — m.ID is deliberately not used as a thread_ts, which would start a stray
// sub-thread. A caller can still force one via WithThreadID.
func (a *adapter) Send(ctx context.Context, channelID, text string, opts core.SendOptions) error {
	threadTS := opts.ThreadID
	if threadTS == "" && opts.ReplyTo != nil {
		threadTS = opts.ReplyTo.ReplyToID
	}
	msgOpts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		msgOpts = append(msgOpts, slackapi.MsgOptionTS(threadTS))
	}
	_, _, err := a.client.PostMessageContext(ctx, channelID, msgOpts...)
	return err
}

// SendThreaded implements [core.ThreadedSender]: it posts text in the thread of
// the message identified by replyToID (a Slack ts) by delegating to Send with a
// WithThreadID option. An empty replyToID inherits Send's guard and degrades to
// a plain top-level channel message (no thread_ts).
func (a *adapter) SendThreaded(ctx context.Context, channelID, replyToID, text string) error {
	return a.Send(ctx, channelID, text, core.SendOptions{ThreadID: replyToID})
}

// Attachments returns the files attached to the message's Slack event.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	msg, _ := RawEvent(m)
	return attachmentsFromMessage(msg), nil
}

// ResolveAttachmentURL implements [core.AttachmentResolver]: for a Slack file it
// prefers the url_private_download link (recovered from att.ExtraData), falling
// back to att.URL (url_private). The result is NOT fetchable with a bare GET —
// download it via [Client](b).GetFileContext, which injects the bot token.
func (a *adapter) ResolveAttachmentURL(_ context.Context, att core.Attachment) (string, error) {
	if file, ok := att.ExtraData.(slackevents.File); ok && file.URLPrivateDownload != "" {
		return file.URLPrivateDownload, nil
	}
	return att.URL, nil
}

// RawEvent returns the raw Slack message event carried on m, reporting whether
// m originated from Slack.
func RawEvent(m *core.Message) (*slackevents.MessageEvent, bool) {
	e, ok := m.Raw.(*slackevents.MessageEvent)
	return e, ok
}

// RawReaction returns the raw Slack reaction event carried on r, reporting
// whether r originated from Slack.
func RawReaction(r *core.Reaction) (*slackevents.ReactionAddedEvent, bool) {
	e, ok := r.Raw.(*slackevents.ReactionAddedEvent)
	return e, ok
}

// Client returns the Slack Web API client backing b, or nil if b is not a Slack
// bot.
func Client(b *core.Bot) *slackapi.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

// SocketClient returns the Socket Mode client backing b, or nil if b is not a
// Slack bot.
func SocketClient(b *core.Bot) *socketmode.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.socket
	}
	return nil
}

// toMessage maps a Slack message event onto a platform-agnostic Message.
// AuthorName is left empty (resolving it would need a per-message API call);
// the message ts doubles as ID and, via ThreadTimeStamp, the reply key.
func toMessage(msg *slackevents.MessageEvent) *core.Message {
	return &core.Message{
		ID:               msg.TimeStamp,
		UserID:           msg.User,
		ChannelID:        msg.Channel,
		Content:          msg.Text,
		Timestamp:        parseSlackTimestamp(msg.TimeStamp),
		ReplyToID:        msg.ThreadTimeStamp,
		MentionedUserIDs: slackMentions(msg.Text),
		Raw:              msg,
	}
}

// toReaction maps a Slack reaction_added event onto a platform-agnostic Reaction.
// The reacted message's channel and ts live under Item; Emoji is the Slack
// shortname wrapped in colons (e.g. ":thumbsup:") so it renders as-is when sent
// back in a Slack message — covering custom workspace emojis too, which have no
// unicode form. AuthorName is left empty (the event carries only a user id).
func toReaction(ev *slackevents.ReactionAddedEvent) *core.Reaction {
	return &core.Reaction{
		Emoji:     ":" + ev.Reaction + ":",
		UserID:    ev.User,
		ChannelID: ev.Item.Channel,
		MessageID: ev.Item.Timestamp,
		Raw:       ev,
	}
}

// slackMentionRE matches "<@U123>" and "<@U123|label>" mention tokens.
var slackMentionRE = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)

// slackMentions extracts mentioned user ids from message text, returning nil
// when there are none. Each id appears once, in first-mention order.
func slackMentions(text string) []string {
	var ids []string
	seen := make(map[string]bool)
	for _, m := range slackMentionRE.FindAllStringSubmatch(text, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		ids = append(ids, m[1])
	}
	return ids
}

// parseSlackTimestamp converts a Slack ts ("1700000000.000100", seconds with a
// 6-digit microsecond fraction) into a UTC time. It returns the zero time when
// ts is empty or its seconds component is non-numeric; a malformed fraction is
// ignored and second precision is kept.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	secs, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var nsec int64
	if frac != "" {
		// Slack's fraction is microseconds; pad/truncate to 6 digits. ParseUint
		// rejects a sign, so a malformed fraction leaves nsec at 0.
		if micros, err := strconv.ParseUint((frac + "000000")[:6], 10, 32); err == nil {
			nsec = int64(micros) * 1000
		}
	}
	return time.Unix(s, nsec).UTC()
}

// prepareDispatch acknowledges evt on the event-pump goroutine and returns a
// closure that dispatches it, or nil when evt carries nothing to dispatch. Ack
// stays on the pump so it is never delayed behind queued handler work.
func (a *adapter) prepareDispatch(ctx context.Context, evt socketmode.Event, deps core.AdapterDeps) func() {
	if evt.Type != socketmode.EventTypeEventsAPI {
		return nil
	}
	payload, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return nil
	}
	if evt.Request != nil {
		a.socket.Ack(*evt.Request)
	}
	return func() { a.handleEventsAPI(ctx, payload, deps) }
}

func (a *adapter) handleEventsAPI(ctx context.Context, e slackevents.EventsAPIEvent, deps core.AdapterDeps) {
	if shouldSkipEvent(e) {
		return
	}

	if msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent); ok {
		deps.Dispatch(ctx, toMessage(msg))
		return
	}

	// reaction_added carries no BotID, so isBotMessage never blocks it. There is
	// no self-reaction filter: the bot never adds reactions (no reaction egress),
	// so a self-reply loop is impossible. Only message reactions carry a channel +
	// ts to reply to; reactions on files/file-comments leave Item.Channel and
	// Item.Timestamp empty, so skip them to keep Reaction.ChannelID/MessageID set.
	if react, ok := e.InnerEvent.Data.(*slackevents.ReactionAddedEvent); ok {
		if react.Item.Type != "message" {
			return
		}
		deps.DispatchReaction(ctx, toReaction(react))
	}
}

// shouldSkipEvent reports whether an inbound event must not be dispatched: it
// drops messages from bots (loop prevention) and messages with no text and no
// files (nothing actionable — e.g. edits, joins). Only *MessageEvent needs skip
// logic because handleEventsAPI dispatches nothing else.
func shouldSkipEvent(event slackevents.EventsAPIEvent) bool {
	ev, ok := event.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return false
	}
	return ev.BotID != "" || ev.SubType == "bot_message" || (ev.Text == "" && len(ev.Files) == 0)
}

func attachmentsFromMessage(m *slackevents.MessageEvent) []core.Attachment {
	if m == nil {
		return nil
	}

	attachments := make([]core.Attachment, 0, len(m.Files))
	for _, file := range m.Files {
		attachments = append(attachments, core.Attachment{
			IsImage:   strings.HasPrefix(file.Mimetype, "image/"),
			URL:       file.URLPrivate,
			ExtraData: file,
		})
	}
	return attachments
}
