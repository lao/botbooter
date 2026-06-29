# botbooter

[![Go Reference](https://pkg.go.dev/badge/github.com/lao/botbooter.svg)](https://pkg.go.dev/github.com/lao/botbooter)
[![CI](https://github.com/lao/botbooter/actions/workflows/ci.yml/badge.svg)](https://github.com/lao/botbooter/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/lao/botbooter)](https://goreportcard.com/report/github.com/lao/botbooter)
[![Go Version](https://img.shields.io/github/go-mod/go-version/lao/botbooter)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> A small, framework-style toolkit for writing chat bots **once** and running them on **Slack, Discord, Telegram, WhatsApp, or a local CLI** — with the same handlers, middleware, and attachment access on every platform.

Inspired by [Gin](https://gin-gonic.com/): you register pattern-matched command handlers and optional middleware, then run the bot. botbooter abstracts the platform behind a single `Bot` type so your business logic does not care whether a message came from Slack, Discord, Telegram, WhatsApp, or stdin.

> ⚠️ **Not production ready.** botbooter is pre-1.0 and under active development. The
> public API may change without notice, and it has not been hardened or battle-tested for
> production workloads. Use it for experiments and side projects; pin a specific version and
> review changes before depending on it for anything critical.

## Features

- **One API, multiple platforms** — Slack (Socket Mode), Discord (Gateway), Telegram (long polling), WhatsApp (Cloud API webhook), and a built-in **CLI adapter** for local development and testing with no credentials.
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
	"github.com/lao/botbooter/cli"
)

func main() {
	bot := cli.New(os.Stdin, os.Stdout)

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
go run ./examples/v1 whatsapp   # uses WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (+ optional WA_PATH)
```

## Concepts

### Constructing a bot

Import `botbooter` for the shared types plus the one `botbooter/<platform>` package you deploy — each constructor lives in its platform package, so a bot that uses one platform never compiles the others' SDKs into its binary:

| Constructor | Signature | Notes |
|---|---|---|
| `cli.New(in io.Reader, out io.Writer)` | `*Bot` | Local adapter; `nil` defaults to stdin/stdout. |
| `slack.New(appToken, botToken string)` | `*Bot` | Socket Mode (`xapp-…` + `xoxb-…`). |
| `discord.New(token string)` | `(*Bot, error)` | Enables the message-content intent (see below). |
| `telegram.New(token string)` | `(*Bot, error)` | Long polling via `getUpdates`; BotFather token. |
| `whatsapp.New(cfg whatsapp.Config)` | `(*Bot, error)` | Meta Cloud API; runs an inbound webhook HTTP server. |

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

### Message fields

Every `Message` carries normalized, platform-agnostic fields so handlers rarely
need the raw event. `UserID`, `ChannelID` and `Content` are always set; the rest
are best-effort and stay at their zero value when a platform cannot supply them:

| Field | Meaning |
|---|---|
| `ID` | Platform message id (`""` for CLI). |
| `AuthorName` | Display/username (empty on Slack, which delivers only an id). |
| `Timestamp` | Message time as a `time.Time` (zero on CLI). |
| `ReplyToID` | Id of the replied-to/thread message (`""` when not a reply). |
| `MentionedUserIDs` | Mentioned user ids; Telegram contributes only `text_mention` ids. |

### Raw platform access

When you need something the normalized fields don't carry, reach the originating
event or the underlying SDK client through typed accessors on each platform
package — `botbooter` and `internal/core` stay free of every platform SDK, so
these live on `botbooter/<platform>`:

```go
if ev, ok := slack.RawEvent(m); ok {
	_ = ev.ThreadTimeStamp // anything on the raw *slackevents.MessageEvent
}

// Raw event per platform: discord.RawEvent, slack.RawEvent, telegram.RawUpdate, whatsapp.RawMessage, cli.RawData.
// Underlying client per platform (WhatsApp has none — it speaks the Cloud API over plain HTTP):
client := slack.Client(bot)        // *slack.Client (nil if not a Slack bot)
session := discord.Session(bot)    // *discordgo.Session
tg := telegram.Client(bot)         // *bot.Bot
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
| WhatsApp | Cloud API token + phone-number id + app secret + verify token + bind addr | [docs/platforms.md](docs/platforms.md#whatsapp) |
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

- [x] Slack, Discord, Telegram, WhatsApp and CLI adapters
- [x] Middleware and attachment abstraction
- [ ] Microsoft Teams adapter
- [ ] Richer message types (blocks, embeds)
- [ ] Unify attachment url retriavel for all implementations

## Contributing

Issues and PRs are welcome. Please run `make all` (format, vet, lint, race tests) before opening a PR.

## License

[MIT](LICENSE) © Lucas Abreu Oliveira
