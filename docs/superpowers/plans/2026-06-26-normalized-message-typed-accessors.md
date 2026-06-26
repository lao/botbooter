# Normalized Message + Typed Accessors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add normalized, platform-agnostic fields to `core.Message`, and replace every exported SDK-typed field (message raw + `Bot` session hatches) with typed accessor functions, so `internal/core` imports zero platform SDKs.

**Architecture:** Additive-then-subtractive. Tasks 1–7 add the new `Raw`/normalized fields, per-adapter `toMessage` mappers, and typed accessors while leaving the old typed fields in place (unused), so every commit compiles and tests pass. Task 8 deletes the now-unused SDK-typed fields from `core`, achieving the decoupling. Task 9 refreshes docs/example.

**Tech Stack:** Go 1.23+, `discordgo`, `slack-go/slack` (+ `slackevents`, `socketmode`), `go-telegram/bot` (+ `models`). Tests use the in-repo `internal/asserts` helpers.

## Global Constraints

- Module `github.com/lao/botbooter`, Go 1.23+.
- `internal/core` is the platform-agnostic engine; after Task 8 it MUST import no platform SDK. The facade `botbooter.go` and the `internal/<platform>` packages are where SDK imports live.
- Keep `botbooter.go` a pure facade: aliases + thin delegations only, no real logic.
- Tests use `internal/asserts` (`Equal`/`NoError`/`True`/`False`/`Error`/`ErrorIs`/`NotNil`), NOT testify. `asserts.Equal` requires `comparable` — compare slices via length + indexed elements, and times via `.Unix()` / `.IsZero()`.
- Exported funcs/types need doc comments (revive `exported` lint). After editing comments run `golangci-lint cache clean` before `make lint` — a stale cache reports a false "0 issues".
- Normalized fields are best-effort: a platform that cannot supply one leaves it at its zero value; never make a per-message network call to fill one.
- First-match-wins dispatch, bot-message dropping, and the connection lifecycle are unchanged by this work — do not touch them.

---

### Task 1: core — normalized fields, `Raw`, and `AdapterAs`

Purely additive: add the new fields alongside the existing four typed fields (which become unused and are deleted in Task 8) and the generic adapter-recovery seam.

**Files:**
- Modify: `internal/core/core.go` (imports; `Message` struct ~lines 66–75; add `AdapterAs` near `New`)
- Test: `internal/core/core_test.go`

**Interfaces:**
- Produces:
  - `Message` fields `ID string`, `AuthorName string`, `Timestamp time.Time`, `ReplyToID string`, `Mentions []string`, `Raw any`.
  - `func AdapterAs[T any](b *Bot) (T, bool)` — returns the Bot's adapter as `T`.

- [ ] **Step 1: Write the failing test**

Add to `internal/core/core_test.go`:

```go
// stubAdapter is a minimal core.Adapter used to exercise AdapterAs.
type stubAdapter struct{ name string }

func (s *stubAdapter) Connect(context.Context, AdapterDeps) error      { return nil }
func (s *stubAdapter) Disconnect() error                              { return nil }
func (s *stubAdapter) Send(context.Context, string, string) error     { return nil }
func (s *stubAdapter) Attachments(*Message) ([]Attachment, error)     { return nil, nil }

func TestAdapterAs(t *testing.T) {
	stub := &stubAdapter{name: "x"}
	bot := New(SlackBotType, stub)

	got, ok := AdapterAs[*stubAdapter](bot)
	asserts.True(t, ok, "AdapterAs should recover the concrete adapter type")
	asserts.Equal(t, got.name, "x", "recovered adapter identity")

	_, ok = AdapterAs[*adapterMismatch](bot)
	asserts.False(t, ok, "AdapterAs should report false for a different type")
}

type adapterMismatch struct{}

func (a *adapterMismatch) Connect(context.Context, AdapterDeps) error  { return nil }
func (a *adapterMismatch) Disconnect() error                          { return nil }
func (a *adapterMismatch) Send(context.Context, string, string) error { return nil }
func (a *adapterMismatch) Attachments(*Message) ([]Attachment, error) { return nil, nil }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestAdapterAs`
Expected: FAIL — `undefined: AdapterAs`.

- [ ] **Step 3: Add the `time` import**

In `internal/core/core.go`, add `"time"` to the standard-library import group:

```go
import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)
```

- [ ] **Step 4: Add normalized fields and `Raw` to `Message`**

Replace the `Message` struct body. Keep the four typed fields for now (Task 8 deletes them):

```go
// Message is a platform-agnostic incoming message handed to command handlers.
// UserID, ChannelID and Content are always set. The remaining normalized fields
// are best-effort: a platform that cannot supply one leaves it at its zero
// value. Raw carries the originating platform's untouched event; read it with
// the matching typed accessor (e.g. botbooter.DiscordRawEvent).
type Message struct {
	ID         string
	UserID     string
	AuthorName string
	ChannelID  string
	Content    string
	Timestamp  time.Time
	ReplyToID  string
	Mentions   []string

	Raw any

	// Deprecated: superseded by Raw; removed once all adapters migrate.
	DiscordData  *discordgo.MessageCreate
	SlackData    *slackevents.MessageEvent
	TelegramData *models.Update
	CLIData      *CLIMessage
}
```

