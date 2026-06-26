# botbooter

[![Go Reference](https://pkg.go.dev/badge/github.com/lao/botbooter.svg)](https://pkg.go.dev/github.com/lao/botbooter)
[![CI](https://github.com/lao/botbooter/actions/workflows/ci.yml/badge.svg)](https://github.com/lao/botbooter/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/lao/botbooter)](https://goreportcard.com/report/github.com/lao/botbooter)
[![Go Version](https://img.shields.io/github/go-mod/go-version/lao/botbooter)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> A small, framework-style toolkit for writing chat bots **once** and running them on **Slack, Discord, Telegram, or a local CLI** — with the same handlers, middleware, and attachment access on every platform.

Inspired by [Gin](https://gin-gonic.com/): you register pattern-matched command handlers and optional middleware, then run the bot. botbooter abstracts the platform behind a single `Bot` type so your business logic does not care whether a message came from Slack, Discord, Telegram, or stdin.

> ⚠️ **Pre-1.0** — the public API may still change.

## Features

- **One API, multiple platforms** — Slack (Socket Mode), Discord (Gateway), Telegram (long polling), and a built-in **CLI adapter** for local development and testing with no credentials.
- **Regex command routing** — patterns are compiled once and matched against message content; first match wins.
- **Middleware chain** — wrap every message (logging, auth, metrics, …) with `next`-style composition.
- **Platform-agnostic attachments** — read image/file attachments uniformly across platforms.
- **Context-first & graceful shutdown** — handlers receive a `context.Context`; `Run(ctx)` / `Start()` connect and shut down cleanly on cancellation or `SIGINT`/`SIGTERM`.
- **Resilient dispatch** — a panicking handler is recovered and logged instead of taking down the bot.

## Install

```bash
go get github.com/lao/botbooter
```

Requires Go 1.23+.

## Quickstart

The fastest way to try it — the CLI adapter needs **no tokens**:

```go
package main

import (
	"context"
	"os"
	"strings"

	"github.com/lao/botbooter"
)

func main() {
	bot := botbooter.InitAsCLIBot(os.Stdin, os.Stdout)

	_ = bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	_ = bot.Run(ctx) // type "echo hi", press enter; Ctrl-D to quit
}
```

Or run the bundled example directly:

```bash
go run ./examples/v1            # CLI mode (default, no credentials)
go run ./examples/v1 slack      # uses SLACK_APP_TOKEN / SLACK_BOT_TOKEN
go run ./examples/v1 discord    # uses DISCORD_BOT_TOKEN
go run ./examples/v1 telegram   # uses TELEGRAM_BOT_TOKEN
```

## Concepts

### Constructing a bot

| Constructor | Signature | Notes |
|---|---|---|
| `InitAsCLIBot(in io.Reader, out io.Writer)` | `*Bot` | Local adapter; `nil` defaults to stdin/stdout. |
| `InitAsSlackBot(appToken, botToken string)` | `*Bot` | Socket Mode (`xapp-…` + `xoxb-…`). |
| `InitAsDiscordBot(token string)` | `(*Bot, error)` | Enables the message-content intent (see below). |
| `InitAsTelegramBot(token string)` | `(*Bot, error)` | Long polling via `getUpdates`; BotFather token. |

### Handlers, commands and middleware

```go
// A command routes messages whose content matches a regular expression.
_ = bot.AddHandler(botbooter.Command{
	Pattern: "^ping$",
	Handler: func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, "pong")
	},
})

// HandleFunc is a shorthand for the common case.
_ = bot.HandleFunc("^hello", greetHandler)

// Fallback when nothing matches.
bot.SetUnknownCommandHandler(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
	_ = b.SendMessageContext(ctx, m.ChannelID, "unknown command")
})

// Middleware wraps dispatch; call next to continue the chain.
bot.AddMiddleware(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message, next botbooter.CommandHandler) {
	log.Printf("%s in %s: %s", m.UserID, m.ChannelID, m.Content)
	next(ctx, b, m)
})
```

`AddHandler` / `HandleFunc` return an error if the pattern is not a valid regular expression.

### Attachments

```go
attachments, err := b.GetAttachments(m)
for _, a := range attachments {
	fmt.Println(a.URL, a.IsImage) // a.ExtraData holds the raw platform payload
}
```

A terminal has no real upload channel, so the **CLI adapter treats any local file path in the message as an attachment** — "uploading" means referencing the path. Image files are detected by content sniffing:

```text
echo here is my screenshot /tmp/cat.png
  → attachment (image): /tmp/cat.png
```

### Lifecycle

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

if err := bot.Run(ctx); err != nil { // connect, serve, and shut down on cancel
	log.Fatal(err)
}
```

- `Run(ctx)` — connect, block until `ctx` is canceled (or the event loop ends), then disconnect cleanly.
- `Start()` — shorthand for `Run` with a context bound to `SIGINT`/`SIGTERM`.
- `Connect(ctx)` / `Disconnect()` — non-blocking control if you want to manage the loop yourself. `Disconnect` is idempotent.

## Platform setup

Each platform takes different credentials. Full step-by-step setup,
[troubleshooting](docs/platforms.md#no-response), and the official
documentation for each live in **[docs/platforms.md](docs/platforms.md)**.

| Platform | What you need | Setup |
|---|---|---|
| Slack | `xapp-…` app-level token + `xoxb-…` bot token | [docs/platforms.md](docs/platforms.md#slack) |
| Discord | bot token + Message Content Intent | [docs/platforms.md](docs/platforms.md#discord) |
| Telegram | BotFather bot token | [docs/platforms.md](docs/platforms.md#telegram) |
| CLI | nothing (local stdin/stdout) | [docs/platforms.md](docs/platforms.md#cli) |

## Development

```bash
make all        # fmt + vet + lint + test
make test-race  # race detector
make cover      # coverage report
make run-cli    # run the example bot in CLI mode
```

The suite runs under the race detector and is hermetic by default. The single test that touches the Slack network is opt-in, enabled by setting the `BOTBOOTER_SLACK_NETWORK_TEST` environment variable (see `botbooter_test.go`).

## DEMO

for slack and discord:

https://user-images.githubusercontent.com/197033/229368894-19b366d3-ca6d-41d2-9ab7-ca8e1a53b31a.mov

## Why

Alternatives:

### [Joe-bot](https://joe-bot.net/?utm_campaign=awesomego&utm_medium=referral&utm_source=awesomego)

- no support for Discord
- no generic access for attachments in messages

### [GoSarah](https://github.com/oklahomer/go-sarah)

- no support for Discord
- no generic access for attachments in messages

## Roadmap

- [x] Slack, Discord, Telegram and CLI adapters
- [x] Middleware and attachment abstraction
- [ ] Microsoft Teams, WhatsApp adapters
- [ ] Richer message types (blocks, embeds)

## Contributing

Issues and PRs are welcome. Please run `make all` (format, vet, lint, race tests) before opening a PR.

## License

[MIT](LICENSE) © Lucas Abreu Oliveira
