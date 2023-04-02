package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/lao/botbooter"
)

func echoHandler(bot *botbooter.Bot, message *botbooter.Message) {
	attachments, err := bot.GetAttachments(message)
	if err != nil {
		log.Println("Failed to get attachments:", err)
	}
	log.Println(attachments)
	bot.SendMessage(message.ChannelID, "You said: "+strings.Replace(message.Content, "echo ", "", 1))
}

func loggingMiddleware(bot *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
	fmt.Printf("User %s sent a message in channel %s: %s\n", message.UserID, message.ChannelID, message.Content)
	next(bot, message)
}

func main() {
	godotenv.Load(".env")

	var b *botbooter.Bot

	botType := os.Args[1]

	if strings.ToLower(botType) == "slack" {
		botToken := os.Getenv("SLACK_BOT_TOKEN")
		appToken := os.Getenv("SLACK_APP_TOKEN")
		b = botbooter.InitAsSlackBot(appToken, botToken)
	} else if strings.ToLower(botType) == "discord" {
		DISCORD_BOT_TOKEN := os.Getenv("DISCORD_BOT_TOKEN")
		b = botbooter.InitAsDiscordBot(DISCORD_BOT_TOKEN)
	} else {
		log.Fatal("Invalid bot type")
		return
	}

	b.AddMiddleware(loggingMiddleware)

	b.AddHandler(botbooter.Command{
		Pattern: "^wetransfer ",
		Handler: echoHandler,
	})

	b.SetUnknownCommandHandler(func(bot *botbooter.Bot, message *botbooter.Message) {
		fmt.Println("Unknown command:", message.Content, message.ChannelID)
	})

	err := b.Connect()
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer b.Disconnect()

	b.StartListening()
}