- [ ] **Step 5: Add `AdapterAs`**

Immediately after `New` in `internal/core/core.go`:

```go
// AdapterAs returns the Bot's adapter as T, reporting whether it is that type.
// Adapter packages use it to recover their concrete adapter — and the platform
// client it holds — from a *Bot, so callers get typed access without core
// importing any platform SDK.
func AdapterAs[T any](b *Bot) (T, bool) {
	a, ok := b.adapter.(T)
	return a, ok
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/core/`
Expected: PASS (new test plus all existing core tests).

- [ ] **Step 7: Verify the whole module still builds and tests**

Run: `go build ./... && go test ./...`
Expected: all packages build and pass (adapters still set the deprecated fields — fine).

- [ ] **Step 8: Commit**

```bash
git add internal/core/core.go internal/core/core_test.go
git commit -m "feat(core): add normalized Message fields, Raw, and AdapterAs"
```

---

### Task 2: Slack adapter — `toMessage`, Raw-backed attachments, accessors

**Files:**
- Modify: `internal/slack/slack.go` (imports; `handleEventsAPI`; `Attachments`; add `toMessage`, `parseSlackTimestamp`, `RawEvent`, `Client`, `SocketClient`)
- Modify: `botbooter_test.go` (the `SlackBot` subtest literal at ~line 208)
- Test: `internal/slack/slack_test.go`

**Interfaces:**
- Consumes: `core.AdapterAs[*adapter]`, `core.Message.Raw`.
- Produces:
  - `func RawEvent(m *core.Message) (*slackevents.MessageEvent, bool)`
  - `func Client(b *core.Bot) *slackapi.Client`
  - `func SocketClient(b *core.Bot) *socketmode.Client`
  - internal `toMessage(*slackevents.MessageEvent) *core.Message` (no `Mentions` yet — Task 7).

- [ ] **Step 1: Write the failing test**

Add to `internal/slack/slack_test.go` (package `slack`):

```go
func TestToMessage(t *testing.T) {
	msg := &slackevents.MessageEvent{
		User:            "U123",
		Channel:         "C456",
		Text:            "hello",
		TimeStamp:       "1700000000.000100",
		ThreadTimeStamp: "1699999999.000000",
	}

	got := toMessage(msg)

	asserts.Equal(t, got.ID, "1700000000.000100", "ID is the ts")
	asserts.Equal(t, got.UserID, "U123", "UserID")
	asserts.Equal(t, got.ChannelID, "C456", "ChannelID")
	asserts.Equal(t, got.Content, "hello", "Content")
	asserts.Equal(t, got.AuthorName, "", "AuthorName empty (Slack gives id only)")
	asserts.Equal(t, got.ReplyToID, "1699999999.000000", "ReplyToID is the thread ts")
	asserts.Equal(t, got.Timestamp.Unix(), int64(1700000000), "Timestamp seconds")
	raw, ok := RawEvent(got)
	asserts.True(t, ok, "RawEvent recovers the event")
	asserts.True(t, raw == msg, "RawEvent returns the same pointer")
}

func TestParseSlackTimestamp(t *testing.T) {
	asserts.True(t, parseSlackTimestamp("").IsZero(), "empty ts is zero time")
	asserts.True(t, parseSlackTimestamp("not-a-ts").IsZero(), "malformed ts is zero time")
	asserts.Equal(t, parseSlackTimestamp("1700000000.000100").Unix(), int64(1700000000), "seconds parsed")
}

func TestClientAccessors(t *testing.T) {
	bot := New("xapp-test", "xoxb-test")
	asserts.NotNil(t, Client(bot), "Client accessor returns the web client")
	asserts.NotNil(t, SocketClient(bot), "SocketClient accessor returns the socket client")
}
```

Ensure the test file imports `"testing"`, `"github.com/slack-go/slack/slackevents"`, and `"github.com/lao/botbooter/internal/asserts"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/slack/ -run 'TestToMessage|TestParseSlackTimestamp|TestClientAccessors'`
Expected: FAIL — `undefined: toMessage` / `parseSlackTimestamp` / `RawEvent` / `Client`.

- [ ] **Step 3: Update imports**

In `internal/slack/slack.go`, add `"strconv"` and `"time"` to the stdlib group (keeping `"context"`, `"strings"`).

- [ ] **Step 4: Add `toMessage` and `parseSlackTimestamp`**

```go
// toMessage maps a Slack message event onto a platform-agnostic Message.
// AuthorName is left empty: the event carries only a user id, and resolving a
// name would require a per-message API call.
func toMessage(msg *slackevents.MessageEvent) *core.Message {
	return &core.Message{
		ID:        msg.TimeStamp,
		UserID:    msg.User,
		ChannelID: msg.Channel,
		Content:   msg.Text,
		Timestamp: parseSlackTimestamp(msg.TimeStamp),
		ReplyToID: msg.ThreadTimeStamp,
		Raw:       msg,
	}
}

// parseSlackTimestamp converts a Slack ts ("1700000000.000100", seconds with a
// 6-digit microsecond fraction) into a UTC time, returning the zero time for an
// empty or malformed value.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	secs, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var nsec int64
	if frac != "" {
		for len(frac) < 6 {
			frac += "0"
		}
		if micros, err := strconv.ParseInt(frac[:6], 10, 64); err == nil {
			nsec = micros * 1000
		}
	}
	return time.Unix(s, nsec).UTC()
}
```

