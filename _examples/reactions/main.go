// Command reactions demonstrates replying when someone adds an emoji reaction.
//
// It registers a single bot.OnReaction handler that fires uniformly across Slack,
// Discord, Telegram and WhatsApp, then replies threaded under the reacted message
// via bot.ReplyToMessage. Like _examples/multi it starts every platform whose
// credentials are present in the environment (or a local .env) and runs them
// concurrently in one process:
//
//	go run ./_examples/reactions      # CLI fallback (no credentials); reactions do not fire on CLI
//	# set any of these groups to run the real reaction handlers:
//	#   SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	#   DISCORD_BOT_TOKEN
//	#   TELEGRAM_BOT_TOKEN
//	#   WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (+ optional WA_PATH)
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
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/cli"
	"github.com/lao/botbooter/discord"
	"github.com/lao/botbooter/slack"
	"github.com/lao/botbooter/telegram"
	"github.com/lao/botbooter/whatsapp"
)

// namedBot pairs a bot with a label so logs identify which one reacted.
type namedBot struct {
	name string
	bot  *botbooter.Bot
}

func main() {
	_ = godotenv.Load(".env")

	bots, err := configuredBots()
	if err != nil {
		log.Fatal(err)
	}
	for _, nb := range bots {
		registerHandlers(nb)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	names := make([]string, len(bots))
	for i, nb := range bots {
		names[i] = nb.name
	}
	log.Printf("botbooter reacting on %d bot(s): %s", len(bots), strings.Join(names, ", "))

	if err := runAll(ctx, bots); err != nil {
		log.Fatal(err)
	}
}

// runAll runs every bot concurrently on one shared context. Bot.Run blocks, so
// each gets its own goroutine. The first bot to fail cancels the context, which
// unblocks the others' Run; the first non-nil error is returned. A clean Ctrl-C
// cancels ctx and every Run returns nil.
func runAll(ctx context.Context, bots []namedBot) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, nb := range bots {
		wg.Add(1)
		go func(nb namedBot) {
			defer wg.Done()
			if err := nb.bot.Run(ctx); err != nil {
				once.Do(func() { firstErr = err })
				log.Printf("[%s] stopped: %v", nb.name, err)
				cancel() // bring the others down too
			}
		}(nb)
	}
	wg.Wait()
	return firstErr
}

// configuredBots builds one bot per platform whose credentials are present,
// falling back to a single CLI bot when nothing is configured.
func configuredBots() ([]namedBot, error) {
	var bots []namedBot

	if appToken, botToken := os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN"); appToken != "" && botToken != "" {
		bots = append(bots, namedBot{"slack", slack.New(appToken, botToken)})
	}
	if token := os.Getenv("DISCORD_BOT_TOKEN"); token != "" {
		bot, err := discord.New(token)
		if err != nil {
			return nil, err
		}
		bots = append(bots, namedBot{"discord", bot})
	}
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		bot, err := telegram.New(token)
		if err != nil {
			return nil, err
		}
		bots = append(bots, namedBot{"telegram", bot})
	}
	if os.Getenv("WA_TOKEN") != "" {
		bot, err := whatsapp.New(whatsapp.Config{
			Token:         os.Getenv("WA_TOKEN"),
			PhoneNumberID: os.Getenv("WA_PHONE_ID"),
			AppSecret:     os.Getenv("WA_APP_SECRET"),
			VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
			Addr:          os.Getenv("WA_ADDR"),
			Path:          os.Getenv("WA_PATH"), // optional; defaults to /webhook
		})
		if err != nil {
			return nil, err
		}
		bots = append(bots, namedBot{"whatsapp", bot})
	}

	if len(bots) == 0 {
		log.Println("no platform credentials found; running a single CLI bot. CLI has no reactions, so nothing will fire — set platform credentials to see OnReaction in action.")
		bots = append(bots, namedBot{"cli", cli.New(os.Stdin, os.Stdout)})
	}
	return bots, nil
}

// registerHandlers wires a reaction handler that replies, threaded under the
// reacted message, acknowledging the emoji.
func registerHandlers(nb namedBot) {
	nb.bot.OnReaction(func(ctx context.Context, b *botbooter.Bot, r *botbooter.Reaction) {
		log.Printf("[%s] reaction %q by %s on message %s", nb.name, r.Emoji, r.UserID, r.MessageID)
		reply := "Thanks for the " + r.Emoji + " reaction!"
		if err := b.ReplyToMessage(ctx, r.ChannelID, r.MessageID, reply); err != nil {
			log.Printf("[%s] failed to reply: %v", nb.name, err)
		}
	})
}
