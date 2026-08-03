package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

func noopComplete(context.Context, *Bot, *Message, Answers) {}

// getConvState / setConvState read and write conversation state through the
// manager mutex, honoring the get/set "caller must hold m.mu" contract from test
// code (get/set are unexported map ops that assume the lock is held).
func getConvState(m *conversationManager, key string) (ConversationState, bool) {
	var (
		st ConversationState
		ok bool
	)
	m.withLock(func() { st, ok = m.get(key) })
	return st, ok
}

func setConvState(m *conversationManager, key string, st ConversationState) {
	m.withLock(func() { m.set(key, st) })
}

func TestNewFlow_Defaults(t *testing.T) {
	f := NewFlow("f")
	asserts.Equal(t, f.cancelWord, "cancel", "default cancel word")
	asserts.Equal(t, f.timeoutOrDefault(), 10*time.Minute, "default per-step timeout")
}

func TestAnswers_GetLookup(t *testing.T) {
	a := Answers{"name": "Alice", "empty": ""}

	asserts.Equal(t, a.Get("name"), "Alice", "present key")
	asserts.Equal(t, a.Get("missing"), "", "missing key returns empty")

	v, ok := a.Lookup("empty")
	asserts.Equal(t, v, "", "empty answer value")
	asserts.True(t, ok, "an empty-but-present answer is found")

	_, ok = a.Lookup("missing")
	asserts.False(t, ok, "a missing answer is not found")
}

func TestBot_HandleFlow_Contract(t *testing.T) {
	valid := func() *Flow { return NewFlow("f").Ask("a", "a?").OnComplete(noopComplete) }

	t.Run("Success", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.NoError(t, bot.HandleFlow("^f$", valid()), "valid flow registers")
		_, ok := bot.flowByID("f")
		asserts.True(t, ok, "flow is in the registry")
	})

	t.Run("InvalidPattern", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		err := bot.HandleFlow("[bad(", valid())
		asserts.Error(t, err, "invalid regexp is rejected")
		_, ok := bot.flowByID("f")
		asserts.False(t, ok, "nothing registered on a bad pattern")
	})

	t.Run("EmptyID", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.Error(t, bot.HandleFlow("^f$", NewFlow("").Ask("a", "a?").OnComplete(noopComplete)), "empty id is rejected")
	})

	t.Run("NoSteps", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.Error(t, bot.HandleFlow("^f$", NewFlow("f").OnComplete(noopComplete)), "zero steps is rejected")
	})

	t.Run("DuplicateKeys", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		f := NewFlow("f").Ask("a", "a?").Ask("a", "again?").OnComplete(noopComplete)
		asserts.Error(t, bot.HandleFlow("^f$", f), "duplicate Ask keys are rejected")
	})

	t.Run("NoOnComplete", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.Error(t, bot.HandleFlow("^f$", NewFlow("f").Ask("a", "a?")), "missing OnComplete is rejected")
	})

	t.Run("DuplicateFlowID", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.NoError(t, bot.HandleFlow("^f$", valid()), "first registration")
		asserts.Error(t, bot.HandleFlow("^g$", valid()), "second flow with the same id is rejected")
	})
}

func TestFlow_ValidatorRePromptsSameStep(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").
		Ask("email", "email?", Validate(func(s string) error {
			if !strings.Contains(s, "@") {
				return errors.New("need an @")
			}
			return nil
		})).
		Ask("name", "name?").
		OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "bad")) // invalid

	s, _ := getConvState(bot.conversations, key)
	asserts.Equal(t, s.Step, 0, "stayed on step 0 after an invalid answer")
	_, stored := s.Answers["email"]
	asserts.False(t, stored, "invalid answer is not stored")

	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "a@b.com")) // valid
	s2, _ := getConvState(bot.conversations, key)
	asserts.Equal(t, s2.Step, 1, "advanced after a valid answer")

	got := adapter.messages()
	asserts.Equal(t, got[0], "email?", "first prompt")
	asserts.Equal(t, got[1], "need an @", "nudge uses err.Error()")
	asserts.Equal(t, got[2], "name?", "next prompt after valid")
}

