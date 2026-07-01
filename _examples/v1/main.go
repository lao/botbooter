// Command v1 is a small demo of botbooter. It runs an "echo" bot on Slack,
// Discord, Telegram, WhatsApp or the local CLI.
//
//	go run ./_examples/v1            # CLI mode (no credentials needed)
//	go run ./_examples/v1 slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/v1 discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/v1 telegram   # reads TELEGRAM_BOT_TOKEN
//	go run ./_examples/v1 whatsapp   # reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
package main

import (
	"context"
	"errors"
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
	if err := registerSignup(b.Bot); err != nil {
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

// registerSignup wires a small multi-step sign-up flow, triggered by "signup".
//
// Flows are DM-intended: while one is active it shadows the command table, so
// every message in that conversation becomes an answer until the form completes,
// the user types the cancel word ("cancel"), or it times out. The "password" step
// uses Secret(), which keeps the answer out of framework logs and any future
// serialized Store state (it is not encryption, and not safe in a public channel).
func registerSignup(b *botbooter.Bot) error {
	signup := botbooter.NewFlow("signup").
		Ask("name", "What's your name?").
		Ask("email", "What's your email?", botbooter.Validate(validEmail)).
		Ask("password", "Choose a password.", botbooter.Secret()).
		OnComplete(func(ctx context.Context, bot *botbooter.Bot, m *botbooter.Message, a botbooter.Answers) {
			// In a real bot this would create the account; the password is never logged.
			log.Printf("signup complete: name=%q email=%q", a.Get("name"), a.Get("email"))
			if err := bot.SendMessageContext(ctx, m.ChannelID, "You're all set 🎉"); err != nil {
				log.Println("failed to send completion message:", err)
			}
		}).
		OnCancel(func(ctx context.Context, bot *botbooter.Bot, m *botbooter.Message) {
			if err := bot.SendMessageContext(ctx, m.ChannelID, "No worries — signup cancelled."); err != nil {
				log.Println("failed to send cancel message:", err)
			}
		})

	return b.HandleFlow("^signup$", signup)
}

func validEmail(s string) error {
	if !strings.Contains(s, "@") {
		return errors.New("that doesn't look like an email — try again")
	}
	return nil
}
