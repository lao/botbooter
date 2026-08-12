package botbooter_test

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/loadtest"
)

// TestSoak_ConcurrentDispatch_ExactCounts pumps many messages through a real Bot
// at high concurrency and asserts every one is handled exactly once (no lost or
// double dispatch), then checks the run leaks no goroutines. Runs under -race in
// the normal suite, so it also proves the lock-free command reads are safe at the
// facade level.
func TestSoak_ConcurrentDispatch_ExactCounts(t *testing.T) {
	before := runtime.NumGoroutine()
	bot, a := loadtest.New()

	var hits, miss atomic.Int64
	bot.HandleFunc("^ping$", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		hits.Add(1)
	})
	bot.SetUnknownCommandHandler(func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		miss.Add(1)
	})

	ctx := context.Background()
	asserts.NoError(t, bot.Connect(ctx), "connect")

	const total = 10000
	a.Pump(ctx, total, 64, func(i int) *botbooter.Message {
		if i%2 == 0 {
			return &botbooter.Message{Content: "ping"}
		}
		return &botbooter.Message{Content: "nope"}
	})

	asserts.NoError(t, bot.Disconnect(), "disconnect")

	asserts.Equal(t, hits.Load(), int64(total/2), "matched dispatches")
	asserts.Equal(t, miss.Load(), int64(total/2), "unmatched dispatches")
	loadtest.AssertNoGoroutineLeak(t, before)
}

// TestSoak_PanicHandlerDoesNotCorruptCounts drives a panicking handler under load
// to prove dispatch's recover keeps the pump alive and never corrupts the counts
// of well-behaved handlers. Each panic is logged by dispatch; that log noise is
// expected.
func TestSoak_PanicHandlerDoesNotCorruptCounts(t *testing.T) {
	bot, a := loadtest.New()

	var ok, boom atomic.Int64
	bot.HandleFunc("^ok$", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		ok.Add(1)
	})
	bot.HandleFunc("^boom$", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		boom.Add(1)
		panic("boom")
	})

	ctx := context.Background()
	asserts.NoError(t, bot.Connect(ctx), "connect")

	const total = 200
	a.Pump(ctx, total, 32, func(i int) *botbooter.Message {
		if i%2 == 0 {
			return &botbooter.Message{Content: "ok"}
		}
		return &botbooter.Message{Content: "boom"}
	})

	asserts.NoError(t, bot.Disconnect(), "disconnect")

	asserts.Equal(t, ok.Load(), int64(total/2), "well-behaved handler counts are intact")
	asserts.Equal(t, boom.Load(), int64(total/2), "panicking handler ran every time (panic recovered, not fatal)")
}
