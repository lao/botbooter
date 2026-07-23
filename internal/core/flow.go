package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// defaultCancelWord is the bail-out word a Flow recognizes unless CancelWord
// overrides or disables it.
const defaultCancelWord = "cancel"

// Errors returned by [Bot.HandleFlow], wrapped with the flow id (and key) for
// context. Check them with errors.Is.
var (
	// ErrFlowEmptyID is returned when a flow's id is empty.
	ErrFlowEmptyID = errors.New("botbooter: flow id must not be empty")
	// ErrFlowNoSteps is returned when a flow has no Ask steps.
	ErrFlowNoSteps = errors.New("botbooter: flow has no steps")
	// ErrFlowEmptyStepKey is returned when a flow has an Ask step with an empty key.
	ErrFlowEmptyStepKey = errors.New("botbooter: flow step has an empty key")
	// ErrFlowDuplicateKey is returned when a flow has two Ask steps with the same key.
	ErrFlowDuplicateKey = errors.New("botbooter: flow has a duplicate Ask key")
	// ErrFlowNoOnComplete is returned when a flow has no OnComplete callback.
	ErrFlowNoOnComplete = errors.New("botbooter: flow has no OnComplete")
	// ErrFlowAlreadyRegistered is returned when a flow id is registered twice.
	ErrFlowAlreadyRegistered = errors.New("botbooter: flow id already registered")
)

// NewFlow starts building a Flow with the given stable id. The id is load-bearing
// for persistence and must stay stable across deploys: a future Store keys
// in-flight state by it, and a renamed id orphans that state. The flow recognizes
// the default cancel word until CancelWord changes it.
func NewFlow(id string) *Flow {
	return &Flow{id: id, cancelWord: defaultCancelWord}
}

// AskOption configures a single Ask step.
type AskOption func(*flowStep)

// Validate attaches a validator to a step. When it returns a non-nil error the
// engine re-prompts the same step — using err.Error() as the nudge when non-empty
// — and neither stores the answer nor advances. The validator runs under the
// conversation's lock, so keep it fast and non-blocking.
func Validate(fn func(string) error) AskOption {
	return func(s *flowStep) { s.validate = fn }
}

// Secret marks a step's answer as sensitive. Its scope is precise:
//
//   - It keeps the answer out of the framework's own logging and excludes the key
//     from any state serialized to a Store, so secrets never reach durable storage.
//   - Its answer is stored with its exact bytes: ordinary answers are trimmed of
//     surrounding whitespace, but a secret (password, token) keeps leading and
//     trailing whitespace, which can be meaningful.
//
// The engine still decides whether a step is answered from the TRIMMED content,
// so an all-whitespace reply is treated as no answer and re-prompts even on a
// Secret() step; the exact-bytes rule only governs how a real answer is stored.
//
// It does NOT encrypt anything, does not police user-installed middleware (which
// sees the raw Message.Content), and does not hide the answer from other members
// of a public channel. Secret-collecting flows should therefore be DM-only.
func Secret() AskOption {
	return func(s *flowStep) { s.secret = true }
}

// Ask appends a question to the flow: prompt is sent to the user and the reply is
// stored under key. Options configure validation (Validate) and secrecy (Secret).
// Keys must be unique within a flow; HandleFlow rejects duplicates. A nil option
// is skipped, so a conditionally-built option slice may carry a nil entry.
func (f *Flow) Ask(key, prompt string, opts ...AskOption) *Flow {
	step := flowStep{key: key, prompt: prompt}
	for _, opt := range opts {
		if opt != nil {
			opt(&step)
		}
	}
	f.steps = append(f.steps, step)
	return f
}

// OnComplete sets the callback run after the last step, with the collected
// answers. It is required: HandleFlow rejects a flow without one.
func (f *Flow) OnComplete(fn func(ctx context.Context, b *Bot, m *Message, a Answers)) *Flow {
	f.onComplete = fn
	return f
}

// OnCancel sets the optional callback run when the user cancels the flow.
func (f *Flow) OnCancel(fn func(ctx context.Context, b *Bot, m *Message)) *Flow {
	f.onCancel = fn
	return f
}

// OnTimeout sets the optional callback run when an idle flow's next message
// lands after expiry but before the background sweeper reaps the state.
//
// It does NOT fire on a timer. Expiry is lazy: the background sweeper deletes
// expired state WITHOUT running OnTimeout, and expiry is otherwise noticed only
// when the user's next message arrives. So OnTimeout runs only if that next
// message lands in the window between expiry and the next sweep (at most one
// sweep interval wide); if the user stays quiet past a sweep the state is reaped
// silently, and a later message falls through to the command table as if no flow
// were ever active.
func (f *Flow) OnTimeout(fn func(ctx context.Context, b *Bot, m *Message)) *Flow {
	f.onTimeout = fn
	return f
}