func TestFlow_EmptyAnswerRePrompts(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("name", "name?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "   ")) // whitespace = non-answer

	s, _ := getConvState(bot.conversations, key)
	asserts.Equal(t, s.Step, 0, "stayed on step 0 for an empty answer")
	asserts.Equal(t, len(s.Answers), 0, "empty answer is not stored")

	got := adapter.messages()
	asserts.Equal(t, got[len(got)-1], "name?", "re-prompted the same step")
}

func TestFlow_CancelBailsAndRunsOnCancel(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	canceled := false
	f := NewFlow("f").Ask("a", "a?").Ask("b", "b?").
		OnComplete(noopComplete).
		OnCancel(func(context.Context, *Bot, *Message) { canceled = true })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha")) // step 0 -> 1
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "cancel"))

	asserts.True(t, canceled, "OnCancel ran")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "state cleared on cancel")
}

func TestFlow_CancelWordDisabledTreatsWordAsAnswer(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()

	var got Answers
	f := NewFlow("f").CancelWord("").Ask("a", "a?").
		OnComplete(func(_ context.Context, _ *Bot, _ *Message, a Answers) { got = a })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "cancel"))

	asserts.Equal(t, got.Get("a"), "cancel", "with cancel disabled, 'cancel' is a normal answer")
}

func TestFlow_TTLSlidesOnEachStep(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Timeout(time.Hour).Ask("a", "a?").Ask("b", "b?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	s1, _ := getConvState(bot.conversations, key)

	time.Sleep(2 * time.Millisecond) // ensure the clock advances between steps
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha"))
	s2, _ := getConvState(bot.conversations, key)

	asserts.True(t, s2.ExpiresAt.After(s1.ExpiresAt), "TTL slides forward on each successful step")
}

func TestFlow_TTLSlidesOnRejectedAnswer(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	// A validator that rejects everything keeps the flow on its first step.
	reject := Validate(func(string) error { return errors.New("nope") })
	f := NewFlow("f").Timeout(time.Hour).Ask("a", "a?", reject).OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	s1, _ := getConvState(bot.conversations, key)

	time.Sleep(2 * time.Millisecond) // ensure the clock advances
	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "bad"))
	asserts.True(t, handled, "a rejected answer is consumed")
	s2, ok := getConvState(bot.conversations, key)
	asserts.True(t, ok, "state survives a rejected answer")
	asserts.Equal(t, s2.Step, 0, "step does not advance on a rejected answer")
	asserts.True(t, s2.ExpiresAt.After(s1.ExpiresAt), "TTL slides forward even when the answer is rejected")
}

func TestFlow_ExpiredStateTimesOut(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	timedOut := false
	f := NewFlow("f").Ask("a", "a?").OnComplete(noopComplete).
		OnTimeout(func(context.Context, *Bot, *Message) { timedOut = true })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	setConvState(bot.conversations, key, ConversationState{
		FlowID:    "f",
		ExpiresAt: time.Now().Add(-time.Minute),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "late"))
	asserts.True(t, handled, "an expired-state message is consumed")
	asserts.True(t, timedOut, "OnTimeout ran")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "expired state is reaped")
}

func TestFlow_UnregisteredFlowIDFallsThrough(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	key := conversationKey(msgFrom("u", "c", ""))

	setConvState(bot.conversations, key, ConversationState{
		FlowID:    "ghost",
		ExpiresAt: time.Now().Add(time.Hour),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "hello"))
	asserts.False(t, handled, "state for an unregistered flow falls through to the command table")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "stale state is reaped, not left to panic later")
}

func TestFlow_PanicInValidatorDoesNotWedgeLock(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()

	f := NewFlow("f").Ask("a", "a?", Validate(func(string) error { panic("validator boom") })).OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")
	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)

	// dispatch's recover would catch this in production; here advance is called
	// directly, so recover locally.
	func() {
		defer func() { _ = recover() }()
		bot.conversations.advance(ctx, bot, msgFrom("u", "c", "x"))
	}()

	// The manager mutex must have been released by defer despite the panic.
	done := make(chan struct{})
	go func() {
		bot.conversations.withLock(func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a panic in the validator wedged the manager lock")
	}
}

func TestFlow_PanicInOnCompleteDoesNotWedgeLock(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()

	f := NewFlow("f").Ask("a", "a?").OnComplete(func(context.Context, *Bot, *Message, Answers) { panic("complete boom") })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")
	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)

	func() {
		defer func() { _ = recover() }()
		bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha")) // last step -> OnComplete panics
	}()

	// OnComplete runs after the lock is released and state is deleted, so the
	// mutex must be free.
	done := make(chan struct{})
	go func() {
		bot.conversations.withLock(func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a panic in OnComplete wedged the manager lock")
	}
}

