package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/lao/botbooter"
	"github.com/teilomillet/gollm"
	"github.com/teilomillet/gollm/llm"
)

// generator is the slice of gollm.LLM that the bot actually needs: turn a prompt
// into completed text. Depending on this narrow interface (instead of the whole
// gollm.LLM) is what keeps askHandler unit-testable with a fake — no network,
// no API key. A real gollm.LLM satisfies it as-is. (gollm.GenerateOption is not
// re-exported, so the option type comes from the gollm/llm subpackage.)
type generator interface {
	Generate(ctx context.Context, prompt *gollm.Prompt, opts ...llm.GenerateOption) (string, error)
}

// askHandler answers "ask <question>" by forwarding the question to the LLM and
// replying with the completion, threaded onto the triggering message. It is the
// only botbooter-specific glue in this example; everything else is wiring.
func askHandler(llm generator) botbooter.CommandHandler {
	return func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		// The "^ask " command pattern guarantees the prefix; strip it to the question.
		question := strings.TrimSpace(strings.TrimPrefix(m.Content, "ask"))
		if question == "" {
			reply(ctx, b, m, "Ask me something, e.g. `ask why is the sky blue?`")
			return
		}

		// Bound the call so a hung provider can't wedge this dispatch until shutdown.
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		answer, err := llm.Generate(ctx, gollm.NewPrompt(question))
		if err != nil {
			// Log the detail; keep it off public channels (Slack, GitHub, …).
			log.Printf("llm generate failed: %v", err)
			reply(ctx, b, m, "Sorry, the LLM request failed — check the server logs.")
			return
		}
		reply(ctx, b, m, answer)
	}
}

// reply threads text onto m and logs (rather than crashes) on a send failure, so
// one bad send never tears down the bot.
func reply(ctx context.Context, b *botbooter.Bot, m *botbooter.Message, text string) {
	if err := b.Reply(ctx, m, text); err != nil {
		log.Printf("reply failed: %v", err)
	}
}