// CancelWord sets the bail-out word (default "cancel"); passing "" disables
// cancellation. The cancel check precedes validation, so the configured word
// shadows that exact answer on every step. The match is exact and
// case-sensitive against the trimmed message: "cancel" (with any surrounding
// whitespace) bails, but "Cancel" or "CANCEL" do not.
func (f *Flow) CancelWord(word string) *Flow {
	f.cancelWord = word
	return f
}

// Timeout sets the per-step idle TTL (default 10m). It measures user silence: the
// deadline slides on ANY message for the active step — including an empty or
// rejected answer — since any message counts as engagement. Only going quiet past
// the TTL lets the flow expire.
func (f *Flow) Timeout(d time.Duration) *Flow {
	f.timeout = d
	return f
}

// validate enforces the HandleFlow contract on f, independent of the pattern.
func (f *Flow) validate() error {
	if f.id == "" {
		return ErrFlowEmptyID
	}
	if len(f.steps) == 0 {
		return fmt.Errorf("botbooter: flow %q: %w", f.id, ErrFlowNoSteps)
	}
	seen := make(map[string]bool, len(f.steps))
	for _, s := range f.steps {
		if s.key == "" {
			return fmt.Errorf("botbooter: flow %q: %w", f.id, ErrFlowEmptyStepKey)
		}
		if seen[s.key] {
			return fmt.Errorf("botbooter: flow %q: %q: %w", f.id, s.key, ErrFlowDuplicateKey)
		}
		seen[s.key] = true
	}
	if f.onComplete == nil {
		return fmt.Errorf("botbooter: flow %q: %w", f.id, ErrFlowNoOnComplete)
	}
	return nil
}

// secretKeys returns the set of answer keys marked Secret(), or nil if none.
func (f *Flow) secretKeys() map[string]bool {
	secrets := make(map[string]bool)
	for _, s := range f.steps {
		if s.secret {
			secrets[s.key] = true
		}
	}
	if len(secrets) == 0 {
		return nil
	}
	return secrets
}

// serializableState returns a copy of state holding only the answers safe to
// persist: answers from Secret() steps are omitted, so a durable Store never
// receives them. It does not mutate state. flow must be the registered flow for
// state.FlowID; a nil flow returns state unchanged.
//
// v1 has no durable Store: the only ConversationStore is the volatile in-memory
// one, which legitimately holds secrets (the Secret() contract keeps them in
// volatile memory only). This is therefore the seam the future Store path applies
// at its persist boundary — it is intentionally not yet wired into advance, which
// would strip secrets from the in-memory store and starve OnComplete.
func serializableState(state ConversationState, flow *Flow) ConversationState {
	if flow == nil {
		return state
	}
	secrets := flow.secretKeys()
	if len(secrets) == 0 {
		return state
	}
	answers := make(map[string]string, len(state.Answers))
	for k, v := range state.Answers {
		if secrets[k] {
			continue
		}
		answers[k] = v
	}
	state.Answers = answers
	return state
}

// HandleFlow registers flow under pattern: a message matching pattern with no
// active flow for the conversation starts it (sending the first prompt). It
// validates both the pattern and the flow, returning an error when the pattern is
// an invalid regexp, flow.id is empty, the flow has no steps, its Ask keys are not
// unique, it has no OnComplete, or another flow with the same id is already
// registered. On any error nothing is registered.
//
// flow must not be mutated after it is registered: HandleFlow stores the pointer
// and dispatch goroutines read its steps and callbacks concurrently, so a later
// builder call (Ask, OnComplete, Timeout, …) on the same *Flow while the Bot is
// connected is a data race. Finish building before calling HandleFlow.
func (b *Bot) HandleFlow(pattern string, flow *Flow) error {
	if flow == nil {
		return errors.New("botbooter: HandleFlow: flow must not be nil")
	}
	if err := flow.validate(); err != nil {
		return err
	}
	if b.flows == nil {
		b.flows = make(map[string]*Flow)
	}
	if _, exists := b.flows[flow.id]; exists {
		return fmt.Errorf("botbooter: flow id %q: %w", flow.id, ErrFlowAlreadyRegistered)
	}
	if b.conversations == nil {
		b.conversations = newConversationManager()
	}
	// AddHandler records an invalid pattern into setupErrs rather than returning it,
	// so compile the pattern here to surface the error synchronously and leave the
	// flow registry untouched on failure.
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("botbooter: flow id %q: invalid pattern %q: %w", flow.id, pattern, err)
	}
	b.AddHandler(Command{Pattern: pattern, Handler: func(ctx context.Context, bot *Bot, m *Message) {
		bot.conversations.start(ctx, bot, m, flow)
	}})
	b.flows[flow.id] = flow
	return nil
}
