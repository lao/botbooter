package core

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"
)

// defaultSweepInterval is how often the background sweeper reaps expired flow
// state when started without an explicit interval.
const defaultSweepInterval = time.Minute

// defaultFlowTimeout is the per-step idle TTL applied when a Flow sets none. It
// measures user silence: any message for the active step — including an empty or
// rejected answer — counts as engagement and slides the deadline, so a long but
// actively-progressing form never times out mid-fill; only going quiet lets it
// expire.
const defaultFlowTimeout = 10 * time.Minute

// ConversationState is the per-conversation flow state the engine carries between
// messages, keyed per user+channel and held in memory for the life of the
// process (a restart loses every in-flight flow).
type ConversationState struct {
	FlowID    string
	Step      int
	Answers   map[string]string
	ExpiresAt time.Time
}

// isExpired reports whether s has a set expiry that is at or before now. A zero
// ExpiresAt never expires.
func (s ConversationState) isExpired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now)
}

// conversationManager owns the in-process coordination for flows: the per-
// conversation state map plus the background TTL sweeper, both guarded by one
// mutex.
//
// ponytail: single global mutex, not per-key striped locks. It serializes every
// conversation's state transition — and the user validator that runs under it,
// per Validate's "keep it fast" contract — against every other conversation. For
// a single-instance in-memory chat bot that is ample; shard the lock by key only
// if flow throughput is ever a measured bottleneck.
type conversationManager struct {
	mu sync.Mutex
	m  map[string]ConversationState
}

func newConversationManager() *conversationManager {
	return &conversationManager{m: make(map[string]ConversationState)}
}

// withLock runs fn while holding the manager mutex, releasing it even if fn
// panics (the panic propagates to dispatch's recover). The get/set/del helpers
// assume this lock is held.
func (m *conversationManager) withLock(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn()
}

// get, set and del are plain map operations; the caller MUST hold m.mu (via
// withLock).
func (m *conversationManager) get(key string) (ConversationState, bool) {
	st, ok := m.m[key]
	return st, ok
}

func (m *conversationManager) set(key string, st ConversationState) { m.m[key] = st }

func (m *conversationManager) del(key string) { delete(m.m, key) }

// sweep deletes every entry expired as of now. It holds the manager mutex across
// the whole scan, so it can never race a concurrent advance that just refreshed
// an entry's TTL (deleting during a map range is safe in Go).
func (m *conversationManager) sweep(now time.Time) {
	m.withLock(func() {
		for key, st := range m.m {
			if st.isExpired(now) {
				delete(m.m, key)
			}
		}
	})
}

// startSweeper runs sweep on an interval until ctx is done, then closes the
// returned channel so a caller can observe that the goroutine has exited — making
// the sweeper leak-free and testable. A non-positive interval falls back to
// defaultSweepInterval.
func (m *conversationManager) startSweeper(ctx context.Context, interval time.Duration) <-chan struct{} {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sweep(time.Now())
			}
		}
	}()
	return done
}

// Answers is the read-only set of answers collected so far in a flow, handed to a
// Flow's OnComplete callback. Get returns "" for a missing key; Lookup
// distinguishes a missing key from one answered with an empty string.
type Answers map[string]string

// Get returns the answer stored under key, or "" if there is none.
func (a Answers) Get(key string) string { return a[key] }

// Lookup returns the answer stored under key and whether it was present.
func (a Answers) Lookup(key string) (string, bool) {
	v, ok := a[key]
	return v, ok
}

// flowStep is one question in a Flow: a prompt, the key its answer is stored
// under, an optional validator, and whether the answer is a secret (excluded from
// any serialized state — see flow.go).
type flowStep struct {
	key      string
	prompt   string
	validate func(string) error
	secret   bool
}