- [ ] **Step 5: Dispatch via `toMessage` and read attachments from `Raw`**

In `handleEventsAPI`, replace the inline `&core.Message{...}` with:

```go
	if msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent); ok {
		deps.Dispatch(ctx, toMessage(msg))
	}
```

Replace `Attachments`:

```go
// Attachments returns the files attached to the message's Slack event.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	msg, _ := m.Raw.(*slackevents.MessageEvent)
	return attachmentsFromMessage(msg), nil
}
```

(`attachmentsFromMessage` already returns nil for a nil message.)

- [ ] **Step 6: Add the accessors**

```go
// RawEvent returns the raw Slack message event carried on m, reporting whether
// m originated from Slack.
func RawEvent(m *core.Message) (*slackevents.MessageEvent, bool) {
	e, ok := m.Raw.(*slackevents.MessageEvent)
	return e, ok
}

// Client returns the Slack Web API client backing b, or nil if b is not a Slack
// bot.
func Client(b *core.Bot) *slackapi.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}

// SocketClient returns the Socket Mode client backing b, or nil if b is not a
// Slack bot.
func SocketClient(b *core.Bot) *socketmode.Client {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.socket
	}
	return nil
}
```

- [ ] **Step 7: Migrate the facade Slack attachment test**

In `botbooter_test.go`, the `SlackBot` subtest builds `&botbooter.Message{ SlackData: ... }`. Change the field name to `Raw`:

```go
		message := &botbooter.Message{
			Raw: &slackevents.MessageEvent{
				Files: []slackevents.File{
					{Mimetype: "image/png", URLPrivate: "https://example.com/image.png"},
				},
			},
		}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/slack/ ./...`
Expected: PASS, including the migrated facade `TestBot_GetAttachments/SlackBot`.

- [ ] **Step 9: Commit**

```bash
git add internal/slack/slack.go internal/slack/slack_test.go botbooter_test.go
git commit -m "feat(slack): normalize messages, back attachments with Raw, add accessors"
```

---

### Task 3: Discord adapter — `toMessage`, Raw-backed attachments, accessors

**Files:**
- Modify: `internal/discord/discord.go` (`onMessage`; `Attachments`; add `toMessage`, `RawEvent`, `Session`)
- Modify: `botbooter_test.go` (the `DiscordBot` subtest literal at ~line 179)
- Test: `internal/discord/discord_test.go`

**Interfaces:**
- Consumes: `core.AdapterAs[*adapter]`, `core.Message.Raw`.
- Produces:
  - `func RawEvent(m *core.Message) (*discordgo.MessageCreate, bool)`
  - `func Session(b *core.Bot) *discordgo.Session`
  - internal `toMessage(*discordgo.MessageCreate) *core.Message` (no `Mentions` yet — Task 7).

- [ ] **Step 1: Write the failing test**

Add to `internal/discord/discord_test.go` (package `discord`):

```go
func TestToMessage(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	mc := &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:               "M1",
		ChannelID:        "C1",
		Content:          "hi",
		Timestamp:        when,
		Author:           &discordgo.User{ID: "U1", Username: "alice"},
		MessageReference: &discordgo.MessageReference{MessageID: "M0"},
	}}

	got := toMessage(mc)

	asserts.Equal(t, got.ID, "M1", "ID")
	asserts.Equal(t, got.UserID, "U1", "UserID")
	asserts.Equal(t, got.AuthorName, "alice", "AuthorName")
	asserts.Equal(t, got.ChannelID, "C1", "ChannelID")
	asserts.Equal(t, got.Content, "hi", "Content")
	asserts.Equal(t, got.ReplyToID, "M0", "ReplyToID")
	asserts.Equal(t, got.Timestamp.Unix(), int64(1700000000), "Timestamp")
	raw, ok := RawEvent(got)
	asserts.True(t, ok, "RawEvent recovers the event")
	asserts.True(t, raw == mc, "RawEvent returns the same pointer")
}

func TestSessionAccessor(t *testing.T) {
	bot, err := New("Bot token")
	asserts.NoError(t, err, "New")
	asserts.NotNil(t, Session(bot), "Session accessor returns the gateway session")
}
```

