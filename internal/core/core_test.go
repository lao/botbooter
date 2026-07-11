package core

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

func noopHandler(ctx context.Context, b *Bot, m *Message) {}

func mustAddHandler(t *testing.T, bot *Bot, pattern string, handler CommandHandler) {
	t.Helper()
	bot.AddHandler(Command{Pattern: pattern, Handler: handler})
	asserts.Equal(t, len(bot.setupErrs), 0, "AddHandler "+pattern)
}

func TestBotType_String(t *testing.T) {
	asserts.Equal(t, SlackBotType.String(), "slack", "Slack string")
	asserts.Equal(t, DiscordBotType.String(), "discord", "Discord string")
	asserts.Equal(t, CLIBotType.String(), "cli", "CLI string")
	asserts.Equal(t, TelegramBotType.String(), "telegram", "Telegram string")
	asserts.Equal(t, WhatsAppBotType.String(), "whatsapp", "WhatsApp string")
	asserts.Equal(t, TeamsBotType.String(), "teams", "Teams string")
	asserts.Equal(t, GitHubBotType.String(), "github", "GitHub string")
	asserts.Equal(t, BotType(999).String(), "BotType(999)", "unknown string")
}

func TestBot_AddHandler(t *testing.T) {
	t.Run("ValidPattern", func(t *testing.T) {
		bot := &Bot{}

		bot.AddHandler(Command{Pattern: "^hello$", Handler: noopHandler})

		asserts.Equal(t, len(bot.setupErrs), 0, "AddHandler with valid pattern")
		asserts.Equal(t, len(bot.commands), 1, "Number of commands after adding handler")
		asserts.Equal(t, bot.commands[0].Pattern, "^hello$", "Handler pattern")
		asserts.NotNil(t, bot.commands[0].re, "pattern should be precompiled")
	})

	t.Run("InvalidPattern", func(t *testing.T) {
		bot := &Bot{}

		bot.AddHandler(Command{Pattern: "[invalid(", Handler: noopHandler})

		asserts.Equal(t, len(bot.setupErrs), 1, "invalid pattern should be recorded")
		asserts.Equal(t, len(bot.commands), 0, "invalid command should not be added")
	})

	// Connect surfaces setupErrs, but a pattern registered AFTER a successful
	// Connect is never read again — the record-time log is the only signal, so
	// assert it fires at AddHandler time.
	t.Run("InvalidPatternLogsAtRecordTime", func(t *testing.T) {
		var buf bytes.Buffer
		bot := &Bot{}
		bot.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

		bot.AddHandler(Command{Pattern: "[invalid(", Handler: noopHandler})

		asserts.Equal(t, len(bot.setupErrs), 1, "invalid pattern should still be recorded")
		asserts.True(t, strings.Contains(buf.String(), "invalid command pattern"),
			"AddHandler logs the invalid pattern immediately")
		asserts.True(t, strings.Contains(buf.String(), "[invalid("),
			"the offending pattern is included in the log")
	})
}

func TestBot_HandleFunc(t *testing.T) {
	bot := &Bot{}
	called := false
	bot.HandleFunc("^hi$", func(ctx context.Context, b *Bot, m *Message) {
		called = true
	})

	bot.dispatch(context.Background(), &Message{Content: "hi"})
	asserts.True(t, called, "HandleFunc handler should be called")

	bot.HandleFunc("[bad(", noopHandler)
	asserts.Equal(t, len(bot.setupErrs), 1, "HandleFunc with invalid pattern should be recorded")
}