// Flow is a declarative multi-step dialog: an ordered list of questions plus
// lifecycle callbacks. Build it with NewFlow and register it with Bot.HandleFlow
// (see flow.go). The engine reads the fields here; the builder writes them.
type Flow struct {
	id         string
	steps      []flowStep
	onComplete func(ctx context.Context, b *Bot, m *Message, a Answers)
	onCancel   func(ctx context.Context, b *Bot, m *Message)
	onTimeout  func(ctx context.Context, b *Bot, m *Message)
	cancelWord string
	timeout    time.Duration
}

// timeoutOrDefault is the flow's per-step idle TTL, falling back to
// defaultFlowTimeout when unset.
func (f *Flow) timeoutOrDefault() time.Duration {
	if f.timeout > 0 {
		return f.timeout
	}
	return defaultFlowTimeout
}

// conversationKey composes the per-conversation store key from a message. It is
// per-user AND per-channel, so one user can run independent flows in a DM and in a
// channel. The NUL separator is unambiguous because no supported platform's UserID
// or ChannelID contains a NUL byte (Slack/Discord/Telegram ids are numeric or
// alphanumeric, WhatsApp ids are numeric, Teams ids are prefixed alphanumerics
// like "29:"/"19:", GitHub ids are "owner/repo"/issue-number strings, GitLab ids
// are "group/project#iid"/"group/project!iid", Signal ids are E.164 numbers or
// "group:"-prefixed group ids, and the CLI is single-user trusted input).
func conversationKey(m *Message) string {
	return m.UserID + "\x00" + m.ChannelID
}

// flowByID returns the registered flow with id, if any. A nil registry (a Bot not
// built via New) reports not-found rather than panicking.
func (b *Bot) flowByID(id string) (*Flow, bool) {
	f, ok := b.flows[id]
	return f, ok
}

// sendFlowMessage sends a flow prompt, logging (not propagating) a send error so a
// transient platform failure cannot wedge the dispatch goroutine.
func (b *Bot) sendFlowMessage(ctx context.Context, channelID, text string) {
	if err := b.SendMessageContext(ctx, channelID, text); err != nil {
		b.log().Error("botbooter: failed to send flow prompt", "error", err)
	}
}

// start begins flow for msg's conversation. Under the lock it records the
// initial state if and only if none exists (set-if-absent); after releasing the
// lock it sends the first prompt. A losing racer — state already present — is a
// no-op, and its trigger message is dropped, never consumed as the first answer.
func (m *conversationManager) start(ctx context.Context, b *Bot, msg *Message, flow *Flow) {
	key := conversationKey(msg)

	var started bool
	m.withLock(func() {
		if _, ok := m.get(key); ok {
			return // already active for this conversation; drop the trigger
		}
		m.set(key, ConversationState{
			FlowID:    flow.id,
			Step:      0,
			Answers:   map[string]string{},
			ExpiresAt: time.Now().Add(flow.timeoutOrDefault()),
		})
		started = true
	})

	if started {
		b.sendFlowMessage(ctx, msg.ChannelID, flow.steps[0].prompt)
	}
}

// advance routes msg into the active flow for its conversation, returning true
// when it consumed the message (the caller must then NOT fall through to the
// command table). It runs the state machine under the shard lock and performs the
// resulting prompt send or lifecycle callback AFTER releasing it — those are user
// code and/or network I/O and must never hold a shard.
func (m *conversationManager) advance(ctx context.Context, b *Bot, msg *Message) bool {
	key := conversationKey(msg)
	now := time.Now()

	var (
		handled bool
		action  func()
	)
	m.withLock(func() {
		handled, action = m.transitionLocked(ctx, b, key, msg, now)
	})

	if action != nil {
		action()
	}
	return handled
}

