# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

botbooter is a Go library (module `github.com/lao/botbooter`, Go 1.23+) for writing a chat bot once and running it on Slack (Socket Mode), Discord (Gateway), Telegram (Bot API), WhatsApp (Cloud API webhook), Microsoft Teams (Azure Bot Framework webhook) or a local CLI. Consumers register regex-matched `Command` handlers plus optional `Middleware` and call `Run(ctx)`; the platform is hidden behind a single `Bot` type.

## Commands

```bash
make all        # fmt + vet + lint + test-race — run before considering work done
make test       # go test ./...
make test-race  # race detector (CI runs this; the lifecycle code is concurrency-heavy)
make cover      # race + atomic coverage, prints total
make lint       # golangci-lint v2 (needs golangci-lint installed)
make run-cli    # run _examples/basic in CLI mode, no credentials
```

Run a single test: `go test -race -run TestBot_Connect/AlreadyConnected ./...` (subtests use `t.Run`, address them with `Parent/Child`).

The suite is hermetic by default. `TestConnectSlack_StartsAndStops` does real Slack network I/O and is skipped unless `BOTBOOTER_SLACK_NETWORK_TEST` is set.

## Architecture

The codebase is a **facade over internal packages**. Understanding the split is the key to navigating it.

- **`botbooter.go`** (package `botbooter`) — the **SDK-free shared-types** package: every exported type is a **type alias** (`Bot = core.Bot`, `Message = core.Message`, …) re-exported from `internal/core`, plus the `BotType` consts and the two error sentinels. It imports only `internal/core` and **no platform SDK**. Because aliases are identities, an `internal/core.Bot` *is* a public `botbooter.Bot` — no conversion. Construction lives in the per-platform packages (below), not here.

- **`internal/core`** — the platform-agnostic engine: the `Bot` struct, command/middleware dispatch, and the connect/run/disconnect lifecycle. It depends on no platform's connection logic. The seam is the **`Adapter` interface** (`Connect`, `Disconnect`, `Send`, `Attachments`). The Bot drives an adapter and hands it an **`AdapterDeps`** struct of callbacks (`Dispatch`, `Done`, `Disconnect`) so adapters in other packages can drive the Bot's unexported internals without those internals leaking.

- **`internal/{cli,slack,discord,telegram,whatsapp,teams}`** — one `core.Adapter` implementation each. Each exposes `New(...)` returning a `*core.Bot` built via `core.New(botType, adapter)`, plus package-level accessors (`slack.Client`, `discord.Session`, …) that recover the concrete adapter from a `*Bot` via `core.AdapterAs[T]` and hand back the raw client. WhatsApp and Teams are webhook adapters: they run their own HTTP server (`Connect` binds a listener; `Disconnect` shuts it down + drains in-flight dispatch) and reply over REST. Teams authenticates every inbound request against the Bot Connector JWKS (its only third-party dep, `golang-jwt/jwt/v5`, used solely for signature/claim verification) and routes replies via an adapter-side `conversationID→serviceUrl` map populated on inbound Activities.

- **`{cli,slack,discord,telegram,whatsapp,teams}`** (public per-platform packages) — a thin wrapper over each `internal/<platform>` adapter, importing only that platform's SDK (CLI and WhatsApp need none — WhatsApp speaks the Cloud API over plain HTTP; Teams speaks the Bot Framework REST API over plain HTTP and pulls only `golang-jwt/jwt/v5`, a crypto lib, not a platform SDK). Each exposes the typed constructor (`slack.New`, `discord.New`, `telegram.New`, `whatsapp.New`, `teams.New`, `cli.New`, returning `*botbooter.Bot`) and that platform's raw/client accessors (`slack.Client(bot)`, `discord.Session(bot)`, `telegram.RawUpdate`, `teams.RawMessage`, …). Attachment resolution is platform-agnostic — `bot.ResolveAttachmentURL(ctx, att)` is a method on `botbooter.Bot` that dispatches to whichever adapter implements `core.AttachmentResolver`. A consumer imports `botbooter` plus the one `botbooter/<platform>` it deploys, so unused platform SDKs never enter its build graph. Per-package `imports_test.go` guards (direct imports) and the root `isolation_deps_test.go` (transitive closure via `go list -deps`) lock this in.

**To add a platform:** add a `core.BotType` iota const + `String()` case in `internal/core`; create `internal/<platform>` implementing `core.Adapter`; add a public `<platform>/` package wrapping it with a typed `New` + accessors, **plus its `imports_test.go` SDK-ban guard and a present+absent entry in the root `isolation_deps_test.go`**; and re-export the new `BotType` const in `botbooter.go`. The lifecycle/dispatch in `core` need no changes.

**Each adapter owns its own setup.** By design, `core` is deliberately thin — connect/run/disconnect and dispatch — and every adapter carries its *own* connection lifecycle rather than sharing a webhook/server helper. So the WhatsApp and Teams adapters repeat similar scaffolding (their own HTTP server, in-flight drain, server timeouts, `Addr`/`Path` normalization) on purpose: each adapter keeps full, independent control over its process and comms, and can diverge freely (auth, retry, framing) without a shared abstraction coupling them. **Prefer this duplication over hoisting a common `internal/webhook` layer** — the copies are cheap and the independence is the point. Only extract into `core` what is genuinely platform-agnostic and already shared (e.g. dispatch, attachment-URL resolution).

