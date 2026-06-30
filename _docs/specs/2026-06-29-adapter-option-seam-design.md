# Adapter Option Seam — Design Spec

- **Date:** 2026-06-29
- **Branch:** `refactor/per-platform-packages` (builds directly on PR #13; not merging #13 first)
- **Status:** Approved design, pre-implementation

## Goal

Introduce an extensible constructor seam — `botbooter.New(adapter, opts ...Option)` — so that a future module (first planned: a key-value `Store`) composes into a `Bot` without breaking any call site. The constructor shape is a strict superset of today's, so future `WithX(...)` options are purely additive.

**Ship only the seam.** No `Store`, no module implementations, no `Option` constructors in this work. The variadic `...Option` tail exists, but is always empty until a real module lands.

### Non-goals

- The `Store` itself — tracked in the README Roadmap.
- Cron / scheduled jobs, observability, or any second module kind.
- Renumbering `BotType` constants (see Open Items).

## Background — current state (post-PR #13)

- `core.New(botType core.BotType, adapter core.Adapter) *Bot` already takes the adapter as an argument. Adapter-as-argument is therefore already the internal seam; only the *public* surface wraps it per platform.
- Public surface: per-platform constructors `cli.New`, `slack.New`, `discord.New`, `telegram.New`, `whatsapp.New`, each returning `*botbooter.Bot` (the three fallible platforms also return `error`).
- The root `botbooter` package is SDK-free; each platform package owns its own SDK. Isolation is enforced by per-package `imports_test.go` and the root `isolation_deps_test.go` (`go list -deps`).
- `BotType` is a separate `int` enum passed redundantly to `core.New`. Adapters do not self-report their type. Core reads `BotType` only in `String()`; no lifecycle or dispatch logic branches on it.

## Design

### 1. Public seam (additive, root stays SDK-free)

```go
// package botbooter — imports only internal/core, no platform SDK
type Adapter = core.Adapter
type Option  = core.Option

// New builds a Bot from a required adapter plus optional modules.
func New(a Adapter, opts ...Option) *Bot { return core.New(a, opts...) }
```

- The adapter is **required by the signature** — fail-fast, and no "zero or two adapters" ambiguity that a uniform `...Module` variadic would allow.
- `opts` is the future-module tail. We define the `Option` type now and ship **zero** `Option` constructors. A later `WithStore(...)` drops in with no call-site churn.

### 2. Per-platform `Adapter()` seams; keep `New()` as shims

Each public platform package adds an `Adapter` constructor that returns the interface value, and reimplements its existing `New` as a thin shim over `botbooter.New`. Existing `New` signatures are **preserved**, so no consumer breaks.

The error-return asymmetry is intrinsic to the adapters (Slack/CLI cannot fail at construction; Discord/Telegram/WhatsApp can), so it rides along on `Adapter()` exactly as it does on `New()` today:

```go
// slack (cannot fail) — composes inline
func Adapter(appToken, botToken string) botbooter.Adapter { return slackint.Adapter(appToken, botToken) }
func New(appToken, botToken string) *botbooter.Bot        { return botbooter.New(Adapter(appToken, botToken)) }

// discord (can fail) — two-step
func Adapter(token string) (botbooter.Adapter, error) { return discordint.Adapter(token) }
func New(token string) (*botbooter.Bot, error) {
    a, err := Adapter(token)
    if err != nil {
        return nil, err
    }
    return botbooter.New(a), nil
}
```

`cli.Adapter(in, out)` mirrors slack (no error); `telegram.Adapter` and `whatsapp.Adapter` mirror discord (error).

**Internal change:** each `internal/<platform>` package replaces its current `New(...) *core.Bot` with an exported `Adapter(...)` constructor returning the concrete adapter (which implements `core.Adapter`). Bot construction moves entirely to the public shim, which calls `botbooter.New(Adapter(...))`. No import cycle: `<platform>` → `botbooter` → `internal/core`, and `<platform>` → `internal/<platform>` → `internal/core`; `botbooter` imports no platform package.

### 3. `core.New` signature + optional `BotType` self-report

```go
// internal/core
type Option func(*Bot)

func New(a Adapter, opts ...Option) *Bot {
    b := &Bot{adapter: a}
    if t, ok := a.(interface{ Type() BotType }); ok {
        b.BotType = t.Type()
    }
    for _, o := range opts {
        o(b)
    }
    return b
}
```

- `BotType` is derived from an **optional** `interface{ Type() BotType }` that an adapter may implement — the same opt-in-capability pattern as the existing `AttachmentResolver`. It is **not** added to the required `core.Adapter` interface, so this does not widen the core seam.
- Each built-in internal adapter gains a trivial `Type() BotType` returning its const.
- This removes the redundant `(enum, adapter)` double-pass at all six current call sites — the cheap internal cleanup folds in for free.
- The public `Bot.BotType` field stays, for consumers that switch on it.

### 4. Isolation

- `botbooter.New` takes the `Adapter` *interface*, creating no compile-time dependency on any concrete adapter or SDK. The root `botbooter` package stays SDK-free; the existing root row in `isolation_deps_test.go` (root closure excludes all three SDKs) continues to hold unchanged.
- No store backend is introduced, so no new isolation rows are needed in this work. (A future `botbooter/store/redis` would add its own row.)

### 5. Migration / breakage

- **Consumers: none.** `cli/slack/discord/telegram/whatsapp.New` keep identical signatures via the shims. The public API only *gains* `botbooter.New`, the `Adapter` and `Option` aliases, and the five `Adapter()` constructors.
- **Internal:** `core.New` signature changes (internal only); each internal adapter gains `Type()` and an exported `Adapter()` constructor; each public package gains `Adapter()` and reimplements `New` as a shim.
- `_examples/v1` and existing tests are unchanged (they still call the per-platform `New`). Optionally add one short example exercising `botbooter.New(slack.Adapter(...))` to document the seam.

### 6. Testing

- New tests:
  - `botbooter.New(<each platform's Adapter>)` sets `BotType` correctly (proving the optional `Type()` path).
  - `botbooter.New(slack.Adapter(a, b))` produces a Bot equivalent to `slack.New(a, b)`.
  - An internal no-op `Option` is applied by `core.New` (guards the `opts` loop until a real `WithX` exists).
- Existing tests stay green via the shims.
- `imports_test.go` / `isolation_deps_test.go` unchanged and still passing.
- `make all` (fmt + vet + lint + race test) green.

## Open items

1. **`BotType` zero-value collision.** `SlackBotType = iota` is `0`, so an adapter that does *not* implement `Type()` would default to `SlackBotType`. All built-in adapters implement `Type()`, so this is never hit in practice today. It only bites a third-party adapter that omits `Type()`. Decision: **document now; do not renumber.** Introduce an `UnknownBotType = 0` sentinel (and renumber the consts) only if/when first-class third-party adapters become a goal — that change breaks any persisted `int` BotType, so it is deferred to that roadmap item.
2. **Doc example.** Whether to ship a short `botbooter.New(... .Adapter(...))` example now. Recommend yes (small, documents the seam).

## Acceptance criteria

- `botbooter.New(adapter Adapter, opts ...Option) *Bot` exists; root `botbooter` stays SDK-free; isolation tests green.
- Each platform package exposes `Adapter(...)`; its `New(...)` is preserved as a shim with an identical signature.
- `core.New(adapter, opts ...Option)` derives `BotType` via the optional `Type()` capability; no call site passes the enum.
- No `Store`, no `Option` constructors — seam only.
- All existing tests pass; `make all` is green.