// transitionLocked runs the flow state machine for a single message and MUST be
// called with key's shard lock held. It returns whether the message was consumed
// and the post-lock work to run after the lock is released (a prompt send or a
// lifecycle callback). It performs no I/O itself.
//
// Reaping asymmetry: an unregistered or out-of-range state is reaped and reported
// NOT handled, so dispatch falls through to the command table; an expired state is
// reaped but reported handled (consuming the trigger) and runs the optional
// OnTimeout. The asymmetry is deliberate — see the design spec.
func (m *conversationManager) transitionLocked(ctx context.Context, b *Bot, key string, msg *Message, now time.Time) (handled bool, action func()) {
	state, ok := m.get(key)
	if !ok {
		return false, nil // no active flow; dispatch continues to the command table
	}

	flow, ok := b.flowByID(state.FlowID)
	if !ok {
		// State outlived its flow registration (e.g. a renamed flow); reap and
		// fall through rather than panic.
		m.del(key)
		return false, nil
	}

	// A Store-loaded state whose flow has since lost steps could index out of
	// range; reap and fall through instead of panicking. (A bare panic would be
	// eaten by dispatch's recover but leave the state in place to wedge every
	// later message until its TTL.)
	if state.Step < 0 || state.Step >= len(flow.steps) {
		m.del(key)
		return false, nil
	}

	if state.isExpired(now) {
		m.del(key)
		if flow.onTimeout != nil {
			return true, func() { flow.onTimeout(ctx, b, msg) }
		}
		return true, nil
	}

	send := func(text string) func() {
		return func() { b.sendFlowMessage(ctx, msg.ChannelID, text) }
	}

	content := strings.TrimSpace(msg.Content)

	// The cancel word shadows every step, so it precedes validation.
	if flow.cancelWord != "" && content == flow.cancelWord {
		m.del(key)
		if flow.onCancel != nil {
			return true, func() { flow.onCancel(ctx, b, msg) }
		}
		return true, nil
	}

	step := flow.steps[state.Step]

	// slideTTL refreshes the idle timeout and persists it. The timeout measures
	// user silence, so any message for the active step — including a rejected or
	// empty answer — counts as engagement and slides the deadline; only going
	// quiet lets it expire.
	slideTTL := func() {
		state.ExpiresAt = now.Add(flow.timeoutOrDefault())
		m.set(key, state)
	}

	// Empty/whitespace answers are non-answers: re-prompt without storing an answer.
	if content == "" {
		slideTTL()
		return true, send(step.prompt)
	}

	// The stored (and validated) value is the trimmed content for ordinary steps —
	// chat clients pad messages — but a Secret() step keeps the exact bytes, since
	// leading/trailing whitespace can be meaningful in a password or token.
	answer := content
	if step.secret {
		answer = msg.Content
	}

	// The validator runs under the lock; it is documented as "keep it fast".
	if step.validate != nil {
		if err := step.validate(answer); err != nil {
			nudge := step.prompt
			if e := err.Error(); e != "" {
				nudge = e
			}
			slideTTL()
			return true, send(nudge)
		}
	}

	// Clone before writing rather than mutating in place: Get may return a map that
	// aliases the store's live state (the in-memory store does), and mutating it here
	// would write behind the store's own lock, relying solely on the shard lock. A
	// fresh map keeps transitionLocked from touching data it did not allocate.
	answers := maps.Clone(state.Answers)
	if answers == nil {
		answers = map[string]string{}
	}
	answers[step.key] = answer
	state.Answers = answers

	// Last step → clear state and complete. onComplete receives an exclusive copy
	// of the answers, so a consumer that retains the map is never surprised by the
	// engine reusing the underlying map.
	if state.Step == len(flow.steps)-1 {
		completed := Answers(maps.Clone(answers))
		m.del(key)
		return true, func() { flow.onComplete(ctx, b, msg, completed) }
	}

	// Otherwise advance, slide the TTL, and send the next prompt. The in-memory
	// store volatile-holds the full state (secrets included, per the Secret()
	// contract); secret exclusion happens only at the future durable-Store
	// boundary.
	state.Step++
	slideTTL()
	return true, send(flow.steps[state.Step].prompt)
}
