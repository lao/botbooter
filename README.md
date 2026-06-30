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

### Conversational flows

A `Flow` runs a multi-step, context-aware dialog — ask a question, wait for that
user's reply, optionally validate it, advance to the next — without hand-writing a
per-user state machine. Register it like a command; a matching message starts it,
and each later message from that conversation is routed to the active flow.

```go
signup := botbooter.NewFlow("signup"). // id must be stable (load-bearing for persistence)
	Ask("name", "What's your name?").
	Ask("email", "What's your email?", botbooter.Validate(validEmail)).
	Ask("password", "Choose a password.", botbooter.Secret()).
	OnComplete(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message, a botbooter.Answers) {
		createUser(a.Get("name"), a.Get("email"), a.Get("password"))
		_ = b.SendMessageContext(ctx, m.ChannelID, "You're all set 🎉")
	})

if err := bot.HandleFlow("^sign ?up$", signup); err != nil {
	log.Fatal(err)
}
```

`HandleFlow` validates the flow and returns an `errors.Is`-checkable sentinel
(`ErrFlowEmptyID`, `ErrFlowNoSteps`, `ErrFlowEmptyStepKey`, `ErrFlowDuplicateKey`,
`ErrFlowNoOnComplete`, `ErrFlowAlreadyRegistered`) — plus the pattern error from a
bad regexp, and a nil-flow error.
`Validate(fn)` re-prompts the same step on a non-nil error (using `err.Error()` as
the nudge); an empty/whitespace reply is a non-answer and also re-prompts.

Things to know (v1):

- **DM-intended.** While a flow is active it shadows the command table, so *every*
  message in that conversation becomes an answer until it completes, the user types
  the cancel word (default `"cancel"`, set with `CancelWord`, disable with `""`), or
  it times out (per-step idle TTL, default 10m via `Timeout`, slides each step). In a
  public channel this means a flow consumes everyone's messages — run flows in DMs.
- **`Secret()`** keeps an answer out of framework logs and any future serialized
  `Store` state. It is **not** encryption, does not police your own middleware, and
  does not hide the answer from other members of a public channel.
- **In-memory only.** In-flight flows live in memory and are lost on restart;
  v1 is single-instance. (A pluggable `Store` and multi-instance are on the roadmap.)
- **Best-effort ordering.** State is never corrupted under concurrent delivery, but a
  user who sends faster than prompts arrive may have an answer matched to a later step.
- **Restart needs cancel.** Re-triggering a flow while it is active is consumed as an
  answer; cancel first to start over.

### Attachments

```go
attachments, err := b.GetAttachments(m)
for _, a := range attachments {
	fmt.Println(a.URL, a.IsImage) // a.ExtraData holds the raw platform payload
}
```

`Attachment.URL` is empty on platforms that deliver media by id (Telegram, WhatsApp). Call `b.ResolveAttachmentURL(ctx, a)` for a downloadable link on any platform — Discord/CLI return `a.URL` as-is, while Slack/Telegram/WhatsApp resolve one on demand. The Telegram link embeds the bot token (a one-line warning logs on each resolve, suppressible via `BOTBOOTER_TELEGRAM_SUPPRESS_URL_WARNING`); see [docs/platforms.md](docs/platforms.md#telegram).

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
- [x] Conversational flows — multi-step forms via `HandleFlow` (linear, in-memory, single-instance)
- [ ] Pluggable `Store` module (persistent key-value brain), composed via `botbooter.New(adapter, opts...)` — in-memory default, optional Redis backend

### Conversational flows — deferred work

v1 flows are **linear, plain-text, in-memory and single-instance** (see the caveats
under [Conversational flows](#conversational-flows)). The following each reuse the v1
engine with no state migration:

1. **Branching.** Add via `Next func(Answers) string` (step id → next step id) or
   `AskIf(cond, …)`. The state already stores `Answers`, so branching needs no migration.
2. **DM-only / mention-gated flows.** Requires a cross-platform `Message.IsDirect` (and/or
   "addressed to bot") signal that `Message` lacks today. Until then, channel scoping is
   documented (§4), not enforced.
3. **Command allowlist during a flow.** An opt-in set of global commands (e.g. `help`) that
   pierce an active flow, instead of cancel/expiry being the only exits.
4. **`Store`-backed persistence** (§5) and **§2 blocking `Ask`** (with the non-blocking
   delivery + atomic-clear requirements already specified in §2).
5. **Multi-instance / horizontal scaling.** Gated on the shared `Store` (item 4) **plus** a
   concurrency redesign — the §4 in-process striped locks must be replaced by store-level
   atomicity, because the validator is user Go code and cannot run inside a Redis
   transaction. Plan: **optimistic compare-and-swap** on the `Version` field — read
   `(state, version)` → run validator in Go → write only if `version` is unchanged; on
   conflict the other replica already advanced, so drop (or re-read) the message. One CAS
   wins → no double-advance. Residual issues to handle then: (a) the prompt send happens
   outside the CAS, so two replicas can both validate-and-send before either writes →
   **duplicate prompt**; make sends idempotent (key by inbound message id) or accept the
   rare dup. (b) Platform delivery constraints remain even with a shared store —
   **Discord** needs gateway **sharding** (one replica owns a conversation) to stop
   duplicate event delivery; **Telegram** must switch from getUpdates to **webhook** mode
   (getUpdates is single-consumer). Until all of this lands, v1 is single-instance (§4, §5).
6. **Normalized validators.** `Validate(func(string) (string, error))` storing the parsed
   value, so `OnComplete` does not re-parse (e.g. `age` to int). v1 keeps the simpler
   `func(string) error` plus `Answers.Lookup`.
7. **Replace-active-flow.** v1 decision: a `HandleFlow` trigger received *during* an active
   flow is consumed as input (the command table is shadowed); to restart, the user cancels
   first. Documented. A future reserved "restart" word or replace-on-retrigger can layer on.


## Contributing

Issues and PRs are welcome. Please run `make all` (format, vet, lint, race tests) before opening a PR.

## License

[MIT](LICENSE) © Lucas Abreu Oliveira
