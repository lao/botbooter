package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/cli"
	"github.com/teilomillet/gollm"
	"github.com/teilomillet/gollm/llm"
)

// fakeLLM is a generator that records the prompt it saw and returns a canned
// answer or error — no network, no API key.
type fakeLLM struct {
	answer    string
	err       error
	gotPrompt string
}

func (f *fakeLLM) Generate(_ context.Context, p *gollm.Prompt, _ ...llm.GenerateOption) (string, error) {
	f.gotPrompt = p.String()
	return f.answer, f.err
}

// newTestBot returns a CLI bot whose replies land in the returned builder. The
// CLI adapter's Send writes to its writer regardless of connection state, so the
// handler can be driven directly without a live Run loop.
func newTestBot() (*botbooter.Bot, *strings.Builder) {
	out := &strings.Builder{}
	return cli.New(strings.NewReader(""), out), out
}

func TestAskHandler_RepliesWithAnswer(t *testing.T) {
	bot, out := newTestBot()
	fake := &fakeLLM{answer: "Because of Rayleigh scattering."}

	msg := &botbooter.Message{ChannelID: "cli", Content: "ask why is the sky blue?"}
	askHandler(fake)(context.Background(), bot, msg)

	if got := out.String(); !strings.Contains(got, "Rayleigh scattering") {
		t.Fatalf("reply = %q, want it to contain the LLM answer", got)
	}
	if !strings.Contains(fake.gotPrompt, "why is the sky blue?") {
		t.Fatalf("prompt = %q, want the trimmed question", fake.gotPrompt)
	}
}

func TestAskHandler_BlankQuestionShowsHint(t *testing.T) {
	bot, out := newTestBot()
	fake := &fakeLLM{answer: "should not be used"}

	msg := &botbooter.Message{ChannelID: "cli", Content: "ask    "}
	askHandler(fake)(context.Background(), bot, msg)

	if fake.gotPrompt != "" {
		t.Fatalf("LLM called for a blank question; prompt = %q", fake.gotPrompt)
	}
	if got := out.String(); !strings.Contains(got, "Ask me something") {
		t.Fatalf("reply = %q, want the usage hint", got)
	}
}

func TestAskHandler_ReportsLLMError(t *testing.T) {
	bot, out := newTestBot()
	fake := &fakeLLM{err: errors.New("rate limited")}

	msg := &botbooter.Message{ChannelID: "cli", Content: "ask anything"}
	askHandler(fake)(context.Background(), bot, msg)

	// The reply must signal failure without leaking the raw upstream error to chat.
	got := out.String()
	if !strings.Contains(got, "failed") {
		t.Fatalf("reply = %q, want it to signal the failure", got)
	}
	if strings.Contains(got, "rate limited") {
		t.Fatalf("reply = %q, leaked the raw LLM error into chat", got)
	}
}
