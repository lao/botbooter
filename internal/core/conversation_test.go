package core

import (
	"context"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

func TestMemConversationStore_GetSetDelete(t *testing.T) {
	s := newMemConversationStore()

	_, ok := s.Get("k")
	asserts.False(t, ok, "missing key reports absent")

	s.Set("k", ConversationState{FlowID: "f", Step: 2})
	st, ok := s.Get("k")
	asserts.True(t, ok, "present after Set")
	asserts.Equal(t, st.FlowID, "f", "FlowID round-trips")
	asserts.Equal(t, st.Step, 2, "Step round-trips")

	s.Delete("k")
	_, ok = s.Get("k")
	asserts.False(t, ok, "absent after Delete")
}

func TestMemConversationStore_VersionBump(t *testing.T) {
	s := newMemConversationStore()

	s.Set("k", ConversationState{FlowID: "f"})
	st, _ := s.Get("k")
	asserts.Equal(t, st.Version, uint64(1), "first Set bumps Version to 1")

	s.Set("k", ConversationState{FlowID: "f", Step: 1})
	st, _ = s.Get("k")
	asserts.Equal(t, st.Version, uint64(2), "second Set bumps Version to 2")

	// A caller-supplied Version is ignored: the store owns the counter.
	s.Set("k", ConversationState{FlowID: "f", Version: 99})
	st, _ = s.Get("k")
	asserts.Equal(t, st.Version, uint64(3), "store overrides a caller-supplied Version")
}

func TestMemConversationStore_ExpiredKeys(t *testing.T) {
	s := newMemConversationStore()
	now := time.Now()
	s.Set("at", ConversationState{ExpiresAt: now}) // == now → expired
	s.Set("past", ConversationState{ExpiresAt: now.Add(-time.Second)})
	s.Set("future", ConversationState{ExpiresAt: now.Add(time.Hour)})
	s.Set("zero", ConversationState{}) // no expiry

	set := map[string]bool{}
	for _, k := range s.expiredKeys(now) {
		set[k] = true
	}

	asserts.True(t, set["at"], "ExpiresAt == now is expired")
	asserts.True(t, set["past"], "past ExpiresAt is expired")
	asserts.False(t, set["future"], "future ExpiresAt is not expired")
	asserts.False(t, set["zero"], "zero ExpiresAt never expires")
	asserts.Equal(t, len(set), 2, "exactly two keys expired")
}

func TestConversationManager_StripedLockBounded(t *testing.T) {
	m := newConversationManager()

	// Lock memory is bounded by construction: a fixed array, not a per-key map.
	asserts.Equal(t, len(m.locks), conversationShards, "fixed shard count")

	a := m.shardFor("alpha")
	b := m.shardFor("alpha")
	asserts.True(t, a == b, "the same key maps to the same shard deterministically")
}

func TestConversationManager_WithLockReleasesOnPanic(t *testing.T) {
	m := newConversationManager()

	func() {
		defer func() { _ = recover() }()
		m.withLock("k", func() { panic("boom") })
	}()

	// The shard must have been released; a second op on the same key (same shard)
	// must not deadlock.
	done := make(chan struct{})
	go func() {
		m.withLock("k", func() {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("withLock wedged the shard after a panic")
	}
}

func TestConversationManager_Sweep(t *testing.T) {
	m := newConversationManager()
	now := time.Now()
	m.store.Set("expired", ConversationState{FlowID: "f", ExpiresAt: now.Add(-time.Minute)})
	m.store.Set("fresh", ConversationState{FlowID: "f", ExpiresAt: now.Add(time.Minute)})
	m.store.Set("noexpiry", ConversationState{FlowID: "f"})

	m.sweep(now)

	_, ok := m.store.Get("expired")
	asserts.False(t, ok, "expired entry is swept")
	_, ok = m.store.Get("fresh")
	asserts.True(t, ok, "fresh entry survives")
	_, ok = m.store.Get("noexpiry")
	asserts.True(t, ok, "zero-ExpiresAt entry survives")
}

// fakeExpirer reports a fixed key set as expired regardless of the store's actual
// contents, letting a test drive sweep's under-lock re-check independently of the
// snapshot.
type fakeExpirer struct {
	*memConversationStore
	report []string
}

func (f *fakeExpirer) expiredKeys(time.Time) []string { return f.report }

func TestConversationManager_SweepRechecksUnderLock(t *testing.T) {
	store := newMemConversationStore()
	now := time.Now()
	// The snapshot claims "k" is expired, but the store holds a fresh "k" — as if
	// advance refreshed the TTL between the snapshot and the delete.
	store.Set("k", ConversationState{FlowID: "f", ExpiresAt: now.Add(time.Hour)})
	m := &conversationManager{store: &fakeExpirer{memConversationStore: store, report: []string{"k"}}}

	m.sweep(now)

	_, ok := m.store.Get("k")
	asserts.True(t, ok, "the under-lock re-check spares a still-fresh entry")
}

func TestConversationManager_SweeperLifecycleExits(t *testing.T) {
	m := newConversationManager()
	ctx, cancel := context.WithCancel(context.Background())

	done := m.startSweeper(ctx, 10*time.Millisecond, nil)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper goroutine did not exit after ctx cancel (leak)")
	}
}

func TestConversationManager_SweeperReapsExpired(t *testing.T) {
	m := newConversationManager()
	m.store.Set("k", ConversationState{FlowID: "f", ExpiresAt: time.Now().Add(-time.Minute)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.startSweeper(ctx, 5*time.Millisecond, nil)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := m.store.Get("k"); !ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("background sweeper did not reap the expired entry")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// recordingAdapter is a core.Adapter that records the prompts a flow sends, so a
// routing test can assert what reached the user.
type recordingAdapter struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingAdapter) Connect(context.Context, AdapterDeps) error { return nil }
func (r *recordingAdapter) Disconnect() error                          { return nil }
func (r *recordingAdapter) Attachments(*Message) ([]Attachment, error) { return nil, nil }
func (r *recordingAdapter) Send(_ context.Context, _, text string, _ SendOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, text)
	return nil
}

func (r *recordingAdapter) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func msgFrom(user, channel, content string) *Message {
	return &Message{UserID: user, ChannelID: channel, Content: content}
}

func TestConversationManager_AdvanceHappyPath(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()

	var completed Answers
	flow := &Flow{
		id:         "signup",
		steps:      []flowStep{{key: "name", prompt: "name?"}, {key: "color", prompt: "color?"}},
		onComplete: func(_ context.Context, _ *Bot, _ *Message, a Answers) { completed = a },
		cancelWord: "cancel",
	}
	bot.flows[flow.id] = flow

	// No active flow yet → advance does not consume the message.
	asserts.False(t, bot.conversations.advance(ctx, bot, msgFrom("u", "c", "hi")), "no active flow yet")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "signup"), flow)
	asserts.True(t, bot.conversations.advance(ctx, bot, msgFrom("u", "c", "Alice")), "first answer consumed")
	asserts.True(t, bot.conversations.advance(ctx, bot, msgFrom("u", "c", "blue")), "final answer consumed")

	asserts.Equal(t, completed.Get("name"), "Alice", "name captured")
	asserts.Equal(t, completed.Get("color"), "blue", "color captured")

	got := adapter.messages()
	asserts.Equal(t, len(got), 2, "exactly two prompts (no third after the last step)")
	asserts.Equal(t, got[0], "name?", "first prompt")
	asserts.Equal(t, got[1], "color?", "second prompt")

	_, ok := bot.conversations.store.Get(conversationKey(msgFrom("u", "c", "")))
	asserts.False(t, ok, "state cleared after completion")
}

func TestConversationManager_SecretAnswerKeepsExactBytes(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()

	var completed Answers
	flow := &Flow{
		id: "signup",
		steps: []flowStep{
			{key: "name", prompt: "name?"},
			{key: "password", prompt: "pw?", secret: true},
		},
		onComplete: func(_ context.Context, _ *Bot, _ *Message, a Answers) { completed = a },
	}
	bot.flows[flow.id] = flow

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "signup"), flow)
	// Ordinary step: surrounding whitespace is trimmed.
	asserts.True(t, bot.conversations.advance(ctx, bot, msgFrom("u", "c", "  Alice  ")), "name consumed")
	// Secret step: exact bytes (including surrounding spaces) are preserved.
	asserts.True(t, bot.conversations.advance(ctx, bot, msgFrom("u", "c", "  s3cret  ")), "password consumed")

	asserts.Equal(t, completed.Get("name"), "Alice", "ordinary answer is trimmed")
	asserts.Equal(t, completed.Get("password"), "  s3cret  ", "secret answer keeps exact bytes")
}

