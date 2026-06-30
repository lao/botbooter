package core

import (
	"context"
	"hash/fnv"
	"log"
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
	store ConversationStore
	locks [conversationShards]sync.Mutex
}

func newConversationManager() *conversationManager {
	return &conversationManager{store: newMemConversationStore()}
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
			log.Printf("botbooter: recovered from panic in conversation sweeper: %v", r)
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
				m.sweepRecovered(time.Now())
			}
		}
	}()
	return done
}
