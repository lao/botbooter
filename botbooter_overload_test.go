package botbooter_test

// These tests document botbooter's overload behavior. core.dispatch is
// synchronous and applies NO queue, rate limit or backpressure: the concurrency
// and head-of-line blocking a bot exhibits under load are determined entirely by
// how its adapter calls dispatch. A single-goroutine adapter (Slack, CLI)
// serializes handlers, so one slow handler blocks the whole stream; a
// callback-per-message adapter (Discord, Telegram) runs handlers concurrently
// with no upper bound. Each test pins one of those patterns with a deterministic
// channel barrier so the assertions never depend on wall-clock timing.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/loadtest"
)

// TestOverload_SequentialDriver_HeadOfLineBlocking models a single-goroutine
// adapter (Slack/CLI): with one driver goroutine, a slow handler serializes the
// stream, so at most one handler is ever in flight.
func TestOverload_SequentialDriver_HeadOfLineBlocking(t *testing.T) {
	bot, a := loadtest.New()
	var g loadtest.Gauge
	var processed atomic.Int64
	asserts.NoError(t, bot.HandleFunc("^msg$", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		g.Enter()
		time.Sleep(time.Millisecond) // stand in for slow work
		g.Leave()
		processed.Add(1)
	}), "register handler")

	ctx := context.Background()
	asserts.NoError(t, bot.Connect(ctx), "connect")

	const total = 50
	start := time.Now()
	a.Pump(ctx, total, 1, func(int) *botbooter.Message {
		return &botbooter.Message{Content: "msg"}
	})
	elapsed := time.Since(start)

	asserts.NoError(t, bot.Disconnect(), "disconnect")

	asserts.Equal(t, processed.Load(), int64(total), "all messages processed")
	asserts.Equal(t, g.Peak(), int64(1), "single-goroutine driver serializes handlers (head-of-line blocking)")
	t.Logf("sequential: %d msgs in %v (%.0f msgs/sec)", total, elapsed, float64(total)/elapsed.Seconds())
}

// TestOverload_ConcurrentDriver_NoBackpressure models a callback-per-message
// adapter (Discord/Telegram): W driver goroutines each block inside a handler, so
// all W handlers run at once — core never throttles them. The handlers park on a
// release channel until the test confirms the peak, making the assertion
// timing-independent.
func TestOverload_ConcurrentDriver_NoBackpressure(t *testing.T) {
	const workers = 32
	bot, a := loadtest.New()
	var g loadtest.Gauge
	var processed atomic.Int64
	release := make(chan struct{})
	asserts.NoError(t, bot.HandleFunc("^msg$", func(_ context.Context, _ *botbooter.Bot, _ *botbooter.Message) {
		g.Enter()
		<-release // hold the slot until every worker is in flight
		g.Leave()
		processed.Add(1)
	}), "register handler")

	ctx := context.Background()
	asserts.NoError(t, bot.Connect(ctx), "connect")

	done := make(chan struct{})
	go func() {
		// total == workers, so each worker dispatches exactly one message and
		// blocks in the handler; no worker is free to dispatch a second.
		a.Pump(ctx, workers, workers, func(int) *botbooter.Message {
			return &botbooter.Message{Content: "msg"}
		})
		close(done)
	}()

	waitForPeak(t, &g, workers)
	close(release)
	<-done

	asserts.NoError(t, bot.Disconnect(), "disconnect")

	asserts.Equal(t, processed.Load(), int64(workers), "all messages processed")
	asserts.Equal(t, g.Peak(), int64(workers), "concurrent driver runs all handlers at once (no backpressure)")
}

// waitForPeak blocks until the gauge reports at least want concurrent occupants,
// failing the test if that does not happen within a generous window.
func waitForPeak(t *testing.T, g *loadtest.Gauge, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g.Peak() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d concurrent handlers; peak was %d", want, g.Peak())
}