func TestConversationManager_StartSetIfAbsent(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()
	flow := &Flow{id: "f", steps: []flowStep{{key: "a", prompt: "a?"}}, onComplete: func(context.Context, *Bot, *Message, Answers) {}}
	bot.flows[flow.id] = flow

	m := msgFrom("u", "c", "f")
	bot.conversations.start(ctx, bot, m, flow)
	bot.conversations.start(ctx, bot, m, flow) // second start must be a no-op

	asserts.Equal(t, len(adapter.messages()), 1, "only one first prompt despite a double start")
}

func TestConversationManager_PerUserChannelKeying(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()

	var mu sync.Mutex
	results := map[string]string{}
	flow := &Flow{
		id:    "f",
		steps: []flowStep{{key: "a", prompt: "a?"}},
		onComplete: func(_ context.Context, _ *Bot, m *Message, a Answers) {
			mu.Lock()
			results[m.UserID+"/"+m.ChannelID] = a.Get("a")
			mu.Unlock()
		},
	}
	bot.flows[flow.id] = flow

	// Same user in two channels, plus a different user — three independent flows.
	bot.conversations.start(ctx, bot, msgFrom("u1", "c1", "f"), flow)
	bot.conversations.start(ctx, bot, msgFrom("u1", "c2", "f"), flow)
	bot.conversations.start(ctx, bot, msgFrom("u2", "c1", "f"), flow)

	bot.conversations.advance(ctx, bot, msgFrom("u1", "c1", "alpha"))
	bot.conversations.advance(ctx, bot, msgFrom("u1", "c2", "beta"))
	bot.conversations.advance(ctx, bot, msgFrom("u2", "c1", "gamma"))

	mu.Lock()
	defer mu.Unlock()
	asserts.Equal(t, results["u1/c1"], "alpha", "u1/c1 isolated")
	asserts.Equal(t, results["u1/c2"], "beta", "same user, different channel is isolated")
	asserts.Equal(t, results["u2/c1"], "gamma", "u2/c1 isolated")
}

