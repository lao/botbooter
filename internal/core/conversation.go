package core

import (
	"cmp"
	"context"
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
	Answers   Answers
	ExpiresAt time.Time
}

// isExpired reports whether s has a set expiry that is at or before now. A zero
// ExpiresAt never expires.
func (s ConversationState) isExpired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now)
}

// The mutex protects all active conversations, including updates during validation.
type conversationManager struct {
	mu     sync.Mutex
	active map[string]ConversationState
}

func newConversationManager() *conversationManager {
	return &conversationManager{active: make(map[string]ConversationState)}
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
	st, ok := m.active[key]
	return st, ok
}

func (m *conversationManager) set(key string, st ConversationState) { m.active[key] = st }

func (m *conversationManager) del(key string) { delete(m.active, key) }

// sweep deletes every entry expired as of now. It holds the manager mutex across
// the whole scan, so it can never race a concurrent advance that just refreshed
// an entry's TTL (deleting during a map range is safe in Go).
func (m *conversationManager) sweep(now time.Time) {
	m.withLock(func() {
		for key, st := range m.active {
			if st.isExpired(now) {
				delete(m.active, key)
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
// under, an optional validator, and whether the answer is a secret (see flow.go).
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

// Separate the user and channel IDs with NUL so their boundaries are unambiguous.
func conversationKey(m *Message) string {
	return m.UserID + "\x00" + m.ChannelID
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
			ExpiresAt: time.Now().Add(flow.timeoutOrDefault()),
		})
		started = true
	})

	if started {
		b.sendFlowMessage(ctx, msg.ChannelID, flow.steps[0].prompt)
	}
}

// advance consumes a message for an active flow. Prompts and callbacks run after unlocking.
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

// transitionLocked updates the active conversation under m.mu and returns work
// for the caller to run after unlocking. Expiry consumes the current message.
func (m *conversationManager) transitionLocked(ctx context.Context, b *Bot, key string, msg *Message, now time.Time) (handled bool, action func()) {
	state, ok := m.get(key)
	if !ok {
		return false, nil // no active flow; dispatch continues to the command table
	}

	flow, ok := b.flows[state.FlowID]
	if !ok || state.Step < 0 || state.Step >= len(flow.steps) {
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

	// Even an invalid answer restarts the idle timeout.
	saveProgress := func() {
		state.ExpiresAt = now.Add(flow.timeoutOrDefault())
		m.set(key, state)
	}

	if content == "" {
		saveProgress()
		return true, send(step.prompt)
	}

	// Leading and trailing whitespace can be part of a secret.
	answer := content
	if step.secret {
		answer = msg.Content
	}

	if step.validate != nil {
		if err := step.validate(answer); err != nil {
			saveProgress()
			return true, send(cmp.Or(err.Error(), step.prompt))
		}
	}

	if state.Answers == nil {
		state.Answers = make(Answers)
	}
	state.Answers[step.key] = answer

	if state.Step == len(flow.steps)-1 {
		// Removing the conversation transfers ownership of its answers to the callback.
		m.del(key)
		return true, func() { flow.onComplete(ctx, b, msg, state.Answers) }
	}

	state.Step++
	saveProgress()
	return true, send(flow.steps[state.Step].prompt)
}
