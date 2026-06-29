# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

botbooter is a Go library (module `github.com/lao/botbooter`, Go 1.23+) for writing a chat bot once and running it on Slack (Socket Mode), Discord (Gateway), Telegram (Bot API) or a local CLI. Consumers register regex-matched `Command` handlers plus optional `Middleware` and call `Run(ctx)`; the platform is hidden behind a single `Bot` type.

## Commands

```bash
make all        # fmt + vet + lint + test — run before considering work done
make test       # go test ./...
make test-race  # race detector (CI runs this; the lifecycle code is concurrency-heavy)
make cover      # race + atomic coverage, prints total
make lint       # golangci-lint v2 (needs golangci-lint installed)
make run-cli    # run examples/v1 in CLI mode, no credentials
```

Run a single test: `go test -race -run TestBot_Connect/AlreadyConnected ./...` (subtests use `t.Run`, address them with `Parent/Child`).

The suite is hermetic by default. `TestConnectSlack_StartsAndStops` does real Slack network I/O and is skipped unless `BOTBOOTER_SLACK_NETWORK_TEST` is set.

## Architecture

The codebase is a **facade over internal packages**. Understanding the split is the key to navigating it.

- **`botbooter.go`** (package `botbooter`) — the **SDK-free shared-types** package: every exported type is a **type alias** (`Bot = core.Bot`, `Message = core.Message`, …) re-exported from `internal/core`, plus the `BotType` consts and the two error sentinels. It imports only `internal/core` and **no platform SDK**. Because aliases are identities, an `internal/core.Bot` *is* a public `botbooter.Bot` — no conversion. Construction lives in the per-platform packages (below), not here.

- **`internal/core`** — the platform-agnostic engine: the `Bot` struct, command/middleware dispatch, and the connect/run/disconnect lifecycle. It depends on no platform's connection logic. The seam is the **`Adapter` interface** (`Connect`, `Disconnect`, `Send`, `Attachments`). The Bot drives an adapter and hands it an **`AdapterDeps`** struct of callbacks (`Dispatch`, `Done`, `Disconnect`) so adapters in other packages can drive the Bot's unexported internals without those internals leaking.

- **`internal/{cli,slack,discord,telegram}`** — one `core.Adapter` implementation each. Each exposes `New(...)` returning a `*core.Bot` built via `core.New(botType, adapter)`, plus package-level accessors (`slack.Client`, `discord.Session`, …) that recover the concrete adapter from a `*Bot` via `core.AdapterAs[T]` and hand back the raw client.

- **`{cli,slack,discord,telegram}`** (public per-platform packages) — a thin wrapper over each `internal/<platform>` adapter, importing only that platform's SDK. Each exposes the typed constructor (`slack.New`, `discord.New`, `telegram.New`, `cli.New`, returning `*botbooter.Bot`) and that platform's raw/client accessors (`slack.Client(bot)`, `discord.Session(bot)`, `telegram.RawUpdate`, `telegram.ResolveAttachmentURL`, …). A consumer imports `botbooter` plus the one `botbooter/<platform>` it deploys, so unused platform SDKs never enter its build graph. Per-package `imports_test.go` guards (direct imports) and the root `isolation_deps_test.go` (transitive closure via `go list -deps`) lock this in.

**To add a platform:** add a `core.BotType` iota const + `String()` case in `internal/core`; create `internal/<platform>` implementing `core.Adapter`; add a public `<platform>/` package wrapping it with a typed `New` + accessors, **plus its `imports_test.go` SDK-ban guard and a present+absent entry in the root `isolation_deps_test.go`**; and re-export the new `BotType` const in `botbooter.go`. The lifecycle/dispatch in `core` need no changes.

### Dispatch (`core.dispatch`)

Commands are regex patterns compiled once in `AddHandler` (invalid patterns return an error there). Matching is **first-match-wins**; no match falls through to the unknown-command handler if set. Middleware is composed inner-to-outer so registration order = execution order, each calling `next`. The whole dispatch is wrapped in a `recover` — a panicking handler is logged, not fatal.

### Lifecycle (the subtle part — read `core.go` before touching it)

- `Connect` is **non-blocking**: adapters start their event loop in a goroutine and report termination via `deps.Done(err)`. `Run` blocks, selecting on `ctx.Done()` vs the done channel, then disconnects.
- Each connection installs a fresh `stop` closure guarded by its **own `sync.Once`**. This is deliberate: a reconnect installs a new closure instead of resetting a shared `Once`, so a lingering disconnect goroutine from a prior connection can't race the new one. Don't collapse this back to a single shared `Once`.
- `Run` **swallows the context's own cancellation error**: a clean Ctrl-C surfaces as `nil`, not `context.Canceled`, because callers commonly do `log.Fatal(bot.Run(ctx))` and shouldn't exit non-zero on graceful shutdown.
- Adapters differ in teardown: Slack/CLI loops stop purely from context cancellation (their `Disconnect` is a no-op); Discord must `Close()` the session and remove its handler, and it watches `ctx.Done()` to call `deps.Disconnect()`.
- Every platform drops the bot's own and other bots' messages to avoid reply loops (`isBotMessage` for Slack, the `Author.Bot`/self-ID checks for Discord).

### CLI adapter caveat

The CLI has no real upload channel, so `parseMessage` treats any whitespace-separated token that resolves to an **existing local file** as an attachment (content-sniffed for `IsImage`). It is for trusted local input only — never wire it to untrusted data.

## Conventions

- Tests use the in-repo **`internal/asserts`** helpers (`asserts.Equal`, `NoError`, `ErrorIs`, …), not testify — testify is only an indirect dependency. Match that style in new tests.
- Errors are sentinel values (`ErrUnknownBotType`, `ErrAlreadyConnected`) re-exported through the facade; check with `errors.Is`.
- Pre-1.0: the public API may still change, but keep `botbooter.go` a pure facade — put real logic in `internal/`.