func TestBot_dispatch_RoutesActiveFlow(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)

	mwCount := 0
	bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
		mwCount++
		next(ctx, b, m)
	})

	done := false
	flow := &Flow{
		id:         "signup",
		steps:      []flowStep{{key: "name", prompt: "name?"}, {key: "color", prompt: "color?"}},
		onComplete: func(context.Context, *Bot, *Message, Answers) { done = true },
		cancelWord: "cancel",
	}
	bot.flows[flow.id] = flow
	mustAddHandler(t, bot, "^signup$", func(ctx context.Context, b *Bot, m *Message) {
		b.conversations.start(ctx, b, m, flow)
	})
	fellThrough := false
	bot.SetUnknownCommandHandler(func(context.Context, *Bot, *Message) { fellThrough = true })

	send := func(text string) { bot.dispatch(context.Background(), msgFrom("u", "c", text)) }
	send("signup") // start
	send("Alice")  // step 1
	send("blue")   // step 2 → complete

	asserts.True(t, done, "OnComplete ran through dispatch routing")
	asserts.Equal(t, mwCount, 3, "middleware wraps every flow step")
	asserts.False(t, fellThrough, "flow answers never reach the unknown-command handler")
	asserts.Equal(t, len(adapter.messages()), 2, "two prompts sent")
}

