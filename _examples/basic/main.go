// Command basic is a small demo of botbooter. It runs an "echo" bot on Slack,
// Discord, Telegram, WhatsApp or the local CLI.
//
//	go run ./_examples/basic            # CLI mode (no credentials needed)
//	go run ./_examples/basic slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/basic discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/basic telegram   # reads TELEGRAM_BOT_TOKEN
//	go run ./_examples/basic whatsapp   # reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
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
	if err := b.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
		log.Println("failed to send message:", err)
	}
}