// TestFlow_ConcurrentDoubleStart proves the set-if-absent start: many racing
// triggers for one conversation yield exactly one first prompt. Run under -race
// for the data-race assertion.
func TestFlow_ConcurrentDoubleStart(t *testing.T) {
	adapter := &recordingAdapter{}
	bot := New(SlackBotType, adapter)
	ctx := context.Background()

	f := NewFlow("f").Ask("a", "a?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	m := msgFrom("u", "c", "f")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.conversations.start(ctx, bot, m, f)
		}()
	}
	wg.Wait()

	asserts.Equal(t, len(adapter.messages()), 1, "concurrent starts yield exactly one first prompt")
}

func TestBot_HandleFlow_SentinelErrors(t *testing.T) {
	newFlow := func() *Flow { return NewFlow("f").Ask("a", "a?").OnComplete(noopComplete) }

	t.Run("EmptyID", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		err := bot.HandleFlow("^f$", NewFlow("").Ask("a", "a?").OnComplete(noopComplete))
		asserts.ErrorIs(t, err, ErrFlowEmptyID, "empty id sentinel")
	})
	t.Run("NoSteps", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.ErrorIs(t, bot.HandleFlow("^f$", NewFlow("f").OnComplete(noopComplete)), ErrFlowNoSteps, "no-steps sentinel")
	})
	t.Run("EmptyStepKey", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.ErrorIs(t, bot.HandleFlow("^f$", NewFlow("f").Ask("", "a?").OnComplete(noopComplete)), ErrFlowEmptyStepKey, "empty-step-key sentinel")
	})
	t.Run("EmptyStepPrompt", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.ErrorIs(t, bot.HandleFlow("^f$", NewFlow("f").Ask("a", "  ").OnComplete(noopComplete)), ErrFlowEmptyStepPrompt, "empty-step-prompt sentinel")
	})
	t.Run("DuplicateKey", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		f := NewFlow("f").Ask("a", "a?").Ask("a", "b?").OnComplete(noopComplete)
		asserts.ErrorIs(t, bot.HandleFlow("^f$", f), ErrFlowDuplicateKey, "duplicate-key sentinel")
	})
	t.Run("NoOnComplete", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.ErrorIs(t, bot.HandleFlow("^f$", NewFlow("f").Ask("a", "a?")), ErrFlowNoOnComplete, "no-OnComplete sentinel")
	})
	t.Run("AlreadyRegistered", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.NoError(t, bot.HandleFlow("^f$", newFlow()), "first registration")
		asserts.ErrorIs(t, bot.HandleFlow("^g$", newFlow()), ErrFlowAlreadyRegistered, "already-registered sentinel")
	})
	t.Run("InvalidPatternStillErrors", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.Error(t, bot.HandleFlow("[bad(", newFlow()), "invalid pattern still errors")
	})
	t.Run("NilFlow", func(t *testing.T) {
		bot := New(SlackBotType, &recordingAdapter{})
		asserts.ErrorIs(t, bot.HandleFlow("^x$", nil), ErrFlowNil, "a nil flow returns the sentinel, not a panic")
	})
}

func TestFlow_OutOfRangeStepFallsThrough(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	f := NewFlow("f").Ask("a", "a?").Ask("b", "b?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")
	key := conversationKey(msgFrom("u", "c", ""))

	// A durable store could load a state whose flow has since lost steps.
	setConvState(bot.conversations, key, ConversationState{
		FlowID:    "f",
		Step:      99,
		ExpiresAt: time.Now().Add(time.Hour),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "hi"))
	asserts.False(t, handled, "out-of-range step falls through to the command table")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "corrupt state is reaped, not left to wedge")

	handled2 := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "again"))
	asserts.False(t, handled2, "subsequent message is clean after the reap")
}

