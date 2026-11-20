// Package loadtest provides an in-memory core.Adapter and helpers for
// botbooter's load, soak and overload tests. It performs no network I/O: it
// captures the Bot's dispatch callback at Connect and pumps synthetic messages
// through the real command/middleware pipeline at configurable concurrency.
package loadtest

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

// Adapter is a core.Adapter that drives a real Bot's dispatch pipeline from a
// test. It sends nothing over a network: Send discards its message.
type Adapter struct {
	dispatch func(ctx context.Context, m *core.Message)
}

// New returns a real *core.Bot wired to a fresh Adapter, plus that Adapter.
// Because botbooter.Bot is a type alias for core.Bot, the returned *core.Bot is
// usable directly as a *botbooter.Bot in facade-level tests.
func New() (*core.Bot, *Adapter) {
	a := &Adapter{}
	return core.New(core.CLIBotType, a), a
}

// Connect captures the dispatch callback so Pump and Dispatch can drive the
// pipeline. It is called by Bot.Connect on the test goroutine before Pump spawns
// any workers; goroutine creation is a happens-before edge, so the unsynchronized
// field write is safe to read from the workers and stays clean under -race.
func (a *Adapter) Connect(_ context.Context, deps core.AdapterDeps) error {
	a.dispatch = deps.Dispatch
	return nil
}

// Disconnect satisfies core.Adapter; there is nothing to tear down.
func (a *Adapter) Disconnect() error { return nil }

// Send discards the message.
func (a *Adapter) Send(_ context.Context, _, _ string, _ core.SendOptions) error {
	return nil
}

// Attachments satisfies core.Adapter; synthetic messages carry no attachments.
func (a *Adapter) Attachments(_ *core.Message) ([]core.Attachment, error) {
	return nil, nil
}

// Pump dispatches total messages across workers goroutines and blocks until all
// are processed. build constructs the i-th message (0 <= i < total), letting a
// test mix matching and non-matching content. The Bot must be connected first.
func (a *Adapter) Pump(ctx context.Context, total, workers int, build func(i int) *core.Message) {
	if workers < 1 {
		workers = 1
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(total) {
					return
				}
				a.dispatch(ctx, build(int(i)))
			}
		}()
	}
	wg.Wait()
}

// Gauge tracks the current and peak number of concurrent occupants. A handler
// brackets its body with Enter and Leave; Peak reports the maximum simultaneous
// in-flight handlers observed, which reveals how much concurrency the caller
// applied (no backpressure means the gauge peaks at the driver's worker count).
type Gauge struct {
	cur  atomic.Int64
	peak atomic.Int64
}

// Enter marks one occupant as in-flight and raises the peak if needed.
func (g *Gauge) Enter() {
	c := g.cur.Add(1)
	for {
		p := g.peak.Load()
		if c <= p || g.peak.CompareAndSwap(p, c) {
			return
		}
	}
}

// Leave marks one occupant as finished.
func (g *Gauge) Leave() { g.cur.Add(-1) }

// Peak returns the maximum concurrent occupancy observed so far.
func (g *Gauge) Peak() int64 { return g.peak.Load() }

// AssertNoGoroutineLeak fails t if the live goroutine count does not settle back
// to at most before within roughly a second — teardown is asynchronous, so the
// count is polled rather than sampled once. Do not run callers with t.Parallel,
// since runtime.NumGoroutine is process-global.
func AssertNoGoroutineLeak(t asserts.TestingT, before int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: started at %d, still %d after settle window", before, runtime.NumGoroutine())
}