func TestBot_Connect_InvalidPatterns(t *testing.T) {
	t.Run("SurfacesFromConnect", func(t *testing.T) {
		bot := New(CLIBotType, &stubAdapter{})
		bot.HandleFunc("^ok$", noopHandler)
		bot.HandleFunc("[invalid(", noopHandler)

		err := bot.Connect(context.Background())

		asserts.Error(t, err, "Connect with an invalid pattern should fail")
		asserts.True(t, strings.Contains(err.Error(), `invalid command pattern "[invalid("`),
			"error should name the offending pattern")
		asserts.Equal(t, bot.conn == nil, true, "no connection should be installed")
	})

	t.Run("JoinsAllPatterns", func(t *testing.T) {
		bot := New(CLIBotType, &stubAdapter{})
		bot.HandleFunc("[bad-one(", noopHandler)
		bot.HandleFunc("[bad-two(", noopHandler)

		err := bot.Connect(context.Background())

		asserts.Error(t, err, "Connect with invalid patterns should fail")
		asserts.True(t, strings.Contains(err.Error(), `"[bad-one("`), "first pattern reported")
		asserts.True(t, strings.Contains(err.Error(), `"[bad-two("`), "second pattern reported")
	})

	t.Run("SurfacesFromRun", func(t *testing.T) {
		bot := New(CLIBotType, &stubAdapter{})
		bot.HandleFunc("[bad(", noopHandler)

		err := bot.Run(context.Background())

		asserts.Error(t, err, "Run with an invalid pattern should fail")
		asserts.True(t, strings.Contains(err.Error(), `invalid command pattern "[bad("`),
			"error should name the offending pattern")
	})
}

func TestBot_SetUnknownCommandHandler(t *testing.T) {
	bot := &Bot{}

	bot.SetUnknownCommandHandler(noopHandler)

	asserts.NotNil(t, bot.unknownCommandHandler, "UnknownCommandHandler should be set")
}

func TestBot_AddMiddleware(t *testing.T) {
	bot := &Bot{}

	bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
		next(ctx, b, m)
	})

	asserts.Equal(t, len(bot.middlewares), 1, "Number of middlewares after adding")
}

func TestBot_dispatch(t *testing.T) {
	t.Run("MatchingCommand", func(t *testing.T) {
		bot := &Bot{}
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.True(t, handlerCalled, "Handler should be called for matching command")
	})

	t.Run("NoMatchingCommand", func(t *testing.T) {
		bot := &Bot{}
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})
		unknownCalled := false
		bot.SetUnknownCommandHandler(func(ctx context.Context, b *Bot, m *Message) {
			unknownCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		asserts.False(t, handlerCalled, "Handler should not be called for non-matching command")
		asserts.True(t, unknownCalled, "Unknown handler should be called")
	})

	t.Run("WithMiddleware", func(t *testing.T) {
		bot := &Bot{}
		middlewareCalled := false
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			middlewareCalled = true
			next(ctx, b, m)
		})
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.True(t, middlewareCalled, "Middleware should be called")
		asserts.True(t, handlerCalled, "Handler should be called after middleware")
	})

	t.Run("MultipleMiddlewares", func(t *testing.T) {
		bot := &Bot{}
		var callOrder []int
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			callOrder = append(callOrder, 1)
			next(ctx, b, m)
		})
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			callOrder = append(callOrder, 2)
			next(ctx, b, m)
		})
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			callOrder = append(callOrder, 3)
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.Equal(t, len(callOrder), 3, "Number of calls")
		asserts.Equal(t, callOrder[0], 1, "First middleware should be called first")
		asserts.Equal(t, callOrder[1], 2, "Second middleware should be called second")
		asserts.Equal(t, callOrder[2], 3, "Handler should be called last")
	})

	t.Run("MiddlewareShortCircuit", func(t *testing.T) {
		bot := &Bot{}
		bot.AddMiddleware(func(ctx context.Context, b *Bot, m *Message, next CommandHandler) {
			// Intentionally does not call next.
		})
		handlerCalled := false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			handlerCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "hello"})

		asserts.False(t, handlerCalled, "Handler should not run when middleware short-circuits")
	})

	t.Run("NoUnknownHandler", func(t *testing.T) {
		bot := &Bot{}
		mustAddHandler(t, bot, "^hello$", noopHandler)

		// Should not panic without an unknown command handler.
		bot.dispatch(context.Background(), &Message{Content: "goodbye"})
	})

	t.Run("FirstMatchWins", func(t *testing.T) {
		bot := &Bot{}
		firstCalled, secondCalled := false, false
		mustAddHandler(t, bot, "^hello$", func(ctx context.Context, b *Bot, m *Message) {
			firstCalled = true
		})
		mustAddHandler(t, bot, "^goodbye$", func(ctx context.Context, b *Bot, m *Message) {
			secondCalled = true
		})

		bot.dispatch(context.Background(), &Message{Content: "goodbye"})

		asserts.False(t, firstCalled, "First handler should not be called")
		asserts.True(t, secondCalled, "Second handler should be called")
	})
}

