# botbooter

Inspired by [Gin](https://gin-gonic.com/), supposed to act as a general purpose golang framework to build bots for Slack and Discord and Microsoft Teams, Telegram, Whatsapp, CLI (for testing) and more in the future.

> NOT READY FOR PRODUCTION

## Features

- Generic handler for connections for bot types
- Generic message handler with support for attachments for all bot types
- Middleware support

## Install
```bash
  go get -u github.com/lao/botbooter
```

## How to use?

Here is a simple example of how to use the BotBooter.

```golang
  package main

  import (
    "fmt"
    "log"
    "os"

    "github.com/joho/godotenv"
    "github.com/lao/botbooter"
  )

  func echoHandler(bot *botbooter.Bot, message *botbooter.Message) {
    bot.SendMessage(message.ChannelID, "You said: "+message.Content)
  }

  func loggingMiddleware(bot *botbooter.Bot, message *botbooter.Message, next botbooter.CommandHandler) {
    fmt.Printf("User %s sent a message in channel %s: %s\n", message.UserID, message.ChannelID, message.Content)
    next(bot, message)
  }

  func main() {
    godotenv.Load(".env")

    var b *botbooter.Bot

    botToken := os.Getenv("SLACK_BOT_TOKEN")
    appToken := os.Getenv("SLACK_APP_TOKEN")
    b = botbooter.InitAsSlackBot(appToken, botToken)
    // SAME CODE SHOULD WORK FOR DISCORD OR SLACK
    // DISCORD_BOT_TOKEN := os.Getenv("DISCORD_BOT_TOKEN")
    // b = botbooter.InitAsDiscordBot(DISCORD_BOT_TOKEN)

    b.AddMiddleware(loggingMiddleware)

    b.AddHandler(botbooter.Command{
      Pattern: "^echo ",
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
```

## DEMO

for slack and discord:

https://user-images.githubusercontent.com/197033/229368894-19b366d3-ca6d-41d2-9ab7-ca8e1a53b31a.mov

## Why

Alternatives:

### [Joe-bot](https://joe-bot.net/?utm_campaign=awesomego&utm_medium=referral&utm_source=awesomego) 
 
- no support for discord
- no generic access for attachments in messages

### [GoSarah](https://github.com/oklahomer/go-sarah)

- no support for discord
- no generic access for attachments in messages

