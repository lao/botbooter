package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/lao/botbooter"
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
		return botbooter.InitAsSlackBot(os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN")), nil
	case "discord":
		return botbooter.InitAsDiscordBot(os.Getenv("DISCORD_BOT_TOKEN"))
	case "telegram":
		return botbooter.InitAsTelegramBot(os.Getenv("TELEGRAM_BOT_TOKEN"))
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
		fmt.Fprintln(os.Stderr, `Type "echo <text>" and press enter (Ctrl-D to quit).`)
		return botbooter.InitAsCLIBot(os.Stdin, os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown bot type %q (want slack, discord, telegram, whatsapp or cli)", botType)
	}
}
