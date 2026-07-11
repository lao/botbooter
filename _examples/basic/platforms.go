package main

import (
	"fmt"
	"os"
	"strings"

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
		// WhatsApp Web flavor: QR-links to a phone on first run, then reuses the
		// session stored in the SQLite file.
		return whatsmeow.New(whatsmeow.Config{DBPath: os.Getenv("WA_MEOW_DB")}) // "" -> botbooter-whatsapp-meow.db
	case "teams":
		return teams.New(teams.Config{
			AppID:       os.Getenv("TEAMS_APP_ID"),
			AppPassword: os.Getenv("TEAMS_APP_PASSWORD"),
			TenantID:    os.Getenv("TEAMS_APP_TENANT_ID"), // optional; single-tenant
			Addr:        os.Getenv("TEAMS_ADDR"),
			Path:        os.Getenv("TEAMS_PATH"), // optional; defaults to /api/messages
		})
	case "github":
		// PAT mode; for GitHub App auth set AppID/InstallationID/PrivateKey instead.
		return github.New(github.Config{
			Token:         os.Getenv("GITHUB_TOKEN"),
			WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
			Addr:          os.Getenv("GITHUB_ADDR"),
			Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
		})
	case "cli":
		fmt.Fprintln(os.Stderr, `Type "echo <text>" and press enter (Ctrl-D to quit).`)
		return cli.New(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram, whatsapp, whatsmeow, teams, github or cli)", botType)
	}
}
