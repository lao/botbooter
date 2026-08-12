package core

import (
	"context"
	"fmt"
	"testing"
)

// benchBot builds a Bot with numCommands mutually-exclusive "^cmdNNNN$" patterns
// and numMiddleware pass-through middlewares, all with no-op handlers so handler
// work never dominates the measurement. When matchFirst is true the returned
// message matches commands[0] (best case: dispatch stops at the first command);
// otherwise it matches none, so dispatch scans every command and falls through
// to the unknown-command handler (worst case for the linear scan).
//
// The Bot is connected before it is returned. That is what makes these numbers
// production-representative: Connect composes the middleware chain once and
// stores it, so dispatch takes the atomic-load fast path. Benchmarking an
// unconnected Bot would instead measure dispatch's compose-on-the-fly fallback
// and charge every iteration for a chain rebuild no real message ever pays for.
func benchBot(b *testing.B, numCommands, numMiddleware int, matchFirst bool) (*Bot, *Message) {
	b.Helper()
	bot := New(CLIBotType, &stubAdapter{})
	noop := func(_ context.Context, _ *Bot, _ *Message) {}
	for i := 0; i < numCommands; i++ {
		bot.AddHandler(Command{Pattern: fmt.Sprintf("^cmd%04d$", i), Handler: noop})
	}
	bot.SetUnknownCommandHandler(noop)
	for i := 0; i < numMiddleware; i++ {
		bot.AddMiddleware(func(ctx context.Context, bot *Bot, m *Message, next CommandHandler) {
			next(ctx, bot, m)
		})
	}
	if err := bot.Connect(context.Background()); err != nil {
		b.Fatalf("connect bench bot: %v", err)
	}
	b.Cleanup(func() { _ = bot.Disconnect() })
	content := "nomatchcontent"
	if matchFirst {
		content = "cmd0000"
	}
	return bot, &Message{Content: content}
}

func benchDispatch(b *testing.B, bot *Bot, msg *Message) {
	b.Helper()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bot.dispatch(ctx, msg)
	}
}

// BenchmarkDispatch_SingleCommand_Match is the baseline: one command, no
// middleware, message matches it.
func BenchmarkDispatch_SingleCommand_Match(b *testing.B) {
	bot, msg := benchBot(b, 1, 0, true)
	benchDispatch(b, bot, msg)
}

// BenchmarkDispatch_SingleCommand_Unknown measures the fall-through cost: one
// command, no match, unknown-command handler invoked.
func BenchmarkDispatch_SingleCommand_Unknown(b *testing.B) {
	bot, msg := benchBot(b, 1, 0, false)
	benchDispatch(b, bot, msg)
}

// BenchmarkDispatch_ScaleCommands proves the O(#commands) regex scan: the
// message matches none, so every command is tested on each dispatch.
func BenchmarkDispatch_ScaleCommands(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("cmds=%d", n), func(b *testing.B) {
			bot, msg := benchBot(b, n, 0, false)
			benchDispatch(b, bot, msg)
		})
	}
}

// BenchmarkDispatch_ScaleCommands_MatchFirst is the O(1) contrast: the message
// matches commands[0] regardless of how many are registered, isolating the scan
// cost when compared against BenchmarkDispatch_ScaleCommands.
func BenchmarkDispatch_ScaleCommands_MatchFirst(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("cmds=%d", n), func(b *testing.B) {
			bot, msg := benchBot(b, n, 0, true)
			benchDispatch(b, bot, msg)
		})
	}
}

// BenchmarkDispatch_ScaleMiddleware quantifies the per-message cost of walking an
// N-deep middleware chain. The chain itself is composed once at Connect, so this
// measures call depth, not construction: allocs/op stays at 0 no matter how many
// middlewares are registered, and only time/op grows.
func BenchmarkDispatch_ScaleMiddleware(b *testing.B) {
	for _, n := range []int{0, 1, 5, 10, 50} {
		b.Run(fmt.Sprintf("mw=%d", n), func(b *testing.B) {
			bot, msg := benchBot(b, 1, n, true)
			benchDispatch(b, bot, msg)
		})
	}
}

// BenchmarkDispatch_Parallel measures concurrent throughput over the lock-free
// command reads and the atomically-loaded dispatch chain. Run with -cpu=1,4,8 to
// confirm it scales with cores (no lock contention).
func BenchmarkDispatch_Parallel(b *testing.B) {
	bot, msg := benchBot(b, 100, 0, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bot.dispatch(ctx, msg)
		}
	})
}