func TestBot_dispatch_RecoversFromPanic(t *testing.T) {
	bot := &Bot{}
	mustAddHandler(t, bot, "^boom$", func(ctx context.Context, b *Bot, m *Message) {
		panic("handler exploded")
	})

	// dispatch must recover; the call should return normally rather than
	// propagating the panic and crashing the event loop.
	bot.dispatch(context.Background(), &Message{Content: "boom"})
}

// log falls back to slog.Default when no logger is injected, and returns the
// injected logger once SetLogger is called.
func TestBot_log_FallbackAndInjected(t *testing.T) {
	bot := &Bot{}
	asserts.Equal(t, bot.log(), slog.Default(), "unset logger falls back to slog.Default")

	custom := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	bot.SetLogger(custom)
	asserts.Equal(t, bot.log(), custom, "SetLogger makes log return the injected logger")
}

// SetLogger routes dispatch's panic-recovery diagnostic to the injected logger
// rather than slog.Default, proving the logger is actually threaded into
// dispatch and not just stored.
func TestBot_SetLogger_RoutesPanicRecovery(t *testing.T) {
	var buf bytes.Buffer
	bot := &Bot{}
	bot.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	mustAddHandler(t, bot, "^boom$", func(ctx context.Context, b *Bot, m *Message) {
		panic("handler exploded")
	})

	bot.dispatch(context.Background(), &Message{Content: "boom"})

	asserts.True(t, strings.Contains(buf.String(), "recovered from panic"),
		"injected logger captures the panic-recovery diagnostic")
	asserts.True(t, strings.Contains(buf.String(), "handler exploded"),
		"the panic value is included in the diagnostic")
}

// An unknown bot type has no adapter, so every lifecycle method reports
// ErrUnknownBotType rather than acting on a platform.

func TestBot_Connect_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.Connect(context.Background())

	asserts.ErrorIs(t, err, ErrUnknownBotType, "Connect with unknown bot type")
}

func TestBot_Disconnect_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.Disconnect()

	asserts.ErrorIs(t, err, ErrUnknownBotType, "Disconnect with unknown bot type")
}

func TestBot_SendMessage_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.SendMessage("channel123", "test message")

	asserts.ErrorIs(t, err, ErrUnknownBotType, "SendMessage with unknown bot type")
}

func TestBot_GetAttachments_UnknownBotType(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	attachments, err := bot.GetAttachments(&Message{})

	asserts.ErrorIs(t, err, ErrUnknownBotType, "GetAttachments with unknown bot type")
	asserts.Equal(t, len(attachments), 0, "Attachments should be empty for unknown bot type")
}

// teardown's adapter-nil branch is defensive (Connect never installs a
// connection without an adapter), so exercise it directly.
func TestConnection_TeardownNilAdapter(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	c := &connection{cancel: cancel, runDone: make(chan struct{})}

	err := c.teardown(true)

	asserts.ErrorIs(t, err, ErrUnknownBotType, "teardown with no adapter")
}

func TestBot_GetAttachments_NilMessage(t *testing.T) {
	bot := New(SlackBotType, &recordingStub{})

	attachments, err := bot.GetAttachments(nil)

	asserts.ErrorIs(t, err, ErrNilMessage, "GetAttachments with a nil message returns ErrNilMessage")
	asserts.Equal(t, len(attachments), 0, "no attachments for a nil message")
}

// stubAdapter is a minimal core.Adapter used to exercise AdapterAs.
type stubAdapter struct{ name string }

