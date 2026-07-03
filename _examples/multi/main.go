// Command multi demonstrates running several botbooter bots in one process.
//
// Each xxx.New(...) returns an independent *botbooter.Bot with no shared global
// state, so any mix of platforms — different or the same — coexists in one main.
// Bot.Run blocks, so this example runs each bot in its own goroutine on a single
// shared, cancelable context: one Ctrl-C (or any bot's loop failing) tears them
// all down together.
//
// It starts every platform whose credentials are present in the environment (or
// a local .env), so the same binary can drive Slack + Discord + Telegram +
// WhatsApp at once. With nothing configured it falls back to a single CLI bot so
// the example still runs with no credentials:
//
//	go run ./_examples/multi         # CLI only (no credentials needed)
//	# or set any of these groups and they all run side by side:
//	#   SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	#   DISCORD_BOT_TOKEN
//	#   TELEGRAM_BOT_TOKEN
//	#   WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (+ optional WA_PATH)
//
// Two of the SAME platform work too — give each its own credentials, and note
// two WhatsApp bots need distinct WA_ADDR ports and two CLI bots would fight over
// stdin (so run at most one CLI per process).
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

// namedBot pairs a bot with a label so logs identify which one spoke.
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
	log.Printf("botbooter running %d bot(s): %s", len(bots), strings.Join(names, ", "))

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
		bot, err := slack.New(slack.Config{AppToken: appToken, BotToken: botToken})
		if err != nil {
			return nil, err
		}
		bots = append(bots, namedBot{"slack", bot})
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
		log.Println(`no platform credentials found; running a single CLI bot. Type "echo <text>" and press enter (Ctrl-D to quit).`)
		bots = append(bots, namedBot{"cli", cli.New(os.Stdin, os.Stdout)})
	}
	return bots, nil
}

// registerHandlers wires the same echo behavior onto a bot, tagging replies with
// the bot's name so it is clear which instance answered.
func registerHandlers(nb namedBot) {
	echo := func(ctx context.Context, b *botbooter.Bot, message *botbooter.Message) {
		log.Printf("[%s] echo: %q", nb.name, message.Content)
		reply := "[" + nb.name + "] You said: " + strings.TrimPrefix(message.Content, "echo ")
		if err := b.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
			log.Printf("[%s] failed to send: %v", nb.name, err)
		}
	}
	if err := nb.bot.HandleFunc("^echo ", echo); err != nil {
		log.Fatal(err)
	}
}
