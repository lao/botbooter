package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// defaultCancelWord is the bail-out word a Flow recognizes unless CancelWord
// overrides or disables it.
const defaultCancelWord = "cancel"

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
//
// It does NOT encrypt anything, does not police user-installed middleware (which
// sees the raw Message.Content), and does not hide the answer from other members
// of a public channel. Secret-collecting flows should therefore be DM-only.
func Secret() AskOption {
	return func(s *flowStep) { s.secret = true }
}

// Ask appends a question to the flow: prompt is sent to the user and the reply is
// stored under key. Options configure validation (Validate) and secrecy (Secret).
// Keys must be unique within a flow; HandleFlow rejects duplicates.
func (f *Flow) Ask(key, prompt string, opts ...AskOption) *Flow {
	step := flowStep{key: key, prompt: prompt}
	for _, opt := range opts {
		opt(&step)
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

// OnTimeout sets the optional callback run when an idle flow expires.
func (f *Flow) OnTimeout(fn func(ctx context.Context, b *Bot, m *Message)) *Flow {
	f.onTimeout = fn
	return f
}

// CancelWord sets the bail-out word (default "cancel"); passing "" disables
// cancellation. The cancel check precedes validation, so the configured word
// shadows that exact answer on every step.
func (f *Flow) CancelWord(word string) *Flow {
	f.cancelWord = word
	return f
}

// Timeout sets the per-step idle TTL (default 10m); it slides on each successful
// step.
func (f *Flow) Timeout(d time.Duration) *Flow {
	f.timeout = d
	return f
}

// validate enforces the HandleFlow contract on f, independent of the pattern.
func (f *Flow) validate() error {
	if f.id == "" {
		return errors.New("botbooter: flow id must not be empty")
	}
	if len(f.steps) == 0 {
		return fmt.Errorf("botbooter: flow %q has no steps", f.id)
	}
	seen := make(map[string]bool, len(f.steps))
	for _, s := range f.steps {
		if s.key == "" {
			return fmt.Errorf("botbooter: flow %q has a step with an empty key", f.id)
		}
		if seen[s.key] {
			return fmt.Errorf("botbooter: flow %q has duplicate Ask key %q", f.id, s.key)
		}
		seen[s.key] = true
	}
	if f.onComplete == nil {
		return fmt.Errorf("botbooter: flow %q has no OnComplete", f.id)
	}
	return nil
}

// secretKeys returns the set of answer keys marked Secret(), or nil if none.
func (f *Flow) secretKeys() map[string]bool {
	var secrets map[string]bool
	for _, s := range f.steps {
		if s.secret {
			if secrets == nil {
				secrets = make(map[string]bool)
			}
			secrets[s.key] = true
		}
	}
	return secrets
}

// serializableState returns a copy of state holding only the answers safe to
// persist: answers from Secret() steps are omitted, so a durable Store never
// receives them. It does not mutate state. flow must be the registered flow for
// state.FlowID; a nil flow returns state unchanged.
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
func (b *Bot) HandleFlow(pattern string, flow *Flow) error {
	if err := flow.validate(); err != nil {
		return err
	}
	if b.flows == nil {
		b.flows = make(map[string]*Flow)
	}
	if _, exists := b.flows[flow.id]; exists {
		return fmt.Errorf("botbooter: flow id %q already registered", flow.id)
	}
	if b.conversations == nil {
		b.conversations = newConversationManager()
	}
	// AddHandler compiles the pattern and returns an error before registering it,
	// so an invalid pattern leaves the flow registry untouched.
	if err := b.AddHandler(Command{Pattern: pattern, Handler: func(ctx context.Context, bot *Bot, m *Message) {
		bot.conversations.start(ctx, bot, m, flow)
	}}); err != nil {
		return err
	}
	b.flows[flow.id] = flow
	return nil
}