func (s *stubAdapter) Connect(context.Context, AdapterDeps) error              { return nil }
func (s *stubAdapter) Disconnect() error                                       { return nil }
func (s *stubAdapter) Send(context.Context, string, string, SendOptions) error { return nil }
func (s *stubAdapter) Attachments(*Message) ([]Attachment, error)              { return nil, nil }

type adapterMismatch struct{}

func (a *adapterMismatch) Connect(context.Context, AdapterDeps) error              { return nil }
func (a *adapterMismatch) Disconnect() error                                       { return nil }
func (a *adapterMismatch) Send(context.Context, string, string, SendOptions) error { return nil }
func (a *adapterMismatch) Attachments(*Message) ([]Attachment, error)              { return nil, nil }

func TestAdapterAs(t *testing.T) {
	stub := &stubAdapter{name: "x"}
	bot := New(SlackBotType, stub)

	got, ok := AdapterAs[*stubAdapter](bot)
	asserts.True(t, ok, "AdapterAs should recover the concrete adapter type")
	asserts.Equal(t, got.name, "x", "recovered adapter identity")

	_, ok = AdapterAs[*adapterMismatch](bot)
	asserts.False(t, ok, "AdapterAs should report false for a different type")
}

// resolverStub is a stubAdapter that also implements AttachmentResolver, recording
// the context it was handed so a test can assert ctx is threaded through.
type resolverStub struct {
	stubAdapter
	url    string
	err    error
	gotCtx context.Context
	called bool
}

func (r *resolverStub) ResolveAttachmentURL(ctx context.Context, _ Attachment) (string, error) {
	r.called = true
	r.gotCtx = ctx
	return r.url, r.err
}

// recordingStub is a stubAdapter that records what its Send received, so a test
// can assert the resolved SendOptions the Bot forwarded to the adapter.
type recordingStub struct {
	stubAdapter
	gotChannel string
	gotText    string
	gotOpts    SendOptions
	calls      int
}

func (r *recordingStub) Send(_ context.Context, channelID, text string, opts SendOptions) error {
	r.gotChannel = channelID
	r.gotText = text
	r.gotOpts = opts
	r.calls++
	return nil
}

func TestResolveSendOptions_PrecedenceAndNilSkip(t *testing.T) {
	m := &Message{ID: "1.0", ReplyToID: "0.9"}

	// resolveSendOptions folds each option into its own field (the adapters apply
	// ThreadID-over-ReplyTo precedence, tested per-adapter); a nil option is skipped.
	got := resolveSendOptions(InReplyTo(m), nil, WithThreadID("RAW"))
	asserts.Equal(t, got.ThreadID, "RAW", "ThreadID field carried")
	asserts.Equal(t, got.ReplyTo, m, "ReplyTo field carried alongside ThreadID")

	// No options yields the zero value; a lone nil option is a no-op.
	asserts.Equal(t, resolveSendOptions().ThreadID, "", "no options yields the zero SendOptions")
	asserts.True(t, resolveSendOptions(nil).ReplyTo == nil, "a lone nil option is a no-op")
}

func TestBot_SendMessageContext_ForwardsResolvedOptions(t *testing.T) {
	stub := &recordingStub{}
	bot := New(SlackBotType, stub)
	m := &Message{ChannelID: "C1", ID: "1.0", ReplyToID: "0.9"}

	asserts.NoError(t, bot.SendMessageContext(context.Background(), "C1", "hi", InReplyTo(m)),
		"SendMessageContext with an option")
	asserts.Equal(t, stub.gotOpts.ReplyTo, m, "adapter received the ReplyTo anchor")
	asserts.Equal(t, stub.gotText, "hi", "adapter received the text")

	// No options → zero SendOptions reaches the adapter (plain send).
	stub.gotOpts = SendOptions{ReplyTo: m}
	asserts.NoError(t, bot.SendMessageContext(context.Background(), "C1", "plain"), "plain send")
	asserts.True(t, stub.gotOpts.ReplyTo == nil, "a plain send forwards the zero SendOptions")
}

