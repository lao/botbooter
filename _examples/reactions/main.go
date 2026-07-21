// Command reactions demonstrates replying when someone adds an emoji reaction.
//
// Like _examples/basic it runs a SINGLE bot, chosen by the first argument, so the
// example stays focused on the one thing it shows: a bot.OnReaction handler that
// fires uniformly across Slack, Discord, Telegram, WhatsApp (both flavors) and
// GitHub (polled, opt-in — see the gotchas below) and replies, threaded under
// the reacted message, via bot.ReplyToMessage.
//
// Like _examples/basic it logs every inbound message (middleware) and every
// unmatched command, so connectivity can be verified in stages: (1) send any
// message — a "message from ..." log proves message ingress works; (2) send
// "echo <text>" — the bot posts a message back; (3) add an emoji reaction to
// that message — a "reaction ..." log plus a threaded reply proves reaction
// ingress works. If (1) works but (3) stays silent, the platform is not
// delivering reaction events (see the gotchas below) — the code path is the
// same.
//
//	go run ./_examples/reactions            # CLI mode (no credentials); CLI has no reactions
//	go run ./_examples/reactions slack      # reads SLACK_APP_TOKEN / SLACK_BOT_TOKEN
//	go run ./_examples/reactions discord    # reads DISCORD_BOT_TOKEN
//	go run ./_examples/reactions telegram   # reads TELEGRAM_BOT_TOKEN
//	go run ./_examples/reactions whatsapp   # Cloud API flavor: reads WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (and optional WA_PATH, default /webhook)
//	go run ./_examples/reactions whatsmeow  # WhatsApp Web flavor: no credentials; scan the QR on first run (optional WA_MEOW_DB)
//	go run ./_examples/reactions teams      # reads TEAMS_APP_ID / TEAMS_APP_PASSWORD / TEAMS_ADDR (and optional TEAMS_APP_TENANT_ID, TEAMS_PATH)
//	go run ./_examples/reactions github     # reads GITHUB_TOKEN (or GITHUB_APP_ID / GITHUB_INSTALLATION_ID / GITHUB_PRIVATE_KEY_FILE) / GITHUB_WEBHOOK_SECRET / GITHUB_ADDR / GITHUB_REPO (comma-separated "owner/name" list; and optional GITHUB_PATH, GITHUB_POLL_SECONDS)
//
// Per-platform setup gotchas for reactions to actually arrive:
//   - Slack: subscribe the app to the reaction_added Events API event and grant
//     the reactions:read scope; otherwise OnReaction never fires.
//   - Discord: the constructor requests the message-reaction gateway intents, but
//     they must also be enabled in the Discord developer portal.
//   - Telegram: reactions are delivered in private chats, and in groups only when
//     the bot is an administrator.
//   - WhatsApp (Cloud API): reactions arrive on the same inbound webhook as
//     messages.
//   - whatsmeow: reactions arrive over the same websocket as messages; nothing
//     extra to configure. Replies are unthreaded (the adapter has no quoted-reply
//     egress yet, so ReplyToMessage falls back to a plain send).
//   - Teams: the adapter does not surface reaction events yet, so the echo
//     command works but OnReaction never fires.
//   - GitHub: the platform sends no webhook for reactions at all (a
//     long-requested feature GitHub has not shipped), so the adapter polls
//     instead — an opt-in: set GITHUB_REPO to one or more comma-separated
//     "owner/name" entries ("lao/botbooter,lao/other") to poll each repo's
//     newest issue comments (Config.ReactionPollRepos); the poller only starts
//     when an OnReaction handler is registered before the bot runs (this
//     example always registers one). Reactions arrive within
//     the poll interval (GITHUB_POLL_SECONDS, default 30), only for the newest
//     comments, and only while the bot is running. Without GITHUB_REPO the echo
//     command works but OnReaction never fires.
//
// Emoji renders as-is on its origin platform: a unicode character on most
// platforms, a colon-wrapped shortname (":thumbsup:") on Slack, "<:name:id>"
// markup for Discord custom emojis.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/cli"
	"github.com/lao/botbooter/discord"
	"github.com/lao/botbooter/github"
	"github.com/lao/botbooter/slack"
	"github.com/lao/botbooter/teams"
	"github.com/lao/botbooter/telegram"
	"github.com/lao/botbooter/whatsapp/cloud"
	"github.com/lao/botbooter/whatsapp/whatsmeow"
)

