package core

import (
	"context"
	"hash/fnv"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
)

// conversationShards is the fixed number of striped locks the conversationManager
// holds. It bounds lock memory by construction: there is no per-key lock map that
// grows with distinct conversations. Two keys that hash to the same shard
// serialize unnecessarily, but with this many shards that is rare and brief.
const conversationShards = 256

// defaultSweepInterval is how often the background sweeper reaps expired flow
// state when started without an explicit interval.
const defaultSweepInterval = time.Minute

// defaultFlowTimeout is the per-step idle TTL applied when a Flow sets none. The
// TTL slides on each successful step, so a long but actively-progressing form does
// not time out mid-fill.
const defaultFlowTimeout = 10 * time.Minute

// ConversationState is the per-conversation flow state the engine carries between
// messages. It is flat and serializable by design so a future durable Store can
// persist it without migration; secret answers are excluded from any serialized
// form (see flow.go). Version bumps on every Set within a key's lifetime; it is
// unused by the single-instance v1 (the in-process striped locks suffice) and is
// exercised from day one so the eventual Store-backed compare-and-swap path needs
// no state change. It is not preserved across a delete-and-recreate of the same
// key (a swept conversation that restarts begins again at 1) — the multi-instance
// CAS path owns cross-lifetime versioning when it lands.
type ConversationState struct {
	FlowID    string
	Step      int
	Answers   map[string]string
	ExpiresAt time.Time
	Version   uint64
}

// isExpired reports whether s has a set expiry that is at or before now. A zero
// ExpiresAt never expires.
func (s ConversationState) isExpired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now)
}

// ConversationStore persists per-conversation flow state by key. The default
// implementation is in-memory (memConversationStore); the interface is the seam a
// durable backend (e.g. Redis) drops into later. The conversationManager
// serializes logical access per key with its striped locks, but a store is still
// touched from the background sweeper, so implementations must be safe for
// concurrent use.
type ConversationStore interface {
	Get(key string) (ConversationState, bool)
	Set(key string, state ConversationState)
	Delete(key string)
}

// expirer is the optional sweeper capability a ConversationStore may implement: it
// reports the keys eligible for reaping at a given instant. memConversationStore
// implements it; a backend with native TTLs (e.g. Redis) need not, and the
// manager simply skips background sweeping for such a store. This mirrors the
// AttachmentResolver optional-capability idiom in core.go.
type expirer interface {
	expiredKeys(now time.Time) []string
}

// memConversationStore is the default in-memory ConversationStore: a map guarded
// by its own RWMutex, held only for the map access itself and never across a
// validator. It is the volatile home for in-flight flows; a process restart loses
// everything in it.
type memConversationStore struct {
	mu sync.RWMutex
	m  map[string]ConversationState
}

func newMemConversationStore() *memConversationStore {
	return &memConversationStore{m: make(map[string]ConversationState)}
}

func (s *memConversationStore) Get(key string) (ConversationState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[key]
	return st, ok
}

// Set stores state under key, bumping Version past the previous value for the key
// (1 for a fresh key) so the field is always monotonic and correct.
func (s *memConversationStore) Set(key string, state ConversationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.Version = s.m[key].Version + 1
	s.m[key] = state
}

func (s *memConversationStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

// expiredKeys returns the keys whose ExpiresAt is set and at or before now. A zero
// ExpiresAt never expires.
func (s *memConversationStore) expiredKeys(now time.Time) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var expired []string
	for k, st := range s.m {
		if st.isExpired(now) {
			expired = append(expired, k)
		}
	}
	return expired
}

// conversationManager owns the in-process coordination for flows: a fixed-size
// striped lock set plus the background TTL sweeper. The ConversationStore behind
// it owns only persistence. The user validator runs UNDER a shard lock, never
// inside a store call, so a durable store can later sit behind compare-and-swap
// without relocating the validator (which is arbitrary user Go code).
type conversationManager struct {
	store  ConversationStore
	locks  [conversationShards]sync.Mutex
	logger *slog.Logger // sweeper diagnostics sink; set by startSweeper, nil → slog.Default
}

func newConversationManager() *conversationManager {
	return &conversationManager{store: newMemConversationStore()}
}

