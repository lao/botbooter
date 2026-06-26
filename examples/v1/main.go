// Command v1 is a small demo of botbooter. It runs an "echo" bot on Slack,
// Discord, WhatsApp or the local CLI.
//
//	go run ./examples/v1            # CLI mode (no credentials needed)
//	go run ./examples/v1 slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./examples/v1 discord    # reads DISCORD_BOT_TOKEN
//	go run ./examples/v1 whatsapp   # reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
)

// echoHandler logs any attachments, then replies with the message text minus
// the leading "echo " prefix.
func echoHandler(ctx context.Context, bot *botbooter.Bot, message *botbooter.Message) {
	if attachments, err := bot.GetAttachments(message); err != nil {
		log.Println("failed to get attachments:", err)
	} else {
		for _, a := range attachments {
			kind := "file"
			if a.IsImage {
				kind = "image"
			}
			log.Printf("attachment (%s): %s", kind, a.URL)
		}
	}

	reply := "You said: " + strings.TrimPrefix(message.Content, "echo ")
	if err := bot.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
		log.Println("failed to send message:", err)
	}
}

// loggingMiddleware logs each incoming message before passing it to next.
func loggingMiddleware(ctx context.Context, bot *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
	log.Printf("user %s in channel %s: %s", message.UserID, message.ChannelID, message.Content)
	next(ctx, bot, message)
}

// newBot builds a bot for the named platform ("slack", "discord" or "cli"),
// reading credentials from the environment, and errors on an unknown type.
func newBot(botType string) (*botbooter.Bot, error) {
	switch botType {
	case "slack":
		return botbooter.InitAsSlackBot(os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN")), nil
	case "discord":
		return botbooter.InitAsDiscordBot(os.Getenv("DISCORD_BOT_TOKEN"))
	case "whatsapp":
		return botbooter.InitAsWhatsAppBot(botbooter.WhatsAppConfig{
			Token:         os.Getenv("WA_TOKEN"),
			PhoneNumberID: os.Getenv("WA_PHONE_ID"),
			AppSecret:     os.Getenv("WA_APP_SECRET"),
			VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
			Addr:          os.Getenv("WA_ADDR"),
			Path:          os.Getenv("WA_PATH"), // optional; defaults to /webhook
		})
	case "cli":
		return botbooter.InitAsCLIBot(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, whatsapp or cli)", botType)
	}
}

func main() {
	_ = godotenv.Load(".env")

	botType := "cli"
	if len(os.Args) > 1 {
		botType = strings.ToLower(os.Args[1])
	}

	bot, err := newBot(botType)
	if err != nil {
		log.Fatal(err)
	}

	bot.AddMiddleware(loggingMiddleware)

	if err := bot.AddHandler(botbooter.Command{Pattern: "^echo ", Handler: echoHandler}); err != nil {
		log.Fatal(err)
	}

	bot.SetUnknownCommandHandler(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		log.Printf("unknown command: %q", m.Content)
	})

	if botType == "cli" {
		fmt.Println(`Type "echo <text>" and press enter (Ctrl-D to quit).`)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
