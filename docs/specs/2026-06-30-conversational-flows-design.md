# Conversational Flows — Design Spec

- **Date:** 2026-06-30
- **Branch:** `worktree-conversational-flows`
- **Status:** Implemented (see the README's [Conversational flows](../../README.md#conversational-flows) section); the §-references below remain the deferred-work index.

## Goal

Let a bot run a **multi-step, context-aware dialog**: ask one question, wait for that
user's reply, optionally validate it, then advance to the next question — until a whole
form (e.g. sign-up: name → email → address → profession → age → password) is collected.
Inspired by [go-sarah's `UserContext`](https://github.com/oklahomer/go-sarah), where a
handler hands the framework "the function to run on this user's next message" instead of
re-routing it through the normal command table.

**The hard requirement is developer ergonomics.** The whole point is that a consumer
defines a form in a few declarative lines and never hand-writes a per-user state machine,
a message-routing switch, or a `map[userID]step`.

### Non-goals (this design)

- Branching/conditional flows (step N depends on answer to step M). v1 is **linear**;
  the engine leaves room for it (see [Deferred work](#deferred-work)).
- Rich inputs (buttons, Slack blocks, Discord components). v1 is plain text in / text out;
  it composes later with the roadmap's "Richer message types".
- Cross-process durability as a launch blocker — v1 is **in-memory only** and loses
  in-flight flows on restart (see [§5 Persistence](#5-persistence--in-memory-now-store-ready-later)).
- Guaranteed within-conversation message **ordering** under concurrent delivery — see
  [§4 Concurrency](#4-concurrency-model-the-load-bearing-part). v1 guarantees no
  corruption, not receive-order processing.
- **Multi-instance / horizontal scaling.** v1 is **single-instance only**; running more
  than one replica of the same bot breaks flows (see [§4](#4-concurrency-model-the-load-bearing-part)
  and [Deferred work](#deferred-work)). Peer frameworks (go-joe, go-sarah) have the same
  single-instance constraint; a shared Memory/Store gives state *visibility*, not the
  cross-process *coordination* multi-instance needs.

## Background — current state

- **Handler signature is void + side-effecting:** `func(ctx, *Bot, *Message)`; handlers
  reply via `b.SendMessageContext(...)`. There is no return value, so go-sarah's
  "return the next func" model does not map 1:1 — the next step must be carried as
  **state the engine holds**, not as a value the handler returns.
- **Dispatch** (`internal/core/core.go`, `core.Bot.dispatch`) is: recover-wrapped →
  middleware chain → inner handler that does **first-match-wins** over the compiled
  command table, else the unknown-command handler. First match wins; no fallthrough.
- **`Message` carries `UserID` and `ChannelID`** (both always set) — enough to key a
  conversation. `Content` is the text to feed into the current step. There is **no**
  cross-platform "is this a DM" flag today (relevant to [§4](#4-concurrency-model-the-load-bearing-part)
  channel scoping).
- **Dispatch is concurrent per conversation on three of five adapters.** Verified:
  Discord registers via `discordgo.AddHandler`, which runs each gateway event in its own
  goroutine (`SyncEvents` is never set), so two messages from one user race into
  `dispatch`; WhatsApp spawns a goroutine per webhook POST (`internal/whatsapp` —
  `go func(){ … deps.Dispatch … }`); the go-telegram library fans each update to a
  handler goroutine. Slack (socketmode channel loop) and CLI (scanner loop) are serial.
  **The engine cannot assume serial delivery.** This is what makes [§4](#4-concurrency-model-the-load-bearing-part)
  load-bearing rather than defensive.
- **Option seam + Store are already planned** (`docs/specs/2026-06-29-adapter-option-seam-design.md`,
  README Roadmap): `botbooter.New(adapter, opts ...Option)` and a future pluggable
  `Store` (`WithStore(...)`, in-memory default → Redis). Conversation state is the
  *first real consumer* of that Store. **This design depends on neither landing first:**
  it ships its own in-memory store and adopts `Store` when present, and it puts every
  per-`Bot` knob it needs on the `Flow` (not on a `WithX` option) so it needs no Option
  seam — see [§4](#4-concurrency-model-the-load-bearing-part) cancel-word.

## Design

### 1. Developer-facing API — declarative `Flow` (primary, recommended)

A flow has a **stable id** (load-bearing for persistence, see §3/§5), a list of steps
(prompt + answer key + optional per-step options), and lifecycle callbacks. The user's
sign-up example, in full:

```go
signup := botbooter.NewFlow("signup").                   // id is required and must be stable
    Ask("name",       "What's your name?").
    Ask("email",      "What's your email?",       botbooter.Validate(validEmail)).
    Ask("address",    "What's your address?").
    Ask("profession", "What's your profession?").
    Ask("age",        "How old are you?",          botbooter.Validate(validAge)).
    Ask("password",   "Choose a password.",        botbooter.Secret()).
    OnComplete(func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message, a botbooter.Answers) {
        createUser(a.Get("name"), a.Get("email"), a.Get("address"),
            a.Get("profession"), a.Get("age"), a.Get("password"))
        _ = b.SendMessageContext(ctx, m.ChannelID, "You're all set 🎉")
    })

// Register like any command. HandleFlow returns an error: it compiles the pattern AND
// validates the flow (see "HandleFlow contract" below).
if err := bot.HandleFlow("^sign ?up$", signup); err != nil {
    log.Fatal(err)
}
```

Semantics:

- `HandleFlow(pattern, flow)` registers a normal start-command **and** the flow in a
  per-`Bot` flow registry keyed by `flow.id`. When the pattern matches and no flow is
  active for the conversation, the engine **starts** the flow (records pending state and
  sends the first prompt) — and that start runs **inside the per-key critical section**
  (see [§4](#4-concurrency-model-the-load-bearing-part)), not loose in the command loop.
- Each subsequent message from the same conversation is routed to the active flow (not the
  command table): the engine runs the current step's validator; on success it stores the
  answer under the step key, slides the TTL, and sends the next prompt; on the last step it
  clears state and runs `OnComplete`.
- `Validate(fn func(string) error)` — on error the engine **re-prompts the same step**
  (using `err.Error()` as the nudge when non-empty) and does not advance or store.
- **Empty / whitespace answers are non-answers, not stored values.** The engine trims the
  content; an empty result re-prompts the same step *before* the validator runs, so a blank
  line never silently satisfies a step (e.g. `name=""`) and a validator is never asked to
  rule on empty input. Empty answers are therefore unconditionally rejected — there is no
  validator opt-out in v1. (This is distinct from the bot's own echo, which adapters already
  drop before dispatch — see §4.)
- `Secret()` — marks the answer sensitive. Its scope is **precisely defined** (see
  "What `Secret()` does and does not cover" below); it is **not** general encryption.
- `Answers` — read-only accessor: `Get(key) string` (returns `""` for a missing key) and
  `Lookup(key) (string, bool)` so a missing key is distinguishable from an empty answer.

**`HandleFlow` contract.** `HandleFlow(pattern string, flow *Flow) error` returns an error
when: the pattern is an invalid regexp; `flow.id` is empty; the flow has zero steps;
`Ask` keys are not unique; or no `OnComplete` is set. (`OnComplete` is required — a flow
that collects answers and does nothing with them is a programming error.) Duplicate `Ask`
keys are a **build-time error**, not last-wins. A second `HandleFlow` with the same
`flow.id` is also an error.

**What `Secret()` does and does not cover** (security-critical — make explicit):

- **Does:** suppress the answer from the framework's *own* logging/telemetry, and **omit
  the key from the serialized state written to any `Store`** (secret answers live only in
  volatile in-memory state and therefore do **not** survive a restart). This keeps
  passwords out of Redis-at-rest.
- **Does not:** encrypt anything; police user-installed middleware (which sees the raw
  `Message.Content`); or hide the answer from other members of a **public channel** — a
  user typing a password in a public channel exposes it regardless of `Secret()`.
  Secret-collecting flows should therefore be DM-only (see §4 channel scoping). All three
  caveats are documented at the call site.

Why this is the primary API: it is **declarative and linear** (zero state-machine code) and
its non-secret state is **serializable-ready** (the flat shape in §3),
so it adopts the persistent `Store` cleanly *when that ships*. **In v1 it is in-memory only
and does not survive restarts** — the choice of §1 over §2 rests on "linear use case with
zero state-machine code, and a state shape ready for durability later," **not** on durability
that exists today (see §2 tradeoff, §5).

### 2. Alternative API — imperative blocking `Ask` (documented, not v1)

For dynamic/branching dialogs, a script-style API reads best:

```go
bot.HandleFunc("^signup$", func(ctx context.Context, b *botbooter.Bot, m *botbooter.Message) {
    c := b.Conversation(m)                       // bound to this user+channel
    name  := c.Ask(ctx, "What's your name?")     // sends prompt, BLOCKS for the reply
    email := c.AskValid(ctx, "Email?", validEmail)
    if strings.HasSuffix(email, "@corp.com") {   // arbitrary Go control flow
        c.Say(ctx, "Corp account detected.")
    }
    c.Say(ctx, "Done.")
})
```

`c.Ask` blocks the handler goroutine until the engine routes the next message into a
channel it reads. Maximally ergonomic; supports loops/branches with plain Go.

**Tradeoff (why it is not v1):** the handler goroutine stays alive for the whole
conversation, so the state is *a live goroutine + channel* — **not serializable**, does not
survive a restart, needs a per-`Ask` timeout and `ctx` cancellation handling, and caps
in-flight conversations by goroutine budget. go-sarah deliberately avoids this.
Additionally, because incoming messages dispatch in their *own* goroutine (per the
background note), the engine delivers a message into the blocked handler's reply channel
**across goroutines** — so when implemented, that delivery MUST be non-blocking (buffered
channel or `select`-with-default) and the per-`Ask` timeout MUST **atomically clear pending
state before the handler returns**, or a late message sent into an abandoned channel will
block the per-event dispatch goroutine forever (goroutine leak).

**Recommendation:** ship **§1 (declarative `Flow`)** first. Keep **§2** as a future
*ephemeral* mode for branching dialogs, behind the same core routing primitive (§3),
explicitly documented as non-durable.

### 3. Core engine — dispatch hook + serialized start path

The engine change is **two** things, not one: a pending-conversation check at the top of
the inner handler, **and** a serialized flow-start path (start is *not* a loose write in
the command loop — see §4 for why that would race).

```go
handler := func(ctx context.Context, bot *Bot, message *Message) {
    if bot.conversations.advance(ctx, bot, message) {  // had pending state → handled
        return
    }
    for i := range bot.commands {
        if bot.commands[i].match(message.Content) {
            // For a HandleFlow command, the handler calls bot.conversations.start(...),
            // which records initial state and sends the first prompt UNDER the per-key
            // lock (set-if-absent), so two racing triggers cannot double-start.
            bot.commands[i].Handler(ctx, bot, message)
            return
        }
    }
    // …existing unknown-command fallthrough…
}
```

- The hook sits **inside** the middleware-wrapped inner handler, so the middleware chain
  still wraps every conversation step identically (logging/auth/metrics keep working). The
  command-matching loop and unknown-command fallthrough are unchanged when no flow is active;
  `HandleFlow` adds an ordinary entry to the command table (its handler delegates to
  `conversations.start`).
- `conversations` is a per-`Bot` manager over a **`ConversationStore`** interface
  (`Get/Set/Delete` by key) plus the per-key serialization described in §4. It owns
  **both** `start` and `advance`; both run under the same per-key lock. It is created in
  `New` (the default in-memory store), since `HandleFlow` registers flows *before* `Connect`;
  only the background sweeper (§4) is tied to the connection lifecycle.
- **Key = `UserID + "\x00" + ChannelID`** — per-user *and* per-channel (one user can run an
  independent flow in a DM and in a channel). See §4 for the public-channel implications.
- **Flow registry:** the `Bot` holds `map[flowID]*Flow`. `advance` reads the active state's
  `FlowID`, looks up the `*Flow`, and runs the current step. **If the looked-up `FlowID` is
  not registered** (e.g. a `Store`-backed state outlived a deploy that renamed/removed the
  flow), the engine treats the state as absent (reaps it) and falls through to the command
  table — it never panics. Persisted state is only meaningful when flows are re-registered
  with identical ids at startup; this is documented.
- **State shape:** `{ FlowID string; Step int; Answers map[string]string; ExpiresAt time.Time; Version uint64 }`
  — flat and serializable. Secret answers are excluded from the serialized form (§1).
  `Version` is unused by the single-instance v1 (in-memory locks suffice); it exists now so
  the eventual `Store`-backed multi-instance path (optimistic compare-and-swap, see
  [Deferred work](#deferred-work)) needs no state migration. The in-memory store still bumps
  it on every `Set` so the field is exercised and correct from day one.

### 4. Concurrency model (the load-bearing part)

Because dispatch is concurrent per conversation on Discord/Telegram/WhatsApp (background
note), this section is a correctness requirement, not a nicety. Implement exactly:

- **Per-key serialization via a fixed-size striped lock set.** The manager holds `N`
  mutexes (e.g. 256); a key hashes to one shard. This bounds memory **by construction** —
  there is no `map[key]*Mutex` that grows per distinct conversation and must be reaped.
  (Trade-off: two keys on the same shard serialize unnecessarily; with `N` large this is
  rare and brief. Accepted.) "Lock-free across keys" in the original draft is corrected to
  "shard-striped": independent conversations almost never contend.
- **Critical section = in-memory state transition + the user validator only.** `advance`:
  `Lock(shard)` (with `defer Unlock`) → `Get` state → re-check `ExpiresAt` under lock (if
  expired: `Delete`, release, run optional `OnTimeout` outside the lock, return handled) →
  run the cancel check → run the step **validator** → compute the next state → `Set` (and
  slide `ExpiresAt`) → release. Branches under the lock: on **validator error or empty
  answer**, do **not** `Set` (no advance, no store), release, then send the re-prompt; on
  **cancel**, `Delete` under the lock, release, then run `OnCancel`; on **last step**,
  `Delete` under the lock, release, then run `OnComplete`. **The prompt/re-prompt send and
  `OnComplete`/`OnCancel`/`OnTimeout` always run *outside* the lock** (they are user code
  and/or network I/O; never hold a lock across them). The validator runs *under* the lock and is documented as "keep it fast and
  non-blocking." `start` follows the same discipline (set-if-absent under the lock; send
  first prompt after releasing). **Set-if-absent loser:** if two triggers race and state
  already exists, the second `start` is a **no-op** — no duplicate first prompt — and the
  losing trigger message is **dropped, not consumed as the first answer**. (This is the
  concurrent *race* at start. The *sequential* case — a trigger arriving while a flow is
  already active — is a different path: it never reaches `start` because `advance` consumes
  it as input first, per [Deferred work](#deferred-work) item 7.)
- **`defer`-release is mandatory.** `dispatch`'s `recover` sits *above* `advance`; a panic
  in the validator must not leave the shard locked. `defer Unlock` guarantees release, and
  the panic still propagates up to the existing dispatch-level `recover`.
- **TTL reaping = lazy + background sweeper.** Lazy: `advance` treats expired state as
  absent (above). Background: a sweeper goroutine periodically deletes expired entries so a
  user who starts a flow and never returns does not leak a map entry forever. The sweeper
  takes the same shard lock per entry, so it cannot race `advance`. **Lifecycle:** the
  sweeper starts on `Connect` and stops on `Disconnect`, mirroring the existing
  per-connection lifecycle (`core.go` `Connect`/`Disconnect`); it must not outlive the
  connection (goroutine-leak test required). **Reconnect:** the store is per-`Bot`, so
  in-flight flow state **survives a transient `Disconnect`→reconnect** (a blip must not nuke
  half-finished forms); only background sweeping pauses for that window — lazy expiry still
  applies on the next `advance`. The sweeper restarts on the next `Connect`. (A full process
  exit still loses everything: in-memory store, §5.)
- **`ExpiresAt` slides on each successful step**, so a long but actively-progressing form
  (6-field signup) does not time out mid-fill. `Flow.Timeout` sets the per-step idle TTL
  (default 10m).
- **No deadlock with `Bot.mu`.** The conversation shards are a separate lock set; `advance`
  never acquires `Bot.mu`, and the lifecycle (`Bot.mu`) never acquires a shard.
- **Ordering is best-effort, not guaranteed.** A mutex gives mutual exclusion, not
  receive-order: on the concurrent adapters two in-flight messages may acquire the shard in
  reverse arrival order, so an answer *can* be matched to the wrong step if a user sends
  ahead of the prompt. v1 **guarantees no corruption / no crash / no lost or duplicated
  state**, and documents within-key ordering as best-effort. True ordering would require a
  per-key FIFO fed in receive order at the read loop — but these SDKs hand the engine
  events already inside per-event goroutines, so the engine cannot reconstruct arrival
  order on its own. Out of scope for v1; noted as a real limitation.
- **Single-instance only (hard constraint).** All of the above — the store and the striped
  locks — is **in-process**. Nothing crosses a process boundary, so running two replicas of
  the same bot breaks flows: instance A holds the pending state in its own memory and an
  answer routed to instance B finds none, falls through to B's command table, and the flow
  desyncs. The damage depends on how each platform fans messages to replicas (all verified
  from the adapters): **Slack** (per-replica socket) and **WhatsApp** (LB-split webhook)
  **split** delivery → answers ping-pong between instances; **Discord** (same token, two
  unsharded gateway connections) **duplicates** every event → both instances start and
  advance their own copy → double prompts / double `OnComplete`; **Telegram** (getUpdates
  long-poll) is **exclusive** → a second poller gets `409 Conflict` and cannot even start.
  v1 is therefore **single-instance by design**; multi-instance is a `Store`-era feature
  with its own concurrency model (see [Deferred work](#deferred-work)). This is the same
  constraint go-joe and go-sarah carry.

**Cancel / bail-out.** A per-`Flow` cancel word (default `"cancel"`, settable via
`Flow.CancelWord`, disableable with `""`) clears pending state and runs an optional
`Flow.OnCancel`. It lives on the `Flow` (not a per-`Bot` option) so this design needs **no
Option seam**. The cancel check precedes validation, so the configured word **shadows that
exact answer on every step** — a step that legitimately expects the word must disable or
change it per flow. Documented at the call site. Without a cancel escape a user is trapped
in a half-finished form, and during an active flow **all** global commands are shadowed
(that is the routing point) — so cancel/expiry are the only exits (see [Deferred work](#deferred-work)
for an optional command-allowlist).

**Channel scoping (public-channel footgun — document loudly).** Because the key is
`UserID+ChannelID` and an active flow shadows the command table, once a user triggers a flow
in a **public** channel, *every* message they post in that channel until completion or
timeout is consumed as a flow answer (`name = "help"`). Flows are therefore **DM-intended**;
the §1 signup example is only well-behaved in a DM, and a secret-collecting flow in public
also leaks the secret to other members. v1 **documents** this; a clean opt-in
(`Flow.DMOnly()` that refuses to start outside a DM, or mention-gating) requires a
cross-platform `Message.IsDirect` flag the `Message` type does not have yet — tracked in
[Deferred work](#deferred-work).

**Other edge cases.** Validation failure re-prompts the same step (never advance, never
store). The bot's own and other bots' messages are dropped by adapters before dispatch, so
the engine never feeds a prompt back into itself.

### 5. Persistence — in-memory now, Store-ready later

- v1 ships `memConversationStore` (striped map + the §4 sweeper + TTL) as the default,
  mirroring how go-sarah ships an in-memory `UserContextStorage`. No dependency on the
  roadmap `Store` landing first. **v1 loses in-flight flows on restart** — after a restart
  the next message finds no state and falls through to the command/unknown path.
- When the roadmap `Store` arrives, add `WithConversationStore(...)` (or let `WithStore`
  back it) so non-secret flow state persists across restarts. The serializable state shape
  in §3 was chosen for this; secret answers (§1) are deliberately excluded from the
  serialized payload, so a `Store` backend never receives plaintext passwords.
- **A shared `Store` is necessary but not sufficient for multi-instance.** It makes state
  *visible* to every replica, but the §4 in-process striped locks give zero cross-process
  mutual exclusion — two replicas would do concurrent read-modify-write on the same key.
  That step requires store-level atomicity (the `Version` CAS in [Deferred work](#deferred-work)),
  which is why multi-instance is gated on the Store work, not delivered by it.

### 6. Isolation / package placement & public surface

- Engine + `ConversationStore` + striped locks + sweeper live in **`internal/core`**. No
  platform SDK is touched, so the SDK-free guarantees and `isolation_deps_test.go` are
  unaffected.
- Public re-exports in root `botbooter` fall into three categories — get them right so the
  isolation/convention guards do not flag them:
  - **Type aliases** (free, identity): `Flow = core.Flow`, `Answers = core.Answers`,
    `AskOption = core.AskOption` (the option type returned by `Validate()`/`Secret()`).
  - **Function wrappers** (`var`/`func` over `core`): `NewFlow`, `Validate`, `Secret`.
  - **Methods that ride the aliases for free:** `Flow.Ask` / `OnComplete` / `OnCancel` /
    `CancelWord` / `Timeout`, and `Bot.HandleFlow` — these need no separate re-export.
- **Convention note:** re-exporting `NewFlow`/`Validate`/`Secret` adds *function* surface to
  `botbooter.go`, which CLAUDE.md currently scopes to "shared-type aliases, `BotType` consts
  and error sentinels only." The Option-seam spec already adds a `New` function, so the
  convention is loosening. **Decision:** extend the documented rule to permit
  constructor/builder function re-exports (real logic still lives in `internal/`), and
  update CLAUDE.md when this lands so the guards/reviewers expect it.

## Deferred work

Decided-but-deferred (not "open"); each reuses the v1 engine with no state migration.

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

## Acceptance criteria (for the eventual implementation)

- A consumer defines the sign-up form in §1 (with `NewFlow("signup")`) and it works on every
  adapter. The CLI adapter (no credentials, single serial channel) verifies the **happy-path
  ergonomics** end-to-end. It does **not** exercise the §4 concurrent-dispatch path (CLI and
  Slack are serial), so the concurrency guarantees below are proven by **store-level unit
  tests** that drive concurrent `start`/`advance` directly — not via any adapter.
- The engine change is exactly the §3 hook **plus the §4 serialized start/advance path**;
  middleware still wraps each step; command matching and the unknown-command path are
  unchanged when no flow is active.
- Validation re-prompts; empty/whitespace input re-prompts (not stored); `cancel` bails and
  runs `OnCancel`; abandoned flows expire (state reaped, **no goroutine leak**, proven by
  test); the TTL slides on each step; per-user+channel keying proven by test.
- **Concurrency (corrected):** under concurrent double-send, state is never corrupted,
  lost, or duplicated, and no key/shard deadlocks — proven by a race test. A
  **panic in a validator/`OnComplete`/`OnCancel` does not wedge the shard** (panicking-step
  test). Within-key *ordering* is asserted only as best-effort and is **not** claimed to be
  guaranteed (no false "can't desync" criterion).
- **Secret answers are excluded from the serialized state** (test: serialize a completed/
  in-flight state and assert no secret value appears); secret answers are absent from
  framework logs.
- An unregistered `FlowID` in loaded state falls through to the command table without
  panicking (test).
- Striped-lock memory is bounded (no per-key lock map): N fixed shards.
- Root `botbooter` stays SDK-free; `imports_test.go` / `isolation_deps_test.go` green;
  `make all` green.

## Suggested build order

Each step is independently `make all`-green.

1. `ConversationStore` interface + `memConversationStore`: striped locks, lazy + background
   sweeper (start/stop wired to a lifecycle hook), TTL with slide, `defer`-release
   discipline. Unit-tested in isolation, including the panic-doesn't-wedge and
   bounded-memory tests. **(This is the riskiest increment — the §4 discipline lives here,
   not in a later step.)**
2. Dispatch hook (§3) + state shape + flow registry + serialized `start`/`advance`; prove
   routing with a hand-built two-step state (test-only scaffolding replaced in step 3).
3. `Flow` builder + `HandleFlow` (with full contract validation) + `Answers` (§1);
   `Validate`, `Secret` (incl. serialization exclusion), `OnComplete`, `OnCancel`,
   `CancelWord`, `Timeout`; empty-answer handling; concurrency + panic + secret tests.
4. Root re-exports (§6 taxonomy) + a CLI example (`examples/v1` "signup", DM-equivalent) +
   docs (DM-intended caveat, ordering caveat, restart-needs-cancel, `Secret` scope).
5. (Later) `Store`-backed persistence; (later) §2 blocking `Ask`; (later) branching;
   (later) `Message.IsDirect` + `Flow.DMOnly`.
