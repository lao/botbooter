// Command llm is a botbooter example that answers questions with an LLM. It wires
// any botbooter adapter (Slack, Discord, Telegram, WhatsApp, WhatsMeow, Teams,
// GitHub or the local CLI) to the gollm library, so "ask <question>" gets a
// model-generated reply.
//
//	go run ./_examples/llm            # CLI mode (still needs an LLM API key)
//	go run ./_examples/llm slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/llm discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/llm telegram   # reads TELEGRAM_BOT_TOKEN
//
// The LLM is selected via LLM_PROVIDER / LLM_MODEL / <PROVIDER>_API_KEY; see llm.go.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/lao/botbooter"
)

func main() {
	_ = godotenv.Load(".env")

	llm, err := newLLM()
	if err != nil {
		log.Fatal(err)
	}

	botType := requestedBotType(os.Args)
	bot, err := newBot(botType)
	if err != nil {
		log.Fatal(err)
	}

	// Invalid command patterns are reported by Run, not here.
	bot.HandleFunc("^ask ", askHandler(llm))
	bot.SetUnknownCommandHandler(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		reply(ctx, b, m, "Say `ask <question>` and I'll ask the LLM.")
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("botbooter llm example running as %q bot", botType)
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