Ensure the test imports `"testing"`, `"time"`, `"github.com/bwmarrin/discordgo"`, and `asserts`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/discord/ -run 'TestToMessage|TestSessionAccessor'`
Expected: FAIL — `undefined: toMessage` / `RawEvent` / `Session`.

- [ ] **Step 3: Add `toMessage`**

```go
// toMessage maps a Discord message-create event onto a platform-agnostic
// Message. The caller (onMessage) guarantees a non-nil Author, but the guards
// keep toMessage safe in isolation.
func toMessage(m *discordgo.MessageCreate) *core.Message {
	msg := &core.Message{
		ID:        m.ID,
		ChannelID: m.ChannelID,
		Content:   m.Content,
		Timestamp: m.Timestamp,
		Raw:       m,
	}
	if m.Author != nil {
		msg.UserID = m.Author.ID
		msg.AuthorName = m.Author.Username
	}
	if m.MessageReference != nil {
		msg.ReplyToID = m.MessageReference.MessageID
	}
	return msg
}
```

- [ ] **Step 4: Dispatch via `toMessage` and read attachments from `Raw`**

In `onMessage`, replace the inline `&core.Message{...}` with:

```go
	deps.Dispatch(ctx, toMessage(m))
```

Replace `Attachments`:

```go
// Attachments returns the files attached to the message's Discord event.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	mc, ok := m.Raw.(*discordgo.MessageCreate)
	if !ok || mc == nil {
		return nil, nil
	}
	return attachmentsFromMessage(mc.Message), nil
}
```

- [ ] **Step 5: Add the accessors**

```go
// RawEvent returns the raw Discord message-create event carried on m, reporting
// whether m originated from Discord.
func RawEvent(m *core.Message) (*discordgo.MessageCreate, bool) {
	e, ok := m.Raw.(*discordgo.MessageCreate)
	return e, ok
}

// Session returns the discordgo gateway session backing b, or nil if b is not a
// Discord bot.
func Session(b *core.Bot) *discordgo.Session {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.session
	}
	return nil
}
```

- [ ] **Step 6: Migrate the facade Discord attachment test**

In `botbooter_test.go`, the `DiscordBot` subtest builds `&botbooter.Message{ DiscordData: ... }`. Change the field name to `Raw`:

```go
		message := &botbooter.Message{
			Raw: &discordgo.MessageCreate{
				Message: &discordgo.Message{
					Attachments: []*discordgo.MessageAttachment{
						{URL: "https://example.com/image.png", Width: 100, Height: 100},
					},
				},
			},
		}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/discord/ ./...`
Expected: PASS, including `TestBot_GetAttachments/DiscordBot` and `/DiscordBotNilData`.

- [ ] **Step 8: Commit**

```bash
git add internal/discord/discord.go internal/discord/discord_test.go botbooter_test.go
git commit -m "feat(discord): normalize messages, back attachments with Raw, add accessors"
```

---

### Task 4: Telegram adapter — `toMessage`, Raw-backed attachments, accessors

**Files:**
- Modify: `internal/telegram/telegram.go` (`onUpdate`; `Attachments`; add `toMessage`, `telegramAuthorName`, `RawUpdate`, `Client`)
- Modify: `internal/telegram/telegram_test.go` (the two `got.TelegramData == u` assertions at ~lines 95, 121)
- Test: `internal/telegram/telegram_test.go`

**Interfaces:**
- Consumes: `core.AdapterAs[*adapter]`, `core.Message.Raw`.
- Produces:
  - `func RawUpdate(m *core.Message) (*models.Update, bool)`
  - `func Client(b *core.Bot) *bot.Bot`
  - internal `toMessage(*models.Update) *core.Message` (no `Mentions` yet — Task 7).

- [ ] **Step 1: Write the failing test**

Add to `internal/telegram/telegram_test.go` (package `telegram`):

```go
func TestToMessage(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		ID:             42,
		From:           &models.User{ID: 7, Username: "bob"},
		Chat:           models.Chat{ID: 100},
		Text:           "hey",
		Date:           1700000000,
		ReplyToMessage: &models.Message{ID: 41},
	}}

	got := toMessage(u)

	asserts.Equal(t, got.ID, "42", "ID")
	asserts.Equal(t, got.UserID, "7", "UserID")
	asserts.Equal(t, got.AuthorName, "bob", "AuthorName from username")
	asserts.Equal(t, got.ChannelID, "100", "ChannelID from chat id")
	asserts.Equal(t, got.Content, "hey", "Content")
	asserts.Equal(t, got.ReplyToID, "41", "ReplyToID")
	asserts.Equal(t, got.Timestamp.Unix(), int64(1700000000), "Timestamp")
	raw, ok := RawUpdate(got)
	asserts.True(t, ok, "RawUpdate recovers the update")
	asserts.True(t, raw == u, "RawUpdate returns the same pointer")
}

