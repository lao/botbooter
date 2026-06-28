package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestDispatch_ConcurrentReads_RaceFree fires many concurrent dispatches through
// one Bot to prove that the command/middleware reads in dispatch are race-free
// (they take no lock; registration happens before any dispatch). Run under
// -race. The exact hit/miss counts also prove no message is lost or
// double-handled under concurrency.
func TestDispatch_ConcurrentReads_RaceFree(t *testing.T) {
	const (
		workers    = 64
		iterations = 200 // even: each worker emits an equal match/miss split
	)

	bot := New(CLIBotType, nil)
	var hits, miss atomic.Int64

	noop := func(_ context.Context, _ *Bot, _ *Message) {}
	if err := bot.AddHandler(Command{Pattern: "^ping$", Handler: func(_ context.Context, _ *Bot, _ *Message) {
		hits.Add(1)
	}}); err != nil {
		t.Fatalf("AddHandler ping: %v", err)
	}
	// Extra non-matching commands so each dispatch scans a multi-entry slice.
	for i := 0; i < 4; i++ {
		if err := bot.AddHandler(Command{Pattern: fmt.Sprintf("^extra%d$", i), Handler: noop}); err != nil {
			t.Fatalf("AddHandler extra%d: %v", i, err)
		}
	}
	bot.SetUnknownCommandHandler(func(_ context.Context, _ *Bot, _ *Message) {
		miss.Add(1)
	})
	// A couple of pass-through middlewares so the chain is rebuilt concurrently too.
	for i := 0; i < 3; i++ {
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			next(ctx, b, m)
		})
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				content := "ping"
				if i%2 == 1 {
					content = "nope"
				}
				bot.dispatch(ctx, &Message{Content: content})
			}
		}()
	}
	wg.Wait()

	const expected = int64(workers * iterations / 2)
	asserts.Equal(t, hits.Load(), expected, "matched dispatches")
	asserts.Equal(t, miss.Load(), expected, "unmatched dispatches")
	asserts.Equal(t, hits.Load()+miss.Load(), int64(workers*iterations), "every dispatch accounted for")
}
