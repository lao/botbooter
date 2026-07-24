// Command flow demonstrates a botbooter conversational flow: a multi-step,
// context-aware dialog registered with HandleFlow. It runs a small "signup" form
// on the local CLI, so it needs no credentials:
//
//	go run ./_examples/flow   # then type "signup" and answer each prompt
//
// Flows are DM-intended. While one is active it shadows the command table, so
// every later message in that conversation is routed to the flow as an answer —
// until the form completes, the user types the cancel word ("cancel"), or it
// times out. Wire the same flow onto Slack/Discord/Telegram/etc. exactly as the
// basic example wires its commands; the CLI just keeps this demo credential-free.
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
	"github.com/lao/botbooter/cli"
)

func main() {
	_ = godotenv.Load(".env")

	bot := cli.New(os.Stdin, os.Stdout)
	if err := registerSignup(bot); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println(`flow demo running: type "signup" and press enter (Ctrl-D or "cancel" to quit).`)
	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// registerSignup wires a small multi-step sign-up flow, triggered by "signup".
//
// The "password" step uses Secret(), which keeps the answer out of framework
// logs and any future serialized Store state (it is not encryption, and not safe
// in a public channel).
func registerSignup(b *botbooter.Bot) error {
	signup := botbooter.NewFlow("signup").
		Ask("name", "What's your name?").
		Ask("email", "What's your email?", botbooter.Validate(validEmail)).
		Ask("password", "Choose a password.", botbooter.Secret()).
		OnComplete(func(ctx context.Context, bot *botbooter.Bot, m *botbooter.Message, a botbooter.Answers) {
			// In a real bot this would create the account. User PII (name, email)
			// is kept out of the log by default — like the password; add explicit,
			// audited logging if you need it.
			log.Printf("signup complete")
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

// validEmail is a deliberately minimal, illustrative check — not a real email
// validator. A production bot would use a proper parser (e.g. net/mail.ParseAddress).
func validEmail(s string) error {
	if !strings.Contains(s, "@") {
		return errors.New("that doesn't look like an email — try again")
	}
	return nil
}
