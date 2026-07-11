# botbooter

[![Go Reference](https://pkg.go.dev/badge/github.com/lao/botbooter.svg)](https://pkg.go.dev/github.com/lao/botbooter)
[![CI](https://github.com/lao/botbooter/actions/workflows/ci.yml/badge.svg)](https://github.com/lao/botbooter/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/lao/botbooter)](go.mod)
[![Test Coverage](https://codecov.io/gh/lao/botbooter/branch/main/graph/badge.svg)](https://codecov.io/gh/lao/botbooter)
[![Releases](https://img.shields.io/github/v/release/lao/botbooter.svg?include_prereleases&color=blue)](https://github.com/lao/botbooter/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> A small, framework-style toolkit for writing chat bots **once** and running them on **Slack, Discord, Telegram, WhatsApp, Microsoft Teams, or a local CLI** — with the same handlers, middleware, and attachment access on every platform.

Inspired by [Gin](https://gin-gonic.com/): you register pattern-matched command handlers and optional middleware, then run the bot. botbooter abstracts the platform behind a single `Bot` type so your business logic does not care whether a message came from Slack, Discord, Telegram, WhatsApp, Microsoft Teams, or stdin.

> ⚠️ **Not production ready.** botbooter is pre-1.0 and under active development. The
> public API may change without notice, and it has not been hardened or battle-tested for
> production workloads. Use it for experiments and side projects; pin a specific version and
> review changes before depending on it for anything critical.

## Features

- **One API, multiple platforms** — Slack (Socket Mode), Discord (Gateway), Telegram (long polling), WhatsApp (two flavors: Cloud API webhook, or WhatsApp Web via whatsmeow — QR-linked, no Meta account), Microsoft Teams (Azure Bot Framework webhook), and a built-in **CLI adapter** for local development and testing with no credentials.
- **Regex command routing** — patterns are compiled once and matched against message content; first match wins.
- **Middleware chain** — wrap every message (logging, auth, metrics, …) with `next`-style composition.
- **Platform-agnostic attachments** — read image/file attachments uniformly across platforms.
- **Context-first & graceful shutdown** — handlers receive a `context.Context`; `Run(ctx)` / `Start()` connect and shut down cleanly on cancellation or `SIGINT`/`SIGTERM`.
- **Resilient dispatch** — a panicking handler is recovered and logged instead of taking down the bot.

## Install

```bash
go get github.com/lao/botbooter
```

Requires Go 1.25+.

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

	bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, strings.TrimPrefix(m.Content, "echo "))
	})

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	_ = bot.Run(ctx) // type "echo hi", press enter; Ctrl-D to quit
}
```

Or run the bundled example directly:

```bash
go run ./_examples/basic            # CLI mode (default, no credentials)
go run ./_examples/basic slack      # uses SLACK_APP_TOKEN / SLACK_BOT_TOKEN
go run ./_examples/basic discord    # uses DISCORD_BOT_TOKEN
go run ./_examples/basic telegram   # uses TELEGRAM_BOT_TOKEN
go run ./_examples/basic whatsapp   # WhatsApp Cloud API flavor: uses WA_TOKEN / WA_PHONE_ID / WA_APP_SECRET / WA_VERIFY_TOKEN / WA_ADDR (+ optional WA_PATH)
go run ./_examples/basic whatsmeow  # WhatsApp Web flavor: no credentials — scan the QR on first run (+ optional WA_MEOW_DB)
go run ./_examples/basic teams      # uses TEAMS_APP_ID / TEAMS_APP_PASSWORD / TEAMS_ADDR (+ optional TEAMS_APP_TENANT_ID / TEAMS_PATH)
```

## Concepts

### Constructing a bot

Import `botbooter` for the shared types plus the one `botbooter/<platform>` package you deploy — each constructor lives in its platform package, so a bot that uses one platform never compiles the others' SDKs into its binary:

| Constructor | Signature | Notes |
|---|---|---|
| `cli.New(in io.Reader, out io.Writer)` | `*Bot` | Local adapter; `nil` defaults to stdin/stdout. |
| `slack.New(cfg slack.Config)` | `(*Bot, error)` | Socket Mode (`AppToken` `xapp-…` + `BotToken` `xoxb-…`). |
| `discord.New(token string)` | `(*Bot, error)` | Enables the message-content intent (see below). |
| `telegram.New(token string)` | `(*Bot, error)` | Long polling via `getUpdates`; BotFather token. |
| `cloud.New(cfg cloud.Config)` | `(*Bot, error)` | WhatsApp, Meta Cloud API flavor (`botbooter/whatsapp/cloud`); runs an inbound webhook HTTP server. |
| `whatsmeow.New(cfg whatsmeow.Config)` | `(*Bot, error)` | WhatsApp, Web-protocol flavor (`botbooter/whatsapp/whatsmeow`); QR-links to a phone, no Meta account needed. |
| `teams.New(cfg teams.Config)` | `(*Bot, error)` | Azure Bot Framework; runs an inbound webhook HTTP server. |

WhatsApp comes in **two flavors selected by import path** — `whatsapp/cloud` (official Meta Cloud API: Business account, webhook + public HTTPS URL, no third-party deps) and `whatsapp/whatsmeow` (unofficial WhatsApp Web multidevice protocol via [whatsmeow](https://github.com/tulir/whatsmeow): pair by QR code like WhatsApp Web, session persisted in a local SQLite file — `Config.DBPath`, default `botbooter-whatsapp-meow.db` in the working directory — no Meta account or webhook). Only the flavor you import is compiled into your binary.

### Handlers, commands and middleware

```go
// A command routes messages whose content matches a regular expression.
bot.AddHandler(botbooter.Command{
	Pattern: "^ping$",
	Handler: func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
		_ = b.SendMessageContext(ctx, m.ChannelID, "pong")
	},
})

// HandleFunc is a shorthand for the common case.
bot.HandleFunc("^hello", greetHandler)

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

`AddHandler` / `HandleFunc` return nothing. If a pattern is not a valid regular expression the error is recorded and `Connect`/`Run` refuse to start, reporting every invalid pattern in one joined error.

### Replies and threads

A send is plain by default and threads only when you pass a **send option**:

- **`b.SendMessageContext(ctx, m.ChannelID, text)`** — plain message in the **channel root**. It ignores where the triggering message lives, so a reply to a message inside a thread lands back in the channel, detached.
- **`b.SendMessageContext(ctx, m.ChannelID, text, botbooter.InReplyTo(m))`** — posts into the **thread or reply-chain of `m`**. Each adapter derives its own correct anchor from `m` — you don't compute one.
- **`b.Reply(ctx, m, text)`** — convenience sugar for exactly the `InReplyTo(m)` call above. Prefer it in handlers.

```go
bot.HandleFunc("^echo ", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
	// Threads the answer onto the triggering message instead of the channel root.
	_ = b.Reply(ctx, m, "You said: "+strings.TrimPrefix(m.Content, "echo "))
})
```

"Thread" means something different on each platform, so `InReplyTo(m)` hands the whole `Message` to the adapter and it picks the anchor. Two scenarios spell out the behavior:

- **A comment already inside a thread** → the reply continues **that same thread**. On Slack the reply is posted with `thread_ts = m.ReplyToID` (the thread root the inbound message carried); on Discord/Telegram/WhatsApp it quotes / references `m.ID`.
- **A top-level comment in a channel** → the reply is a **direct reply to that comment**, anchored on it, *not* forced into the channel root. On Slack a top-level message has no thread root, so it gets a plain top-level reply (Slack does **not** open a brand-new thread off it — that would only bury the reply under an empty thread); on Discord it becomes an inline reply referencing the message, on Telegram a `reply_to_message_id`, and on WhatsApp (Cloud API) a quoted reply. The WhatsApp Web (whatsmeow) flavor currently ignores the threading anchor and sends a plain message.

For a raw, platform-specific anchor there's **`botbooter.WithThreadID(id)`** — the adapter uses the string verbatim (a Slack `thread_ts`, or a reply/quote message id elsewhere) and it wins over `InReplyTo`. You own platform-correctness with it. Per-platform anchor semantics, the precedence and fallback rules, and how `ReplyToID` vs `ID` are chosen are documented in [_docs/platforms.md](_docs/platforms.md#threaded-replies).

Fallback is automatic and safe: **Teams** and **CLI** ignore the options (every send is plain), and an anchor that resolves to nothing degrades to a plain send — a send never fails just because a message can't be threaded. `Reply` returns an error only when the bot has no adapter or `m` is `nil`.

### Attachments

```go
attachments, err := b.GetAttachments(m)
for _, a := range attachments {
	fmt.Println(a.URL, a.IsImage) // a.ExtraData holds the raw platform payload
}
```

`Attachment.URL` is empty on platforms that deliver media by id (Telegram, WhatsApp Cloud API). Call `b.ResolveAttachmentURL(ctx, a)` for a downloadable link on any platform — Discord/Teams/CLI return `a.URL` as-is, while Slack/Telegram/WhatsApp Cloud API resolve one on demand. The Telegram link embeds the bot token (a one-line warning logs on each resolve, suppressible via `BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING`); see [_docs/platforms.md](_docs/platforms.md#telegram).

WhatsApp Web (whatsmeow) media is end-to-end encrypted and has no URL at all — fetch and decrypt the bytes with `whatsmeow.Download(ctx, bot, a)`.

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

// Raw event per platform: discord.RawEvent, slack.RawEvent, telegram.RawUpdate, cloud.RawMessage (WhatsApp Cloud API), whatsmeow.RawMessage (WhatsApp Web), teams.RawMessage, cli.RawData.
// Underlying client per platform (WhatsApp Cloud API and Teams have none — they speak REST over plain HTTP):
client := slack.Client(bot)        // *slack.Client (nil if not a Slack bot)
sock := slack.SocketClient(bot)    // *socketmode.Client (nil if not a Slack bot)
session := discord.Session(bot)    // *discordgo.Session
tg := telegram.Client(bot)         // *bot.Bot
wa := whatsmeow.Client(bot)        // *whatsmeow.Client (WhatsApp Web flavor)
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
[troubleshooting](_docs/platforms.md#no-response), and the official
documentation for each live in **[_docs/platforms.md](_docs/platforms.md)**.

| Platform | What you need | Setup |
|---|---|---|
| Slack | `xapp-…` app-level token + `xoxb-…` bot token | [_docs/platforms.md](_docs/platforms.md#slack) |
| Discord | bot token + Message Content Intent | [_docs/platforms.md](_docs/platforms.md#discord) |
| Telegram | BotFather bot token | [_docs/platforms.md](_docs/platforms.md#telegram) |
| WhatsApp (Cloud API) | Cloud API token + phone-number id + app secret + verify token + bind addr | [_docs/platforms.md](_docs/platforms.md#whatsapp) |
| WhatsApp (Web / whatsmeow) | nothing — scan a QR code from WhatsApp > Linked devices on first run | [_docs/platforms.md](_docs/platforms.md#whatsapp-web-whatsmeow) |
| Microsoft Teams | Azure Bot app id + password (+ optional tenant id) + bind addr | [_docs/platforms.md](_docs/platforms.md#microsoft-teams) |
| CLI | nothing (local stdin/stdout) | [_docs/platforms.md](_docs/platforms.md#cli) |

## Development

```bash
make all        # fmt + vet (both incl. _examples) + lint + test-race
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

- [x] Slack, Discord, Telegram, WhatsApp, Microsoft Teams and CLI adapters
- [x] Middleware and attachment abstraction
- [x] Unify attachment URL retrieval for all implementations
- [ ] WeChat, Mastodon adapters
- [ ] Richer message types (blocks, embeds)
- [ ] Pluggable `Store` module (persistent key-value brain), composed via `botbooter.New(adapter, opts...)` — in-memory default, optional Redis backend
- [ ] **Conversational flows** (multi-step, context-aware dialogs) — a handler can pause and own the *next* message from the same user instead of re-routing it, so a sign-up form (name → email → address → profession → age → password) asks one question, waits for the reply, validates, then advances to the next. Per-user state lives in the pluggable `Store` above; the goal is a declarative, minimal-boilerplate API for defining the steps.

## Contributing

Issues and PRs are welcome. Please run `make all` (format, vet, lint, race tests) before opening a PR.

## License

[MIT](LICENSE) © Lucas Abreu Oliveira
