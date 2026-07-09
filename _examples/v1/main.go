// Command v1 is a small demo of botbooter. It runs an "echo" bot on Slack,
// Discord, Telegram, WhatsApp (either flavor), Microsoft Teams or the local CLI.
//
//	go run ./_examples/v1            # CLI mode (no credentials needed)
//	go run ./_examples/v1 slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/v1 discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/v1 telegram   # reads TELEGRAM_BOT_TOKEN
//	go run ./_examples/v1 whatsapp   # Cloud API flavor: reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
//	go run ./_examples/v1 whatsmeow  # WhatsApp Web flavor: no credentials; scan the QR on first run (optional WA_MEOW_DB, default botbooter-whatsapp.db)
//	go run ./_examples/v1 teams      # reads TEAMS_APP_ID / TEAMS_APP_PASSWORD / TEAMS_ADDR (and optional TEAMS_APP_TENANT_ID, TEAMS_PATH, default /api/messages)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
)

type ExampleBot struct {
	*botbooter.Bot
}

func main() {
	_ = godotenv.Load(".env")

	botType := requestedBotType(os.Args)
	bot, err := newBot(botType)
	if err != nil {
		log.Fatal(err)
	}
	b := &ExampleBot{Bot: bot}

	b.AddMiddleware(loggingMiddleware)
	if err := b.HandleFunc("^echo ", b.echo); err != nil {
		log.Fatal(err)
	}
	b.SetUnknownCommandHandler(b.unknownCommand)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("botbooter running as %q bot", botType)
	if err := b.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func (*ExampleBot) unknownCommand(_ context.Context, _ *botbooter.Bot, message *botbooter.Message) {
	log.Printf("unknown command: %q", message.Content)
}

func (b *ExampleBot) echo(ctx context.Context, _ *botbooter.Bot, message *botbooter.Message) {
	log.Printf("echo command: %q", message.Content)

	reply := "You said: " + strings.TrimPrefix(message.Content, "echo ")
	// Reply threads the response into the triggering message (e.g. inside a Slack
	// thread) instead of posting to the channel root as SendMessageContext would.
	if err := b.Reply(ctx, message, reply); err != nil {
		log.Println("failed to send message:", err)
	}
}