func main() {
	_ = godotenv.Load(".env")

	botType := requestedBotType(os.Args)
	bot, err := newBot(botType)
	if err != nil {
		log.Fatal(err)
	}

	bot.AddMiddleware(func(ctx context.Context, b *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
		log.Printf("message from %s in channel %s: %q (id %s)", cmp.Or(message.AuthorName, message.UserID), message.ChannelID, message.Content, message.ID)
		next(ctx, b, message)
	})

	echo := func(ctx context.Context, b *botbooter.Bot, message *botbooter.Message) {
		reply := "You said: " + strings.TrimPrefix(message.Content, "echo ") + " — now add an emoji reaction to this message."
		if err := b.SendMessageContext(ctx, message.ChannelID, reply); err != nil {
			log.Printf("failed to send: %v", err)
		}
	}
	bot.HandleFunc("^echo ", echo)

	bot.SetUnknownCommandHandler(func(_ context.Context, _ *botbooter.Bot, message *botbooter.Message) {
		log.Printf("unknown command: %q (send \"echo <text>\" to get a message to react to)", message.Content)
	})

	// The whole point of this example: reply when someone adds an emoji reaction.
	// The handler runs the same on every platform that surfaces reactions; the
	// reply is threaded under the reacted message by bot.ReplyToMessage.
	bot.OnReaction(func(ctx context.Context, b *botbooter.Bot, r *botbooter.Reaction) {
		log.Printf("reaction %q by %s on message %s", r.Emoji, r.UserID, r.MessageID)
		// r.Emoji renders as-is on the platform it came from: a unicode char on
		// most platforms, ":shortname:" on Slack, "<:name:id>" for Discord custom
		// emojis — no per-platform formatting needed here.
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
		return cloud.New(cloud.Config{
			Token:         os.Getenv("WA_TOKEN"),
			PhoneNumberID: os.Getenv("WA_PHONE_ID"),
			AppSecret:     os.Getenv("WA_APP_SECRET"),
			VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
			Addr:          os.Getenv("WA_ADDR"),
			Path:          os.Getenv("WA_PATH"), // optional; defaults to /webhook
		})
	case "whatsmeow":
		return whatsmeow.New(whatsmeow.Config{DBPath: os.Getenv("WA_MEOW_DB")}) // "" -> botbooter-whatsapp-meow.db
	case "teams":
		fmt.Fprintln(os.Stderr, "note: the Teams adapter does not surface reaction events yet, so OnReaction will not fire.")
		return teams.New(teams.Config{
			AppID:       os.Getenv("TEAMS_APP_ID"),
			AppPassword: os.Getenv("TEAMS_APP_PASSWORD"),
			TenantID:    os.Getenv("TEAMS_APP_TENANT_ID"), // optional; single-tenant
			Addr:        os.Getenv("TEAMS_ADDR"),
			Path:        os.Getenv("TEAMS_PATH"), // optional; defaults to /api/messages
		})
	case "github":
		cfg := github.Config{
			Token:         os.Getenv("GITHUB_TOKEN"),
			WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
			Addr:          os.Getenv("GITHUB_ADDR"),
			Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
		}
		// GitHub has no reaction webhook; the adapter polls instead, opt-in per
		// repository. GITHUB_REPO takes one or more comma-separated "owner/name"
		// entries; entries are trimmed here (a leading space would survive the
		// library's format check as part of the owner) while format errors are
		// left to github.New's validation. Without any repo the bot still
		// echoes, but OnReaction never fires.
		for _, repo := range strings.Split(os.Getenv("GITHUB_REPO"), ",") {
			if repo = strings.TrimSpace(repo); repo != "" {
				cfg.ReactionPollRepos = append(cfg.ReactionPollRepos, repo)
			}
		}
		if len(cfg.ReactionPollRepos) > 0 {
			if s := os.Getenv("GITHUB_POLL_SECONDS"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil || n <= 0 {
					return nil, fmt.Errorf("parse GITHUB_POLL_SECONDS %q", s)
				}
				cfg.ReactionPollInterval = time.Duration(n) * time.Second
			}
		} else {
			fmt.Fprintln(os.Stderr, "note: set GITHUB_REPO (comma-separated \"owner/name\" entries) to poll for reactions; without it OnReaction never fires.")
		}
		if keyFile := os.Getenv("GITHUB_PRIVATE_KEY_FILE"); keyFile != "" {
			key, err := os.ReadFile(keyFile)
			if err != nil {
				return nil, fmt.Errorf("read GITHUB_PRIVATE_KEY_FILE: %w", err)
			}
			appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse GITHUB_APP_ID: %w", err)
			}
			installationID, err := strconv.ParseInt(os.Getenv("GITHUB_INSTALLATION_ID"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse GITHUB_INSTALLATION_ID: %w", err)
			}
			cfg.AppID, cfg.InstallationID, cfg.PrivateKey = appID, installationID, key
		}
		return github.New(cfg)
	case "cli":
		fmt.Fprintln(os.Stderr, "CLI has no reactions; run with slack, discord, telegram, whatsapp or whatsmeow to see OnReaction fire.")
		return cli.New(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram, whatsapp, whatsmeow, teams, github or cli)", botType)
	}
}