// log returns the configured sweeper logger, falling back to slog.Default so the
// manager is usable before startSweeper wires one in (e.g. in unit tests).
func (m *conversationManager) log() *slog.Logger {
	if m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// shardFor returns the lock guarding key. The same key always maps to the same
// shard.
func (m *conversationManager) shardFor(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &m.locks[h.Sum32()%conversationShards]
}

// withLock runs fn while holding key's shard lock, releasing it even if fn
// panics. The panic propagates to dispatch's recover; the shard stays usable for
// the next operation on any key it guards.
func (m *conversationManager) withLock(key string, fn func()) {
	mu := m.shardFor(key)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

// sweepRecovered runs sweep with a recover so a panic in a custom
// ConversationStore cannot take down the sweeper goroutine (and with it the
// process), mirroring the recover that guards dispatch in core.go.
func (m *conversationManager) sweepRecovered(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			m.log().Error("botbooter: recovered from panic in conversation sweeper", "panic", r)
		}
	}()
	m.sweep(now)
}

// sweep deletes every expired entry as of now. It takes each key's shard lock and
// re-checks expiry under it, so it can never race a concurrent advance that just
// refreshed the entry's TTL. A store without the expirer capability is a no-op.
func (m *conversationManager) sweep(now time.Time) {
	ex, ok := m.store.(expirer)
	if !ok {
		return
	}
	for _, key := range ex.expiredKeys(now) {
		m.withLock(key, func() {
			if st, ok := m.store.Get(key); ok && st.isExpired(now) {
				m.store.Delete(key)
			}
		})
	}
}

// startSweeper runs sweep on an interval until ctx is done, then closes the
// returned channel so a caller can observe that the goroutine has exited — making
// the sweeper leak-free and testable. A non-positive interval falls back to
// defaultSweepInterval. logger (may be nil) routes sweeper panic recovery through
// the Bot's configured diagnostics sink.
func (m *conversationManager) startSweeper(ctx context.Context, interval time.Duration, logger *slog.Logger) <-chan struct{} {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	m.logger = logger
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
				m.sweepRecovered(time.Now())
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
// alphanumeric, WhatsApp ids are numeric, and the CLI is single-user trusted
// input).
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

// start begins flow for msg's conversation. Under the shard lock it records the
// initial state if and only if none exists (set-if-absent); after releasing the
// lock it sends the first prompt. A losing racer — state already present — is a
// no-op, and its trigger message is dropped, never consumed as the first answer.
func (m *conversationManager) start(ctx context.Context, b *Bot, msg *Message, flow *Flow) {
	key := conversationKey(msg)

	var started bool
	m.withLock(key, func() {
		if _, ok := m.store.Get(key); ok {
			return // already active for this conversation; drop the trigger
		}
		m.store.Set(key, ConversationState{
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
	m.withLock(key, func() {
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
	state, ok := m.store.Get(key)
	if !ok {
		return false, nil // no active flow; dispatch continues to the command table
	}

	flow, ok := b.flowByID(state.FlowID)
	if !ok {
		// State outlived its flow registration (e.g. a renamed flow); reap and
		// fall through rather than panic.
		m.store.Delete(key)
		return false, nil
	}

	// A Store-loaded state whose flow has since lost steps could index out of
	// range; reap and fall through instead of panicking. (A bare panic would be
	// eaten by dispatch's recover but leave the state in place to wedge every
	// later message until its TTL.)
	if state.Step < 0 || state.Step >= len(flow.steps) {
		m.store.Delete(key)
		return false, nil
	}

	if state.isExpired(now) {
		m.store.Delete(key)
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
		m.store.Delete(key)
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
		m.store.Set(key, state)
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
		m.store.Delete(key)
		return true, func() { flow.onComplete(ctx, b, msg, completed) }
	}

	// Otherwise advance, slide the TTL, and send the next prompt. The in-memory
	// store volatile-holds the full state (secrets included, per the Secret()
	// contract); secret exclusion happens only at the future durable-Store
	// boundary via serializableState.
	state.Step++
	slideTTL()
	return true, send(flow.steps[state.Step].prompt)
}