func TestToMessageCaptionAndFirstName(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		ID:      1,
		From:    &models.User{ID: 7, FirstName: "Bob"},
		Chat:    models.Chat{ID: 100},
		Caption: "a photo",
		Date:    1700000000,
	}}

	got := toMessage(u)

	asserts.Equal(t, got.Content, "a photo", "Content falls back to caption")
	asserts.Equal(t, got.AuthorName, "Bob", "AuthorName falls back to first name")
	asserts.Equal(t, got.ReplyToID, "", "no reply")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/telegram/ -run 'TestToMessage'`
Expected: FAIL — `undefined: toMessage` / `RawUpdate`.

- [ ] **Step 3: Add the `time` import**

In `internal/telegram/telegram.go`, add `"time"` to the stdlib group (keeping `"context"`, `"strconv"`, `"strings"`).

- [ ] **Step 4: Add `toMessage` and `telegramAuthorName`**

```go
// toMessage maps a Telegram update's message onto a platform-agnostic Message.
// The caller (onUpdate) guarantees a non-nil Message and From. Content is the
// text, or the caption for media-only messages.
func toMessage(u *models.Update) *core.Message {
	m := u.Message
	content := m.Text
	if content == "" {
		content = m.Caption
	}
	msg := &core.Message{
		ID:         strconv.Itoa(m.ID),
		UserID:     strconv.FormatInt(m.From.ID, 10),
		AuthorName: telegramAuthorName(m.From),
		ChannelID:  strconv.FormatInt(m.Chat.ID, 10),
		Content:    content,
		Timestamp:  time.Unix(int64(m.Date), 0).UTC(),
		Raw:        u,
	}
	if m.ReplyToMessage != nil {
		msg.ReplyToID = strconv.Itoa(m.ReplyToMessage.ID)
	}
	return msg
}

// telegramAuthorName prefers the @username, falling back to the first name.
func telegramAuthorName(u *models.User) string {
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return u.Username
	}
	return u.FirstName
}
```

- [ ] **Step 5: Dispatch via `toMessage` and read attachments from `Raw`**

In `onUpdate`, after the existing guards (`m == nil || m.From == nil`, bot/self checks, deps load), replace the trailing `content := ...` block and the inline `&core.Message{...}` dispatch with:

```go
	deps.Dispatch(ctx, toMessage(u))
```

Replace `Attachments`:

```go
// Attachments returns the files attached to the message's Telegram update.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	u, ok := m.Raw.(*models.Update)
	if !ok || u == nil {
		return nil, nil
	}
	return attachmentsFromMessage(u.Message), nil
}
```

- [ ] **Step 6: Add the accessors**

```go
// RawUpdate returns the raw Telegram update carried on m, reporting whether m
// originated from Telegram.
func RawUpdate(m *core.Message) (*models.Update, bool) {
	u, ok := m.Raw.(*models.Update)
	return u, ok
}

// Client returns the go-telegram bot client backing b, or nil if b is not a
// Telegram bot.
func Client(b *core.Bot) *bot.Bot {
	if a, ok := core.AdapterAs[*adapter](b); ok {
		return a.client
	}
	return nil
}
```

- [ ] **Step 7: Migrate the existing raw-carry assertions**

In `internal/telegram/telegram_test.go`, the two assertions `asserts.True(t, got.TelegramData == u, ...)` must read the update via `RawUpdate` now:

```go
		raw, ok := RawUpdate(got)
		asserts.True(t, ok, "raw update should be recoverable")
		asserts.True(t, raw == u, "raw update carried so handlers can read it")
```

Apply the same replacement at both call sites (the second keeps its "read the photo" intent — adjust the message string to match).

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/telegram/ ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/telegram/telegram.go internal/telegram/telegram_test.go
git commit -m "feat(telegram): normalize messages, back attachments with Raw, add accessors"
```

---

### Task 5: CLI adapter — carry `Raw`, add accessor

The CLI message has only `UserID`/`ChannelID`/`Content` plus the parsed line. Carry the parsed line on `Raw` and read attachments from it.

**Files:**
- Modify: `internal/cli/cli.go` (`Connect` dispatch ~line 53; `Attachments`; add `RawData`)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `core.Message.Raw`, `core.CLIMessage`.
- Produces: `func RawData(m *core.Message) (*core.CLIMessage, bool)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/cli_test.go` (package `cli`):

```go
func TestRawData(t *testing.T) {
	data := &core.CLIMessage{Text: "hello"}
	m := &core.Message{Raw: data}

	got, ok := RawData(m)
	asserts.True(t, ok, "RawData recovers the CLI payload")
	asserts.True(t, got == data, "RawData returns the same pointer")

	_, ok = RawData(&core.Message{})
	asserts.False(t, ok, "RawData reports false when Raw is unset")
}
```

Ensure the test imports `"github.com/lao/botbooter/internal/core"` and `asserts`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestRawData`
Expected: FAIL — `undefined: RawData`.

- [ ] **Step 3: Carry the parsed line on `Raw`**

In `Connect`, change the dispatched message field from `CLIData` to `Raw`:

```go
			deps.Dispatch(ctx, &core.Message{
				UserID:    userID,
				ChannelID: channelID,
				Content:   line,
				Raw:       parseMessage(line),
			})
```

- [ ] **Step 4: Read attachments from `Raw` and add the accessor**

Replace `Attachments`:

```go
// Attachments returns the attachments parsed from the message's typed line.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	data, ok := m.Raw.(*core.CLIMessage)
	if !ok || data == nil {
		return nil, nil
	}
	return data.Attachments, nil
}

// RawData returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func RawData(m *core.Message) (*core.CLIMessage, bool) {
	data, ok := m.Raw.(*core.CLIMessage)
	return data, ok
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/ ./...`
Expected: PASS (including the facade `TestBot_GetAttachments/CLIBot`, which passes a message with no `Raw` and expects zero attachments).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(cli): carry parsed line on Raw, add RawData accessor"
```

