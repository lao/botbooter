# Normalized Message + Typed Accessors (SDK-free core)

Date: 2026-06-26
Status: Approved design, ready for planning

## Problem

`internal/core.Message` exposes four platform-specific raw fields, only one
non-nil per platform:

```go
DiscordData  *discordgo.MessageCreate
SlackData    *slackevents.MessageEvent
TelegramData *models.Update
CLIData      *CLIMessage
```

Two issues follow:

1. **Handlers must reach into raw platform structs** to read anything beyond
   `UserID`/`ChannelID`/`Content`. There is no normalized author name, message
   id, timestamp, reply-to, or mentions.
2. **The "platform-agnostic engine" imports every platform SDK.** Those four
   `Message` fields *and* the four session escape-hatches on `Bot`
   (`DiscordSession`, `SlackClient`, `SlackSocketClient`, `TelegramBot`) are the
   **only** SDK usage anywhere in `internal/core`. They force `core` to import
   `discordgo`, `slackevents`, `slack`, `socketmode`, and `go-telegram/bot`.

## Goal

- **Primary:** add normalized, platform-agnostic fields to `Message` so handlers
  rarely need the raw event.
- **Secondary:** replace every exported typed-SDK field (message raw + session
  hatches) with a typed *accessor*, so `internal/core` imports zero SDKs.

This is a breaking API change. The library is pre-1.0; that is acceptable.

## Design

### New `Message` shape

```go
type Message struct {
    ID         string    // platform message id; "" when none (CLI)
    UserID     string    // author id (unchanged)
    AuthorName string    // best-effort display name; "" when unavailable
    ChannelID  string    // unchanged
    Content    string    // unchanged
    Timestamp  time.Time // message time; zero when unavailable
    ReplyToID  string    // replied-to message id; "" when not a reply
    Mentions   []string  // mentioned user ids; nil when none/unsupported

    Raw any // originating platform's untouched event; read via a typed accessor
}
```

Normalized fields are **best-effort**: a platform that cannot supply a field
leaves it at its zero value rather than doing extra work (e.g. a network call).

### Per-adapter normalization

Each adapter gains a tested `toMessage(raw) *core.Message` that maps its event:

| field      | Slack                  | Discord                       | Telegram                      | CLI         |
|------------|------------------------|-------------------------------|-------------------------------|-------------|
| ID         | `msg.TimeStamp`        | `m.ID`                        | `itoa(m.ID)`                  | ""          |
| UserID     | `msg.User`             | `m.Author.ID`                 | `itoa(m.From.ID)`             | (unchanged) |
| AuthorName | "" (id only) ¹         | `m.Author.Username`           | `From.Username`/`FirstName`   | ""          |
| Timestamp  | parse `msg.TimeStamp`  | `m.Timestamp`                 | `unix(m.Date)`                | zero        |
| ReplyToID  | `msg.ThreadTimeStamp`  | `MessageReference.MessageID`  | `itoa(ReplyToMessage.ID)`     | ""          |
| Mentions   | parse `<@U…>` in text ² | ids from `m.Mentions`        | from message entities ²       | nil         |
| Raw        | `msg`                  | `m`                           | `u`                           | `*CLIMessage` |

¹ Slack's `MessageEvent.User` is an id; the display name requires a `users.info`
API call. We deliberately leave `AuthorName` empty rather than make a per-message
network request. A handler that needs the name uses the Slack client accessor.

² Mentions is the least uniform and riskiest field (Slack text-parsing, Telegram
entity-walking). It ships as its own final increment; everything else lands
without it.

### Typed accessors (replace every exported SDK field)

Go cannot attach methods to an alias type across packages, and a method that
returns an SDK type would re-couple `core` to that SDK. The accessors are
therefore **package-level functions in the facade**, not methods:

```go
// raw event (message)
botbooter.DiscordRawEvent(m)   (*discordgo.MessageCreate, bool)
botbooter.SlackRawEvent(m)     (*slackevents.MessageEvent, bool)
botbooter.TelegramRawUpdate(m) (*models.Update, bool)
botbooter.CLIRawData(m)        (*CLIMessage, bool)

// session hatches (replace bot.DiscordSession, bot.SlackClient, …)
botbooter.DiscordSession(b)    *discordgo.Session
botbooter.SlackClient(b)       *slack.Client
botbooter.SlackSocketClient(b) *socketmode.Client
botbooter.TelegramBot(b)       *bot.Bot
```

The `(T, bool)` raw-event accessors let a caller test the platform without a nil
dance.

### Core decoupling mechanics

- `core.Message`: drop the four typed fields → `Raw any` + the normalized fields.
- `core.Bot`: drop the four session fields. Each client already lives on its
  adapter struct (`a.client`, `a.session`, `a.socket`), so no new storage is
  needed.
- `core` gains one small generic seam:

  ```go
  func AdapterAs[T any](b *Bot) (T, bool) { v, ok := b.adapter.(T); return v, ok }
  ```

  Each adapter package implements its accessors via `core.AdapterAs[*adapter](b)`
  and returns the client; the facade re-exports them. Because `*adapter` is the
  adapter package's own unexported type, only that package can instantiate the
  accessor — the seam does not leak the concrete type.
- Constructors stop setting `bot.SlackClient = …` etc.
- Result: `internal/core` imports **no** platform SDK.

### Attachments — unchanged

The lazy `GetAttachments(m)` path stays; its per-adapter internals just read
`m.Raw` (type-asserted) instead of `m.SlackData`/`m.DiscordData`/… No new eager
field — that would duplicate an already-working API and add per-message cost.

## Breaking changes / migration

- `botbooter_test.go` `Message` literals (`DiscordData:`, `SlackData:`) → `Raw:`.
- Telegram tests asserting `got.TelegramData == u` → assert via
  `TelegramRawUpdate`.
- Consumers using `bot.SlackClient` (and the other three fields) →
  `botbooter.SlackClient(bot)` (and equivalents).

## Testing

- Per-adapter table tests for `toMessage`, covering every normalized field
  including zero-value cases.
- Accessor round-trip tests (raw event + session) per platform.
- Existing lifecycle/dispatch tests are untouched.

## Increments

1. **core:** `Raw any` + normalized fields + `AdapterAs`; drop the four `Bot`
   session fields.
2. **per adapter:** `toMessage` + raw/session accessors + facade re-export
   (slack, discord, telegram, cli).
3. **mentions:** the separate, riskiest field across all platforms.
4. **docs + example refresh.**

## Followup work (deferred, not in this effort)

- **Eager attachments** — surface attachments as a populated `Message` field
  instead of (or in addition to) the lazy `GetAttachments` call. Deferred to
  avoid per-message extraction cost and a second way to do the same thing.
- **Tree-shaking SDK deps per platform** — today the module is one unit, so every
  consumer pulls all platform SDKs transitively (unchanged by this work). A
  followup could split adapters into separate modules or use build tags so a
  CLI-only consumer does not compile in `discordgo`/`slack`/`telegram`.
- **Slack author-name resolution** — optionally resolve `AuthorName` on Slack via
  a cached `users.info` lookup (or a `GetAuthorName(m)` lazy accessor mirroring
  `GetAttachments`), so Slack handlers get a name without using the raw client.