func TestBot_SweeperLifecycle_ConnectDisconnect(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})

	// The sweeper only starts when at least one flow is registered, so register one
	// or this asserts nothing.
	f := NewFlow("f").Ask("a", "a?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register flow")

	// The recording adapter spawns no goroutine of its own, so Connect adds only
	// the sweeper; Disconnect cancels its run context and it must exit.
	before := runtime.NumGoroutine()
	asserts.NoError(t, bot.Connect(context.Background()), "connect")
	asserts.NoError(t, bot.Disconnect(), "disconnect")

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("sweeper goroutine leaked after Disconnect: before=%d now=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// panicStore is a ConversationStore whose Delete panics, used to prove the
// background sweeper recovers rather than crashing the process. expiredKeys/Get
// are promoted from the embedded memConversationStore.
type panicStore struct {
	*memConversationStore
}

func (p *panicStore) Delete(string) { panic("store delete exploded") }

func TestConversationManager_SweeperRecoversFromStorePanic(t *testing.T) {
	store := newMemConversationStore()
	store.Set("k", ConversationState{FlowID: "f", ExpiresAt: time.Now().Add(-time.Minute)})
	m := &conversationManager{store: &panicStore{memConversationStore: store}}

	// sweepRecovered must swallow the store's panic; if it propagated, the test
	// binary would crash here.
	m.sweepRecovered(time.Now())

	// Delete panicked before removing the entry, so it remains — and the manager
	// is still alive.
	_, ok := store.Get("k")
	asserts.True(t, ok, "a panicking Delete is recovered; the sweeper did not crash")
}

// TestConversationManager_SweepConcurrentWithAdvance hammers sweep against a
// goroutine that keeps refreshing a key's TTL under the shard lock. Both touch
// the same shard, so under -race this asserts no data race and that an actively
// refreshed entry is never reaped (the under-lock re-check always sees a future
// ExpiresAt).
func TestConversationManager_SweepConcurrentWithAdvance(t *testing.T) {
	m := newConversationManager()
	key := "u\x00c"
	m.store.Set(key, ConversationState{FlowID: "f", ExpiresAt: time.Now().Add(time.Hour)})

	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			m.withLock(key, func() {
				st, ok := m.store.Get(key)
				if !ok {
					st = ConversationState{FlowID: "f"}
				}
				st.ExpiresAt = time.Now().Add(time.Hour)
				m.store.Set(key, st)
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			m.sweep(time.Now())
		}
	}()
	wg.Wait()

	_, ok := m.store.Get(key)
	asserts.True(t, ok, "an actively-refreshed entry is never swept")
}

// TestConversationManager_ConcurrentWithLock proves the shard locks serialize the
// read-modify-write for a key: 50 increments across 5 keys land exactly once each
// with no lost updates. Run under -race to also assert no data race on the map.
func TestConversationManager_ConcurrentWithLock(t *testing.T) {
	m := newConversationManager()
	const goroutines = 50
	const keys = 5

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + strconv.Itoa(i%keys)
			m.withLock(key, func() {
				st, _ := m.store.Get(key)
				st.FlowID = "f"
				st.Step++
				m.store.Set(key, st)
			})
		}(i)
	}
	wg.Wait()

	total := 0
	for i := 0; i < keys; i++ {
		st, ok := m.store.Get("k" + strconv.Itoa(i))
		asserts.True(t, ok, "key should exist after concurrent writes")
		total += st.Step
	}
	asserts.Equal(t, total, goroutines, "every increment applied exactly once (no lost updates)")
}