---

### Task 6: Facade — re-export the typed accessors

**Files:**
- Modify: `botbooter.go` (imports; add the accessor functions)
- Test: `botbooter_test.go`

**Interfaces:**
- Consumes: `discord.RawEvent`/`Session`, `slack.RawEvent`/`Client`/`SocketClient`, `telegram.RawUpdate`/`Client`, `cli.RawData`.
- Produces: `botbooter.DiscordRawEvent`, `SlackRawEvent`, `TelegramRawUpdate`, `CLIRawData`, `DiscordSession`, `SlackClient`, `SlackSocketClient`, `TelegramBot`.

- [ ] **Step 1: Write the failing test**

Add to `botbooter_test.go` (package `botbooter_test`):

```go
func TestRawAccessors(t *testing.T) {
	t.Run("Discord", func(t *testing.T) {
		mc := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "M1"}}
		got, ok := botbooter.DiscordRawEvent(&botbooter.Message{Raw: mc})
		asserts.True(t, ok, "DiscordRawEvent")
		asserts.True(t, got == mc, "same pointer")
	})

	t.Run("Slack", func(t *testing.T) {
		e := &slackevents.MessageEvent{User: "U1"}
		got, ok := botbooter.SlackRawEvent(&botbooter.Message{Raw: e})
		asserts.True(t, ok, "SlackRawEvent")
		asserts.True(t, got == e, "same pointer")
	})

	t.Run("WrongPlatform", func(t *testing.T) {
		_, ok := botbooter.SlackRawEvent(&botbooter.Message{Raw: &discordgo.MessageCreate{}})
		asserts.False(t, ok, "SlackRawEvent on a Discord message reports false")
	})
}

func TestSessionAccessors(t *testing.T) {
	slackBot := botbooter.InitAsSlackBot("xapp-test", "xoxb-test")
	asserts.NotNil(t, botbooter.SlackClient(slackBot), "SlackClient")
	asserts.NotNil(t, botbooter.SlackSocketClient(slackBot), "SlackSocketClient")

	cliBot := botbooter.InitAsCLIBot(emptyReader{}, &syncBuffer{})
	asserts.Equal(t, botbooter.SlackClient(cliBot) == nil, true, "SlackClient nil for non-Slack bot")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestRawAccessors|TestSessionAccessors'`
Expected: FAIL — `undefined: botbooter.DiscordRawEvent` etc.

- [ ] **Step 3: Update facade imports**

In `botbooter.go`, extend the import block (alias the Slack SDK so it does not collide with the `internal/slack` import named `slack`):

```go
import (
	"io"

	"github.com/bwmarrin/discordgo"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/lao/botbooter/internal/cli"
	"github.com/lao/botbooter/internal/core"
	"github.com/lao/botbooter/internal/discord"
	"github.com/lao/botbooter/internal/slack"
	"github.com/lao/botbooter/internal/telegram"
)
```

- [ ] **Step 4: Add the accessor functions**

Append to `botbooter.go`:

```go
// DiscordRawEvent returns the raw Discord event carried on m, reporting whether
// m originated from Discord.
func DiscordRawEvent(m *Message) (*discordgo.MessageCreate, bool) { return discord.RawEvent(m) }

// SlackRawEvent returns the raw Slack event carried on m, reporting whether m
// originated from Slack.
func SlackRawEvent(m *Message) (*slackevents.MessageEvent, bool) { return slack.RawEvent(m) }

// TelegramRawUpdate returns the raw Telegram update carried on m, reporting
// whether m originated from Telegram.
func TelegramRawUpdate(m *Message) (*models.Update, bool) { return telegram.RawUpdate(m) }

// CLIRawData returns the parsed CLI line carried on m, reporting whether m
// originated from the CLI adapter.
func CLIRawData(m *Message) (*CLIMessage, bool) { return cli.RawData(m) }

// DiscordSession returns the discordgo session backing b, or nil if b is not a
// Discord bot.
func DiscordSession(b *Bot) *discordgo.Session { return discord.Session(b) }

// SlackClient returns the Slack Web API client backing b, or nil if b is not a
// Slack bot.
func SlackClient(b *Bot) *slackapi.Client { return slack.Client(b) }

// SlackSocketClient returns the Socket Mode client backing b, or nil if b is not
// a Slack bot.
func SlackSocketClient(b *Bot) *socketmode.Client { return slack.SocketClient(b) }

// TelegramBot returns the go-telegram bot client backing b, or nil if b is not a
// Telegram bot.
func TelegramBot(b *Bot) *bot.Bot { return telegram.Client(b) }
```