func TestBot_Reply_ForwardsInReplyTo(t *testing.T) {
	stub := &recordingStub{}
	bot := New(SlackBotType, stub)
	m := &Message{ChannelID: "C1", ID: "1.0", ReplyToID: "0.9"}

	asserts.NoError(t, bot.Reply(context.Background(), m, "hi"), "Reply")
	asserts.Equal(t, stub.gotChannel, "C1", "Reply sends to the message's channel")
	asserts.Equal(t, stub.gotOpts.ReplyTo, m, "Reply forwards InReplyTo(m)")
	asserts.Equal(t, stub.calls, 1, "Reply sends exactly once")
}

func TestBot_Reply_NilAdapter(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.Reply(context.Background(), &Message{ID: "1"}, "hi")
	asserts.ErrorIs(t, err, ErrUnknownBotType, "Reply with no adapter")
}

func TestBot_Reply_NilMessage(t *testing.T) {
	bot := New(SlackBotType, &recordingStub{})

	err := bot.Reply(context.Background(), nil, "hi")
	asserts.ErrorIs(t, err, ErrNilMessage, "Reply with a nil message returns ErrNilMessage, not a bot-type error")
}

func TestBot_ResolveAttachmentURL_NilAdapter(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	url, err := bot.ResolveAttachmentURL(context.Background(), Attachment{URL: "x"})

	asserts.ErrorIs(t, err, ErrUnknownBotType, "ResolveAttachmentURL with no adapter")
	asserts.Equal(t, url, "", "no URL when the adapter is missing")
}

func TestBot_ResolveAttachmentURL_Passthrough(t *testing.T) {
	bot := New(SlackBotType, &stubAdapter{})

	url, err := bot.ResolveAttachmentURL(context.Background(), Attachment{URL: "https://cdn/x"})
	asserts.NoError(t, err, "passthrough is not an error")
	asserts.Equal(t, url, "https://cdn/x", "an adapter without a resolver returns att.URL verbatim")

	empty, err := bot.ResolveAttachmentURL(context.Background(), Attachment{})
	asserts.NoError(t, err, "an empty att.URL is not an error")
	asserts.Equal(t, empty, "", "an empty att.URL passes through as empty")
}

func TestBot_ResolveAttachmentURL_DelegatesToResolver(t *testing.T) {
	r := &resolverStub{url: "resolved://link"}
	bot := New(TelegramBotType, r)

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")

	url, err := bot.ResolveAttachmentURL(ctx, Attachment{URL: "ignored"})

	asserts.NoError(t, err, "resolver success")
	asserts.Equal(t, url, "resolved://link", "the resolver result is returned, not att.URL")
	asserts.True(t, r.called, "the resolver was invoked")
	asserts.True(t, r.gotCtx.Value(ctxKey{}) == "sentinel", "ctx is threaded through to the resolver")
}

func TestBot_ResolveAttachmentURL_ResolverEmptyNotOverridden(t *testing.T) {
	bot := New(TelegramBotType, &resolverStub{url: ""})

	url, err := bot.ResolveAttachmentURL(context.Background(), Attachment{URL: "should-not-leak"})

	asserts.NoError(t, err, "a resolver returning empty is not an error")
	asserts.Equal(t, url, "", "an adapter-returned empty is NOT overridden by att.URL")
}

// threadedStub is a stubAdapter that also implements ThreadedSender, recording
// the arguments it was handed.
type threadedStub struct {
	stubAdapter
	gotChannel, gotReplyTo, gotText string
	called                          bool
}

func (t *threadedStub) SendThreaded(_ context.Context, channelID, replyToID, text string) error {
	t.called = true
	t.gotChannel, t.gotReplyTo, t.gotText = channelID, replyToID, text
	return nil
}

// sendRecorder is a stubAdapter that records plain Send calls (stubAdapter.Send
// discards them) so the ReplyToMessage fallback can be asserted.
type sendRecorder struct {
	stubAdapter
	gotChannel, gotText string
	called              bool
}

