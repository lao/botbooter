package core

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

func TestConversationManager_GetSetDel(t *testing.T) {
	m := newConversationManager()

	m.withLock(func() {
		_, ok := m.get("k")
		asserts.False(t, ok, "missing key reports absent")

		m.set("k", ConversationState{FlowID: "f", Step: 2})
		st, ok := m.get("k")
		asserts.True(t, ok, "present after set")
		asserts.Equal(t, st.FlowID, "f", "FlowID round-trips")
		asserts.Equal(t, st.Step, 2, "Step round-trips")

		m.del("k")
		_, ok = m.get("k")
		asserts.False(t, ok, "absent after del")
	})
}

func TestConversationManager_WithLockReleasesOnPanic(t *testing.T) {
	m := newConversationManager()

	func() {
		defer func() { _ = recover() }()
		m.withLock(func() { panic("boom") })
	}()

	// The mutex must have been released; a second op must not deadlock.
	done := make(chan struct{})
	go func() {
		m.withLock(func() {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("withLock wedged the mutex after a panic")
	}
}

func TestConversationManager_Sweep(t *testing.T) {
	m := newConversationManager()
	now := time.Now()
	m.withLock(func() {
		m.set("expired", ConversationState{FlowID: "f", ExpiresAt: now.Add(-time.Minute)})
		m.set("fresh", ConversationState{FlowID: "f", ExpiresAt: now.Add(time.Minute)})
		m.set("noexpiry", ConversationState{FlowID: "f"})
	})

	m.sweep(now)

	m.withLock(func() {
		_, ok := m.get("expired")
		asserts.False(t, ok, "expired entry is swept")
		_, ok = m.get("fresh")
		asserts.True(t, ok, "fresh entry survives")
		_, ok = m.get("noexpiry")
		asserts.True(t, ok, "zero-ExpiresAt entry survives")
	})
}

func TestConversationManager_SweeperLifecycleExits(t *testing.T) {
	m := newConversationManager()
	ctx, cancel := context.WithCancel(context.Background())

	done := m.startSweeper(ctx, 10*time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper goroutine did not exit after ctx cancel (leak)")
	}
}

func TestConversationManager_SweeperReapsExpired(t *testing.T) {
	m := newConversationManager()
	m.withLock(func() {
		m.set("k", ConversationState{FlowID: "f", ExpiresAt: time.Now().Add(-time.Minute)})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.startSweeper(ctx, 5*time.Millisecond)

	deadline := time.After(2 * time.Second)
	for {
		var present bool
		m.withLock(func() { _, present = m.get("k") })
		if !present {
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

	var present bool
	bot.conversations.withLock(func() {
		_, present = bot.conversations.get(conversationKey(msgFrom("u", "c", "")))
	})
	asserts.False(t, present, "state cleared after completion")
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
	// Register through the production HandleFlow wiring so dispatch invokes the
	// real start closure (flow.go), not a hand-rolled handler.
	flow := NewFlow("signup").
		Ask("name", "name?").
		Ask("color", "color?").
		OnComplete(func(context.Context, *Bot, *Message, Answers) { done = true })
	asserts.NoError(t, bot.HandleFlow("^signup$", flow), "register flow")
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

	asserts.NoError(t, bot.Connect(context.Background()), "connect")
	c := bot.currentConn()
	asserts.True(t, c.sweeperDone != nil, "Connect starts the sweeper for a bot with flows")
	asserts.NoError(t, bot.Disconnect(), "disconnect")

	// Disconnect cancels the run context; observe the sweeper goroutine actually
	// exit through the channel it closes on return — leak-free, and without polling
	// the whole process's goroutine count.
	select {
	case <-c.sweeperDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper goroutine did not exit after Disconnect (leak)")
	}
}

// TestConversationManager_SweepConcurrentWithAdvance hammers sweep against a
// goroutine that keeps refreshing a key's TTL under the manager lock. Under -race
// this asserts no data race and that an actively refreshed entry is never reaped
// (sweep holds the lock across its scan, so it always sees a future ExpiresAt).
func TestConversationManager_SweepConcurrentWithAdvance(t *testing.T) {
	m := newConversationManager()
	key := "u\x00c"
	m.withLock(func() {
		m.set(key, ConversationState{FlowID: "f", ExpiresAt: time.Now().Add(time.Hour)})
	})

	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			m.withLock(func() {
				st, ok := m.get(key)
				if !ok {
					st = ConversationState{FlowID: "f"}
				}
				st.ExpiresAt = time.Now().Add(time.Hour)
				m.set(key, st)
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

	var ok bool
	m.withLock(func() { _, ok = m.get(key) })
	asserts.True(t, ok, "an actively-refreshed entry is never swept")
}

// TestConversationManager_ConcurrentWithLock proves the manager lock serializes the
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
			m.withLock(func() {
				st, _ := m.get(key)
				st.FlowID = "f"
				st.Step++
				m.set(key, st)
			})
		}(i)
	}
	wg.Wait()

	total := 0
	m.withLock(func() {
		for i := 0; i < keys; i++ {
			st, ok := m.get("k" + strconv.Itoa(i))
			asserts.True(t, ok, "key should exist after concurrent writes")
			total += st.Step
		}
	})
	asserts.Equal(t, total, goroutines, "every increment applied exactly once (no lost updates)")
}