Confirm `CLIMessage` is already an alias in `botbooter.go` (`CLIMessage = core.CLIMessage`); if not, add it to the alias block.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test . ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add botbooter.go botbooter_test.go
git commit -m "feat(botbooter): expose typed raw-event and session accessors"
```

---

### Task 7: Mentions across all adapters

Populate `Message.Mentions` (user ids). Discord has structured mentions; Slack is parsed from text; Telegram exposes ids only via `text_mention` entities.

**Files:**
- Modify: `internal/discord/discord.go` (`toMessage`)
- Modify: `internal/slack/slack.go` (`toMessage`; add `slackMentions`)
- Modify: `internal/telegram/telegram.go` (`toMessage`; add `telegramMentions`)
- Test: each adapter's `_test.go`

**Interfaces:**
- Produces: populated `Message.Mentions []string` per platform (nil when none).

- [ ] **Step 1: Write the failing tests**

Discord — add to `internal/discord/discord_test.go`:

```go
func TestToMessageMentions(t *testing.T) {
	mc := &discordgo.MessageCreate{Message: &discordgo.Message{
		Author:   &discordgo.User{ID: "U1"},
		Mentions: []*discordgo.User{{ID: "U2"}, {ID: "U3"}},
	}}
	got := toMessage(mc)
	asserts.Equal(t, strings.Join(got.Mentions, ","), "U2,U3", "mention ids")
}
```

Slack — add to `internal/slack/slack_test.go`:

```go
func TestSlackMentions(t *testing.T) {
	got := toMessage(&slackevents.MessageEvent{Text: "hi <@U2> and <@U3|carol> there"})
	asserts.Equal(t, strings.Join(got.Mentions, ","), "U2,U3", "parsed mention ids")

	none := toMessage(&slackevents.MessageEvent{Text: "no mentions"})
	asserts.Equal(t, len(none.Mentions), 0, "no mentions -> nil")
}
```

Telegram — add to `internal/telegram/telegram_test.go`:

```go
func TestTelegramMentions(t *testing.T) {
	u := &models.Update{Message: &models.Message{
		From: &models.User{ID: 1},
		Chat: models.Chat{ID: 1},
		Text: "hi Bob",
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeTextMention, User: &models.User{ID: 99}},
			{Type: models.MessageEntityTypeMention}, // @username — no id, skipped
		},
	}}
	got := toMessage(u)
	asserts.Equal(t, strings.Join(got.Mentions, ","), "99", "text_mention id only")
}
```

Add `"strings"` to each test file's imports where missing.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/discord/ ./internal/slack/ ./internal/telegram/ -run Mention`
Expected: FAIL (mentions currently nil).

- [ ] **Step 3: Discord — collect structured mention ids**

In `discord` `toMessage`, before `return msg`:

```go
	for _, u := range m.Mentions {
		if u != nil {
			msg.Mentions = append(msg.Mentions, u.ID)
		}
	}
```

- [ ] **Step 4: Slack — parse mention ids from text**

In `internal/slack/slack.go`, add a package-level compiled regexp and helper, then set `Mentions` in `toMessage`:

```go
// slackMentionRE matches "<@U123>" and "<@U123|label>" mention tokens.
var slackMentionRE = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)

// slackMentions extracts mentioned user ids from message text.
func slackMentions(text string) []string {
	matches := slackMentionRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m[1])
	}
	return ids
}
```

Add `"regexp"` to the imports, and set the field in `toMessage`:

```go
		Mentions:  slackMentions(msg.Text),
```

- [ ] **Step 5: Telegram — collect text_mention ids**

