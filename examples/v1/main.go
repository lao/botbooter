// Command v1 is a small demo of botbooter. It runs an "echo" bot on Slack,
// Discord, Telegram or the local CLI.
//
//	go run ./examples/v1            # CLI mode (no credentials needed)
//	go run ./examples/v1 slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./examples/v1 discord    # reads DISCORD_BOT_TOKEN
//	go run ./examples/v1 telegram   # reads TELEGRAM_BOT_TOKEN
package main

import (
	"cmp"
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

func echoHandler(ctx context.Context, bot *botbooter.Bot, message *botbooter.Message) {
	reply := "You said: " + strings.TrimPrefix(message.Content, "echo ")
	if err := bot.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
		log.Println("failed to send message:", err)
	}
}

// loggingMiddleware dumps every field of each incoming message plus its
// attachments, then continues the chain. Running as middleware (rather than a
// command handler) is deliberate: middleware sees every message, including
// media-only uploads whose text matches no command pattern — a Slack file share
// or a Telegram photo sent without a caption both arrive with empty Content and
// would never reach a "^echo "-style handler.
func loggingMiddleware(ctx context.Context, bot *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
	// AuthorName is best-effort; empty on platforms that deliver only an id (e.g. Slack).
	log.Printf("message from %s in channel %s:", cmp.Or(message.AuthorName, message.UserID), message.ChannelID)
	log.Printf("  ID:         %s", message.ID)
	log.Printf("  UserID:     %s", message.UserID)
	log.Printf("  AuthorName: %s", message.AuthorName)
	log.Printf("  ChannelID:  %s", message.ChannelID)
	log.Printf("  Content:    %s", message.Content)
	log.Printf("  Timestamp:  %s", message.Timestamp)
	log.Printf("  ReplyToID:  %s", message.ReplyToID)
	log.Printf("  Mentions:   %v", message.Mentions)

	// URL is empty on platforms that deliver media by id rather than link (e.g.
	// Telegram carries the FileID in ExtraData); resolve it via the raw client if
	// you need the bytes.
	if attachments, err := bot.GetAttachments(message); err != nil {
		log.Println("  failed to get attachments:", err)
	} else {
		for i, a := range attachments {
			log.Printf("  attachment[%d]: isImage=%t url=%q extraData=%+v", i, a.IsImage, a.URL, a.ExtraData)
		}
	}

	next(ctx, bot, message)
}

func newBot(botType string) (*botbooter.Bot, error) {
	switch botType {
	case "slack":
		return botbooter.InitAsSlackBot(os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN")), nil
	case "discord":
		return botbooter.InitAsDiscordBot(os.Getenv("DISCORD_BOT_TOKEN"))
	case "telegram":
		return botbooter.InitAsTelegramBot(os.Getenv("TELEGRAM_BOT_TOKEN"))
	case "cli":
		return botbooter.InitAsCLIBot(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram or cli)", botType)
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
		fmt.Fprintln(os.Stderr, `Type "echo <text>" and press enter (Ctrl-D to quit).`)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("botbooter running as %q bot", botType)
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
