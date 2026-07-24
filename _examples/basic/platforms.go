package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/lao/botbooter"
	"github.com/lao/botbooter/cli"
	"github.com/lao/botbooter/discord"
	"github.com/lao/botbooter/github"
	"github.com/lao/botbooter/gitlab"
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
		// PAT mode (GITHUB_TOKEN) or GitHub App mode (GITHUB_APP_ID +
		// GITHUB_INSTALLATION_ID + GITHUB_PRIVATE_KEY_FILE); set one, not both.
		cfg := github.Config{
			Token:         os.Getenv("GITHUB_TOKEN"),
			WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
			Addr:          os.Getenv("GITHUB_ADDR"),
			Path:          os.Getenv("GITHUB_PATH"), // optional; defaults to /webhook
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
	case "gitlab":
		return gitlab.New(gitlab.Config{
			Token:   os.Getenv("GITLAB_TOKEN"),  // personal, project or group access token
			Secret:  os.Getenv("GITLAB_SECRET"), // the webhook's "Secret token"
			Addr:    os.Getenv("GITLAB_ADDR"),
			Path:    os.Getenv("GITLAB_PATH"),     // optional; defaults to /webhook
			BaseURL: os.Getenv("GITLAB_BASE_URL"), // optional; empty targets gitlab.com
		})
	case "cli":
		fmt.Fprintln(os.Stderr, `Type "echo <text>" and press enter (Ctrl-D to quit).`)
		return cli.New(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram, whatsapp, whatsmeow, teams, github, gitlab or cli)", botType)
	}
}