func (s *sendRecorder) Send(_ context.Context, channelID, text string, _ SendOptions) error {
	s.called = true
	s.gotChannel, s.gotText = channelID, text
	return nil
}

// reactionCaptureAdapter captures the AdapterDeps it was handed at Connect so a
// test can drive DispatchReaction directly.
type reactionCaptureAdapter struct {
	stubAdapter
	deps AdapterDeps
}

func (a *reactionCaptureAdapter) Connect(_ context.Context, deps AdapterDeps) error {
	a.deps = deps
	return nil
}

func TestBot_OnReaction_RunsAllHandlers(t *testing.T) {
	bot := &Bot{}
	var got1, got2 *Reaction
	bot.OnReaction(func(_ context.Context, _ *Bot, r *Reaction) { got1 = r })
	bot.OnReaction(func(_ context.Context, _ *Bot, r *Reaction) { got2 = r })

	r := &Reaction{Emoji: "thumbsup", UserID: "U1", ChannelID: "C1", MessageID: "M1"}
	bot.dispatchReaction(context.Background(), r)

	asserts.True(t, got1 == r, "first reaction handler runs")
	asserts.True(t, got2 == r, "second reaction handler runs")
}

func TestBot_dispatchReaction_PerHandlerRecover(t *testing.T) {
	bot := &Bot{}
	secondCalled := false
	bot.OnReaction(func(_ context.Context, _ *Bot, _ *Reaction) { panic("reaction boom") })
	bot.OnReaction(func(_ context.Context, _ *Bot, _ *Reaction) { secondCalled = true })

	// A panic in the first handler must neither skip the second nor propagate.
	bot.dispatchReaction(context.Background(), &Reaction{Emoji: "x"})

	asserts.True(t, secondCalled, "a panicking handler must not skip later handlers")
}

func TestBot_Connect_WiresDispatchReaction(t *testing.T) {
	a := &reactionCaptureAdapter{}
	bot := New(SlackBotType, a)
	fired := false
	bot.OnReaction(func(_ context.Context, _ *Bot, _ *Reaction) { fired = true })

	asserts.NoError(t, bot.Connect(context.Background()), "Connect")
	t.Cleanup(func() { _ = bot.Disconnect() })

	asserts.True(t, a.deps.DispatchReaction != nil, "Connect wires DispatchReaction into AdapterDeps")
	a.deps.DispatchReaction(context.Background(), &Reaction{Emoji: "x"})
	asserts.True(t, fired, "dispatching through the wired callback runs OnReaction handlers")
}

func TestBot_ReplyToMessage_NilAdapter(t *testing.T) {
	bot := &Bot{BotType: BotType(999)}

	err := bot.ReplyToMessage(context.Background(), "C1", "M1", "hi")

	asserts.ErrorIs(t, err, ErrUnknownBotType, "ReplyToMessage with no adapter")
}

func TestBot_ReplyToMessage_DelegatesToThreadedSender(t *testing.T) {
	ts := &threadedStub{}
	bot := New(SlackBotType, ts)

	err := bot.ReplyToMessage(context.Background(), "C1", "M1", "hi")

	asserts.NoError(t, err, "threaded send")
	asserts.True(t, ts.called, "SendThreaded invoked")
	asserts.Equal(t, ts.gotChannel, "C1", "channel threaded through")
	asserts.Equal(t, ts.gotReplyTo, "M1", "replyToID threaded through")
	asserts.Equal(t, ts.gotText, "hi", "text threaded through")
}

func TestBot_ReplyToMessage_FallsBackToSend(t *testing.T) {
	s := &sendRecorder{}
	bot := New(CLIBotType, s)

	err := bot.ReplyToMessage(context.Background(), "C1", "M1", "hi")

	asserts.NoError(t, err, "fallback send")
	asserts.True(t, s.called, "an adapter without ThreadedSender falls back to Send")
	asserts.Equal(t, s.gotChannel, "C1", "fallback posts to the channel")
	asserts.Equal(t, s.gotText, "hi", "fallback sends the text")
}