In `internal/telegram/telegram.go`, add a helper and set `Mentions` in `toMessage` (use the message's `Entities`, falling back to `CaptionEntities` for media):

```go
// telegramMentions collects user ids from text_mention entities — the only
// entity kind that carries a numeric user id (a plain @username "mention"
// entity references a name, not an id, so it is skipped).
func telegramMentions(m *models.Message) []string {
	entities := m.Entities
	if len(entities) == 0 {
		entities = m.CaptionEntities
	}
	var ids []string
	for _, e := range entities {
		if e.Type == models.MessageEntityTypeTextMention && e.User != nil {
			ids = append(ids, strconv.FormatInt(e.User.ID, 10))
		}
	}
	return ids
}
```

In `toMessage`, set:

```go
		Mentions:   telegramMentions(m),
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/discord/ ./internal/slack/ ./internal/telegram/ ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/discord/discord.go internal/discord/discord_test.go internal/slack/slack.go internal/slack/slack_test.go internal/telegram/telegram.go internal/telegram/telegram_test.go
git commit -m "feat: populate Message.Mentions across adapters"
```

---

### Task 8: Decouple core — delete the SDK-typed fields

Every adapter now uses `Raw` and the accessors; the deprecated `Message` fields and the `Bot` session fields are unused. Delete them and drop their SDK imports from `core`.

**Files:**
- Modify: `internal/core/core.go` (`Message` struct; `Bot` struct; imports)
- Modify: `internal/slack/slack.go`, `internal/discord/discord.go`, `internal/telegram/telegram.go` (constructors stop setting the removed `Bot` fields)
- Test: full suite + an SDK-free guard

- [ ] **Step 1: Remove the deprecated `Message` fields**

In `internal/core/core.go`, delete the four-line deprecated block from `Message`, leaving the normalized fields and `Raw any`.

- [ ] **Step 2: Remove the `Bot` session fields**

Delete these four lines from the `Bot` struct:

```go
	DiscordSession    *discordgo.Session
	SlackClient       *slack.Client
	SlackSocketClient *socketmode.Client
	TelegramBot       *bot.Bot
```

- [ ] **Step 3: Drop the now-unused SDK imports from core**

In `internal/core/core.go`, remove the platform-SDK import lines (`discordgo`, `go-telegram/bot`, `go-telegram/bot/models`, `slack`, `slackevents`, `socketmode`). The import block becomes standard-library only.

- [ ] **Step 4: Stop the constructors setting the removed fields**

- `internal/slack/slack.go` `New`: delete `bot.SlackClient = client` and `bot.SlackSocketClient = socket`; return the bot built by `core.New` directly.
- `internal/discord/discord.go` `New`: delete `bot.DiscordSession = dg`.
- `internal/telegram/telegram.go` `New`: delete `b.TelegramBot = tg`.

(The clients still live on each adapter struct, which the accessors read.)

- [ ] **Step 5: Build and run the whole suite**

Run: `go build ./... && go test ./...`
Expected: PASS. If a constructor now has an unused local (e.g. `bot :=`), inline or rename it to `_` as the compiler directs.

- [ ] **Step 6: Add an SDK-free guard test for core**

Create `internal/core/imports_test.go`:

```go
package core

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
)

// TestCoreImportsNoPlatformSDK locks in the decoupling: the engine must not
// import any platform SDK. Adapters and the facade own those imports.
func TestCoreImportsNoPlatformSDK(t *testing.T) {
	banned := []string{"discordgo", "slack-go/slack", "go-telegram/bot"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	asserts.NoError(t, err, "parse core package")

	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			for _, imp := range file.Imports {
				for _, b := range banned {
					asserts.False(t, strings.Contains(imp.Path.Value, b),
						"core file "+name+" must not import "+b)
				}
			}
		}
	}
}
```

- [ ] **Step 7: Run the guard**

Run: `go test ./internal/core/ -run TestCoreImportsNoPlatformSDK`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/core/core.go internal/core/imports_test.go internal/slack/slack.go internal/discord/discord.go internal/telegram/telegram.go
git commit -m "refactor(core): drop SDK-typed Message and Bot fields; core is now SDK-free"
```

---

### Task 9: Docs, example, and full quality gate

**Files:**
- Modify: `README.md`, `docs/platforms.md` (document the accessor functions if escape hatches are mentioned)
- Modify: `examples/v1/main.go` (optional: log a normalized field to demonstrate it)

- [ ] **Step 1: Document the typed accessors**

In `README.md` and `docs/platforms.md`, where the raw-client/raw-event escape hatch is described (or add a short subsection if absent), state the new API: handlers read normalized fields (`m.AuthorName`, `m.Timestamp`, `m.ReplyToID`, `m.Mentions`, `m.ID`) directly, and reach the raw platform event or client via `botbooter.DiscordRawEvent(m)` / `botbooter.SlackClient(bot)` etc. Keep the existing platform table formatting.

- [ ] **Step 2: (Optional) show a normalized field in the example**

In `examples/v1/main.go` `loggingMiddleware`, extend the log line to include `message.AuthorName` when set, demonstrating the normalized API. Keep it a one-line change.

- [ ] **Step 3: Run the full quality gate**

Run:
```bash
golangci-lint cache clean
make all
```
Expected: `fmt`, `vet`, `lint`, and `test` all pass. (Cache clean avoids a stale "0 issues" hiding a revive `exported` finding on the new accessor doc comments.)

- [ ] **Step 4: Run the race detector**

Run: `make test-race`
Expected: PASS (the concurrency-heavy lifecycle is untouched, but CI runs this).

- [ ] **Step 5: Commit**

```bash
git add README.md docs/platforms.md examples/v1/main.go
git commit -m "docs: document normalized Message fields and typed accessors"
```

---

## Self-Review

**Spec coverage:**
- Normalized fields (ID/AuthorName/Timestamp/ReplyToID/Mentions) → Tasks 2–5 (base) + Task 7 (mentions). ✅
- `Raw any` + typed accessors (message + session) → Tasks 2–6. ✅
- `core` SDK-free via `AdapterAs` + field removal → Tasks 1 & 8 + guard test. ✅
- Attachments stay lazy, backed by `Raw` → Tasks 2–5. ✅
- Migration (test literals, telegram assertions, constructors) → Tasks 2, 3, 4, 8. ✅
- Followup work (eager attachments, tree-shaking, Slack name resolution) → intentionally NOT in any task. ✅

**Placeholder scan:** No `TBD`/`add error handling`/"similar to Task N"; every code step shows real code. ✅

**Type consistency:** `AdapterAs[T any](b *Bot) (T, bool)`, `toMessage` per package, accessor names (`RawEvent`/`RawUpdate`/`RawData`, `Client`/`SocketClient`/`Session`) and their facade re-exports (`DiscordRawEvent`/`SlackRawEvent`/`TelegramRawUpdate`/`CLIRawData`, `DiscordSession`/`SlackClient`/`SlackSocketClient`/`TelegramBot`) are consistent across tasks. `parseSlackTimestamp`, `telegramAuthorName`, `slackMentions`, `telegramMentions` are each defined once and used where referenced. ✅