func TestFlow_SecretAnswerReachesOnComplete(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()

	var got Answers
	f := NewFlow("signup").
		Ask("name", "name?").
		Ask("password", "pw?", Secret()).
		OnComplete(func(_ context.Context, _ *Bot, _ *Message, a Answers) { got = a })
	asserts.NoError(t, bot.HandleFlow("^signup$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "signup"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "Alice"))   // name
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "hunter2")) // secret password

	// v1 keeps secrets in volatile memory and delivers them to OnComplete; the
	// secret exclusion applies only to the future durable-Store serialized form.
	asserts.Equal(t, got.Get("name"), "Alice", "name delivered to OnComplete")
	asserts.Equal(t, got.Get("password"), "hunter2", "secret answer reaches OnComplete from volatile memory")
}

// TestFlow_ConcurrentAdvanceNoDeadlock fires many concurrent answers at one
// conversation. The shard lock must serialize them with no deadlock, no panic,
// and no map corruption (run under -race). State ends either completed (reaped) or
// at a valid step.
func TestFlow_ConcurrentAdvanceNoDeadlock(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f")
	for i := 0; i < 5; i++ {
		f.Ask("k"+strconv.Itoa(i), "p?")
	}
	f.OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.conversations.advance(ctx, bot, msgFrom("u", "c", "x"))
		}()
	}
	wg.Wait()

	if s, ok := getConvState(bot.conversations, key); ok {
		asserts.True(t, s.Step >= 0 && s.Step < len(f.steps), "step stays within bounds")
	}
}

// The expired-state branch with NO OnTimeout still consumes the trigger message
// (returns handled) and reaps the state, taking the nil-onTimeout default path.
func TestFlow_ExpiredStateWithoutOnTimeoutConsumesAndReaps(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("a", "a?").OnComplete(noopComplete) // no OnTimeout
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	setConvState(bot.conversations, key, ConversationState{
		FlowID:    "f",
		ExpiresAt: time.Now().Add(-time.Minute),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "late"))
	asserts.True(t, handled, "an expired-state message is consumed even with no OnTimeout set")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "expired state is reaped on the nil-OnTimeout default path")
}

// The cancel-word branch with NO OnCancel still consumes the message and reaps
// the state, taking the nil-onCancel default path.
func TestFlow_CancelWithoutOnCancelConsumesAndReaps(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("a", "a?").Ask("b", "b?").OnComplete(noopComplete) // no OnCancel
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "cancel"))
	asserts.True(t, handled, "the cancel word is consumed even with no OnCancel set")
	_, ok := getConvState(bot.conversations, key)
	asserts.False(t, ok, "state is cleared on the nil-OnCancel default path")
}

// failingSendAdapter's Send always errors, so a flow prompt can never be
// delivered — exercising sendFlowMessage's log-and-continue path.
type failingSendAdapter struct{}

func (failingSendAdapter) Connect(context.Context, AdapterDeps) error { return nil }
func (failingSendAdapter) Disconnect() error                          { return nil }
func (failingSendAdapter) Attachments(*Message) ([]Attachment, error) { return nil, nil }
func (failingSendAdapter) Send(context.Context, string, string, SendOptions) error {
	return errors.New("send boom")
}

// A prompt-send failure must not wedge or roll back the flow: sendFlowMessage
// logs the error and the state machine advances regardless.
func TestFlow_SendFailureStillAdvancesAndLogs(t *testing.T) {
	var buf bytes.Buffer
	bot := New(SlackBotType, failingSendAdapter{})
	bot.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("a", "a?").Ask("b", "b?").OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	s, ok := getConvState(bot.conversations, key)
	asserts.True(t, ok, "flow starts despite a failed first-prompt send")
	asserts.Equal(t, s.Step, 0, "on step 0 after start")

	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha"))
	asserts.True(t, handled, "answer is consumed even though the prompt send fails")
	s2, _ := getConvState(bot.conversations, key)
	asserts.Equal(t, s2.Step, 1, "flow advances to step 1 despite the send error")

	asserts.True(t, strings.Contains(buf.String(), "failed to send flow prompt"),
		"the send failure is logged, not propagated")
}

// A nil AskOption must be skipped rather than panicking, mirroring
// resolveSendOptions' nil-skip; the step still registers and collects answers.
func TestFlow_AskSkipsNilOption(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()

	var got Answers
	f := NewFlow("f").
		Ask("a", "a?", nil, Validate(func(string) error { return nil }), nil).
		OnComplete(func(_ context.Context, _ *Bot, _ *Message, a Answers) { got = a })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "a flow whose Ask carried nil options registers")

	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "answer"))
	asserts.Equal(t, got.Get("a"), "answer", "the step with nil options still collects its answer")
}
