package core

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

// captureAdapter hands the deps it receives back to the test so it can drive
// dispatch directly, then behaves as a no-op adapter.
type captureAdapter struct{ deps chan AdapterDeps }

func (c *captureAdapter) Connect(_ context.Context, deps AdapterDeps) error {
	c.deps <- deps
	return nil
}
func (c *captureAdapter) Disconnect() error                          { return nil }
func (c *captureAdapter) Send(context.Context, string, string) error { return nil }
func (c *captureAdapter) Attachments(*Message) ([]Attachment, error) { return nil, nil }

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestShardIndex(t *testing.T) {
	const n = 8
	for _, id := range []string{"", "a", "channel-123", "999"} {
		got := shardIndex(id, n)
		asserts.True(t, got >= 0 && got < n, "shard in range for "+id)
		asserts.Equal(t, got, shardIndex(id, n), "shard is deterministic for "+id)
	}
}

func TestWithWorkers_Chains(t *testing.T) {
	bot := &Bot{}

	got := bot.WithWorkers(4)

	asserts.True(t, got == bot, "WithWorkers returns the same bot for chaining")
	asserts.Equal(t, bot.workers, 4, "worker count is recorded")
}

// With workers enabled, every message for a given channel must be handled in the
// order it was submitted (same channel -> same shard -> one worker), while
// different channels are free to run on different workers.
func TestBot_WithWorkers_PreservesPerChannelOrder(t *testing.T) {
	const (
		channels = 3
		perChan  = 40
	)

	a := &captureAdapter{deps: make(chan AdapterDeps, 1)}
	bot := New(CLIBotType, a).WithWorkers(4)

	var (
		mu  sync.Mutex
		seq = map[string][]int{}
		wg  sync.WaitGroup
	)
	wg.Add(channels * perChan)
	// Handlers are registered before Connect (the config phase), so the worker
	// goroutines started by Connect observe them without a data race.
	mustAddHandler(t, bot, ".*", func(_ context.Context, _ *Bot, m *Message) {
		n, _ := strconv.Atoi(m.Content)
		mu.Lock()
		seq[m.ChannelID] = append(seq[m.ChannelID], n)
		mu.Unlock()
		wg.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, bot.Connect(ctx), "connect")
	asserts.True(t, bot.pool.Load() != nil, "pool is active when workers > 1")
	deps := <-a.deps

	for i := 0; i < perChan; i++ {
		for c := 0; c < channels; c++ {
			deps.Dispatch(ctx, &Message{ChannelID: "chan-" + strconv.Itoa(c), Content: strconv.Itoa(i)})
		}
	}

	waitTimeout(t, &wg, 2*time.Second)

	mu.Lock()
	for c := 0; c < channels; c++ {
		got := seq["chan-"+strconv.Itoa(c)]
		asserts.Equal(t, len(got), perChan, "all messages handled for channel")
		for i, v := range got {
			asserts.Equal(t, v, i, "per-channel order preserved")
		}
	}
	mu.Unlock()

	asserts.NoError(t, bot.Disconnect(), "disconnect")
}

func TestBot_WithWorkers_DisabledRunsInline(t *testing.T) {
	a := &captureAdapter{deps: make(chan AdapterDeps, 1)}
	bot := New(CLIBotType, a).WithWorkers(1)

	called := false
	mustAddHandler(t, bot, ".*", func(context.Context, *Bot, *Message) { called = true })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asserts.NoError(t, bot.Connect(ctx), "connect")
	asserts.True(t, bot.pool.Load() == nil, "no pool when workers <= 1")
	deps := <-a.deps

	// Inline dispatch runs on the caller's goroutine, so the effect is visible
	// immediately after the call returns.
	deps.Dispatch(ctx, &Message{ChannelID: "c", Content: "x"})
	asserts.True(t, called, "inline dispatch runs synchronously")

	asserts.NoError(t, bot.Disconnect(), "disconnect")
}
