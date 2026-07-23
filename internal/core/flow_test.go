package core

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lao/botbooter/internal/asserts"
)

func noopComplete(context.Context, *Bot, *Message, Answers) {}

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

	s, _ := bot.conversations.store.Get(key)
	asserts.Equal(t, s.Step, 0, "stayed on step 0 after an invalid answer")
	_, stored := s.Answers["email"]
	asserts.False(t, stored, "invalid answer is not stored")

	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "a@b.com")) // valid
	s2, _ := bot.conversations.store.Get(key)
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

	s, _ := bot.conversations.store.Get(key)
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
	_, ok := bot.conversations.store.Get(key)
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
	s1, _ := bot.conversations.store.Get(key)

	time.Sleep(2 * time.Millisecond) // ensure the clock advances between steps
	bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha"))
	s2, _ := bot.conversations.store.Get(key)

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
	s1, _ := bot.conversations.store.Get(key)

	time.Sleep(2 * time.Millisecond) // ensure the clock advances
	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "bad"))
	asserts.True(t, handled, "a rejected answer is consumed")
	s2, ok := bot.conversations.store.Get(key)
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

	bot.conversations.store.Set(key, ConversationState{
		FlowID:    "f",
		ExpiresAt: time.Now().Add(-time.Minute),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(ctx, bot, msgFrom("u", "c", "late"))
	asserts.True(t, handled, "an expired-state message is consumed")
	asserts.True(t, timedOut, "OnTimeout ran")
	_, ok := bot.conversations.store.Get(key)
	asserts.False(t, ok, "expired state is reaped")
}

func TestFlow_UnregisteredFlowIDFallsThrough(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	key := conversationKey(msgFrom("u", "c", ""))

	bot.conversations.store.Set(key, ConversationState{
		FlowID:    "ghost",
		ExpiresAt: time.Now().Add(time.Hour),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "hello"))
	asserts.False(t, handled, "state for an unregistered flow falls through to the command table")
	_, ok := bot.conversations.store.Get(key)
	asserts.False(t, ok, "stale state is reaped, not left to panic later")
}

func TestFlow_SecretExcludedFromSerializedState(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	f := NewFlow("signup").Ask("name", "name?").Ask("password", "pw?", Secret()).OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^signup$", f), "register")

	state := ConversationState{
		FlowID:  "signup",
		Step:    2,
		Answers: map[string]string{"name": "Alice", "password": "hunter2"},
	}

	ser := serializableState(state, f)

	_, hasPw := ser.Answers["password"]
	asserts.False(t, hasPw, "secret key is excluded from the serialized state")
	asserts.Equal(t, ser.Answers["name"], "Alice", "non-secret answers are retained")

	payload, err := json.Marshal(ser)
	asserts.NoError(t, err, "marshal serialized state")
	asserts.False(t, strings.Contains(string(payload), "hunter2"), "secret value never appears in the serialized payload")

	asserts.Equal(t, state.Answers["password"], "hunter2", "serialization does not mutate the live state")
}

func TestFlow_PanicInValidatorDoesNotWedgeShard(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("a", "a?", Validate(func(string) error { panic("validator boom") })).OnComplete(noopComplete)
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")
	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)

	// dispatch's recover would catch this in production; here advance is called
	// directly, so recover locally.
	func() {
		defer func() { _ = recover() }()
		bot.conversations.advance(ctx, bot, msgFrom("u", "c", "x"))
	}()

	// The shard must have been released by defer despite the panic.
	done := make(chan struct{})
	go func() {
		bot.conversations.withLock(key, func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a panic in the validator wedged the shard")
	}
}

func TestFlow_PanicInOnCompleteDoesNotWedgeShard(t *testing.T) {
	bot := New(SlackBotType, &recordingAdapter{})
	ctx := context.Background()
	key := conversationKey(msgFrom("u", "c", ""))

	f := NewFlow("f").Ask("a", "a?").OnComplete(func(context.Context, *Bot, *Message, Answers) { panic("complete boom") })
	asserts.NoError(t, bot.HandleFlow("^f$", f), "register")
	bot.conversations.start(ctx, bot, msgFrom("u", "c", "f"), f)

	func() {
		defer func() { _ = recover() }()
		bot.conversations.advance(ctx, bot, msgFrom("u", "c", "alpha")) // last step -> OnComplete panics
	}()

	// OnComplete runs after the shard is released and state is deleted, so the
	// shard must be free.
	done := make(chan struct{})
	go func() {
		bot.conversations.withLock(key, func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a panic in OnComplete wedged the shard")
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
	bot.conversations.store.Set(key, ConversationState{
		FlowID:    "f",
		Step:      99,
		ExpiresAt: time.Now().Add(time.Hour),
		Answers:   map[string]string{},
	})

	handled := bot.conversations.advance(context.Background(), bot, msgFrom("u", "c", "hi"))
	asserts.False(t, handled, "out-of-range step falls through to the command table")
	_, ok := bot.conversations.store.Get(key)
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

	if s, ok := bot.conversations.store.Get(key); ok {
		asserts.True(t, s.Step >= 0 && s.Step < len(f.steps), "step stays within bounds")
	}
}