The tradeoff of that duplication: **a bug or fix in one adapter's copied scaffolding probably applies to the siblings too — sweep them when you touch it.** (The shutdown-drain context handling in the webhook adapters is a case in point.) Duplication buys independence, not license to let the copies silently drift on shared correctness concerns.

### Dispatch (`core.Bot.dispatch`)

Commands are regex patterns compiled once in `AddHandler` (invalid patterns return an error there). Matching is **first-match-wins**; no match falls through to the unknown-command handler if set. Middleware is composed inner-to-outer so registration order = execution order, each calling `next`. The whole dispatch is wrapped in a `recover` — a panicking handler is logged, not fatal.

### Reactions (`core.Bot.dispatchReaction`)

Emoji-reaction handling is a **second, optional ingress path** alongside messages, built on the same optional-capability seam as `AttachmentResolver` — the mandatory `Adapter` interface is unchanged.

- Ingress: adapters that see reaction events map them into a `core.Reaction` and call `deps.DispatchReaction`; consumers register `bot.OnReaction(handler)`. Unlike commands, reaction handlers are **not** regex-matched (branch on `Reaction.Emoji` in code), run **all** registered handlers with **per-handler** `recover`, and **bypass the `Middleware` chain**.
- Egress: `bot.ReplyToMessage(ctx, channelID, replyToID, text)` posts a threaded reply. Adapters implement the optional `core.ThreadedSender` (`SendThreaded`) to thread it (Slack `thread_ts`, Discord message reference, Telegram `reply_to_message_id`, WhatsApp `context.message_id`); adapters without it fall back to a plain unthreaded `Send`.
- Scope is **added-only** (removed reactions are dropped). `Reaction.Emoji` is platform-shaped: a shortname (`thumbsup`) on Slack, a unicode char elsewhere — best-effort, not normalized. There is **no self-reaction filter** anywhere: v1 has no reaction egress, so a self-reply loop is impossible; revisit this if a reaction-add egress is ever added. Per platform: Slack skips file/file-comment reactions (no reply target); Telegram diffs `NewReaction`/`OldReaction` for genuine adds and needs `message_reaction` in `allowed_updates` (plus bot-admin in groups); WhatsApp treats an empty-emoji reaction as a removal. Read `Reaction.Raw` with the matching typed accessor (`slack.RawReaction`, `discord.RawReaction`, `telegram.RawReactionUpdate`, `whatsapp.RawReaction`). **A Slack consumer must also subscribe the app to the `reaction_added` Events API event and grant `reactions:read`, or `OnReaction` never fires — a config requirement, not a code bug.**

### Lifecycle (the subtle part — read `internal/core/core.go` before touching it)

- `Connect` is **non-blocking**: adapters start their event loop in a goroutine and report termination via `deps.Done(err)`. `Run` blocks, selecting on `ctx.Done()` vs the done channel, then disconnects.
- Each `Connect` allocates a fresh `connection` struct holding that connection's `cancel`/`done`/`runDone` and its **own `sync.Once`** (`teardown`). A reconnect installs a new struct instead of resetting shared state, so a lingering disconnect goroutine from a prior connection can't race the new one — and a superseded connection's `teardown` skips the shared adapter's `Disconnect` entirely (`disconnectConn` scopes teardown to the still-installed connection). Don't collapse this back to shared, bot-scoped teardown state.
- `Connect` holds `b.mu` across the (non-blocking) `adapter.Connect` and installs `b.conn` only on success, so a Disconnect racing an in-flight Connect serializes behind the lock rather than double-disconnecting a half-connected adapter.
- `Run` **swallows the context's own cancellation error**: a clean Ctrl-C surfaces as `nil`, not `context.Canceled`, because callers commonly do `log.Fatal(bot.Run(ctx))` and shouldn't exit non-zero on graceful shutdown.
- Adapters differ in teardown: Slack/CLI loops stop purely from context cancellation (their `Disconnect` is a no-op); Discord must `Close()` the session and remove its handler, and it watches `ctx.Done()` to call `deps.Disconnect()`.
- Every platform drops the bot's own and other bots' messages to avoid reply loops (`isBotMessage` for Slack, the `Author.Bot`/self-ID checks for Discord).

### CLI adapter caveat

The CLI has no real upload channel, so `parseMessage` treats any whitespace-separated token that resolves to an **existing local file** as an attachment (content-sniffed for `IsImage`). It is for trusted local input only — never wire it to untrusted data.

## Conventions

- Tests use the in-repo **`internal/asserts`** helpers (`asserts.Equal`, `NoError`, `ErrorIs`, …), not testify — testify is only an indirect dependency. Match that style in new tests.
- Errors are sentinel values (`ErrUnknownBotType`, `ErrAlreadyConnected`) re-exported through the root package; check with `errors.Is`.
- Pre-1.0: the public API may still change, but keep `botbooter.go` SDK-free (shared-type aliases, `BotType` consts and error sentinels only) and the per-platform `{cli,slack,discord,telegram,whatsapp,teams}` packages thin — put real logic in `internal/`.
