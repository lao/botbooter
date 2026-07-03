// Command reactions demonstrates replying when someone adds an emoji reaction.
//
// Like _examples/basic it runs a SINGLE bot, chosen by the first argument, so the
// example stays focused on the one thing it shows: a bot.OnReaction handler that
// fires uniformly across Slack, Discord, Telegram and WhatsApp and replies,
// threaded under the reacted message, via bot.ReplyToMessage.
//
//	go run ./_examples/reactions            # CLI mode (no credentials); CLI has no reactions
//	go run ./_examples/reactions slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/reactions discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/reactions telegram   # reads TELEGRAM_BOT_TOKEN
//	go run ./_examples/reactions whatsapp   # reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
//
// Per-platform setup gotchas for reactions to actually arrive:
//   - Slack: subscribe the app to the reaction_added Events API event and grant
//     the reactions:read scope; otherwise OnReaction never fires.
//   - Discord: the constructor requests the message-reaction gateway intents, but
//     they must also be enabled in the Discord developer portal.
//   - Telegram: reactions are delivered in private chats, and in groups only when
//     the bot is an administrator.
//   - WhatsApp: reactions arrive on the same inbound webhook as messages.
//
// Emoji is platform-shaped: a shortname ("thumbsup") on Slack, a unicode
// character elsewhere.
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
	"github.com/lao/botbooter/cli"
	"github.com/lao/botbooter/discord"
	"github.com/lao/botbooter/slack"
	"github.com/lao/botbooter/telegram"
	"github.com/lao/botbooter/whatsapp"
)

func main() {
	_ = godotenv.Load(".env")

	botType := requestedBotType(os.Args)
	bot, err := newBot(botType)
	if err != nil {
		log.Fatal(err)
	}

	// The whole point of this example: reply when someone adds an emoji reaction.
	// The handler runs the same on every platform that surfaces reactions; the
	// reply is threaded under the reacted message by bot.ReplyToMessage.
	bot.OnReaction(func(ctx context.Context, b *botbooter.Bot, r *botbooter.Reaction) {
		log.Printf("reaction %q by %s on message %s", r.Emoji, r.UserID, r.MessageID)
		reply := "Thanks for the " + r.Emoji + " reaction!"
		if err := b.ReplyToMessage(ctx, r.ChannelID, r.MessageID, reply); err != nil {
			log.Printf("failed to reply: %v", err)
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("botbooter reacting as %q bot", botType)
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func requestedBotType(args []string) string {
	if len(args) > 1 {
		return strings.ToLower(args[1])
	}
	return "cli"
}

func newBot(botType string) (*botbooter.Bot, error) {
	switch botType {
	case "slack":
		return slack.New(slack.Config{AppToken: os.Getenv("SLACK_APP_TOKEN"), BotToken: os.Getenv("SLACK_BOT_TOKEN")})
	case "discord":
		return discord.New(os.Getenv("DISCORD_BOT_TOKEN"))
	case "telegram":
		return telegram.New(os.Getenv("TELEGRAM_BOT_TOKEN"))
	case "whatsapp":
		return whatsapp.New(whatsapp.Config{
			Token:         os.Getenv("WA_TOKEN"),
			PhoneNumberID: os.Getenv("WA_PHONE_ID"),
			AppSecret:     os.Getenv("WA_APP_SECRET"),
			VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
			Addr:          os.Getenv("WA_ADDR"),
			Path:          os.Getenv("WA_PATH"), // optional; defaults to /webhook
		})
	case "cli":
		fmt.Fprintln(os.Stderr, "CLI has no reactions; run with slack, discord, telegram or whatsapp to see OnReaction fire.")
		return cli.New(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram, whatsapp or cli)", botType)
	}
}
