# GitHub Adapter Design

**Date:** 2026-07-03
**Status:** Approved design, pre-implementation
**Library:** `github.com/lao/botbooter` (pre-1.0)

## Motivation

botbooter runs the same regex-command bot on Slack, Discord, Telegram, WhatsApp, Teams, and a local CLI. GitHub "issue-ops" bots — bots that respond to `/deploy`, `/label`, `/retest`-style commands typed as issue or PR comments — are the same shape of program: inbound text events, regex dispatch, text replies. A GitHub adapter lets a consumer point their existing command set at a repository with only a constructor swap, exactly as they would swap `slack.New` for `teams.New`.

The repo already has two webhook adapters (WhatsApp, Teams) whose scaffolding — own `http.Server`, signature verification, ack-then-dispatch, in-flight drain — is deliberately duplicated per adapter. GitHub becomes the third copy of that pattern, mirrored byte-for-byte where the concerns are shared (per CLAUDE.md: duplication buys independence, but shared correctness concerns must not drift).

## Goals & Non-Goals

### Goals

- New platform: `internal/github` implementing `core.Adapter`, plus a thin public `github/` facade returning `*botbooter.Bot`.
- Inbound surface: **`issue_comment` webhook events with `action == "created"` only** — comments on issues and on PR conversation threads (GitHub delivers both under `issue_comment`).
- Transport: adapter-owned webhook HTTP server, same lifecycle as WhatsApp/Teams (`Connect` binds, `Disconnect` shuts down + drains). No polling.
- Inbound auth: HMAC-SHA256 verification of `X-Hub-Signature-256` against a configured webhook secret, via `github.ValidateSignature` over an explicitly read body.
- Outbound: post issue comments via the go-github SDK (`Issues.CreateComment`), which works identically for issues and PRs.
- Outbound auth: **both** PAT and GitHub App, resolved at the `http.RoundTripper` level behind a single `*github.Client` (approach A). One `Config`; constructor errors unless exactly one auth mode is configured.
- Reply-loop prevention: drop bot-authored comments (`comment.user.type == "Bot"`) and the adapter's own comments (self-ID check).
- Isolation guards: `github/imports_test.go` SDK ban + present/absent rows in root `isolation_deps_test.go`, so consumers of other platforms never pull go-github into their build graph.

### Non-Goals (v1)

- **No attachments.** `Attachments` returns `(nil, nil)`; the adapter does not implement `core.AttachmentResolver`. Issue comments carry markdown, not an upload channel worth modeling in v1.
- Not handled: PR *review* comments (`pull_request_review_comment`), discussions, issue/PR bodies (`issues` / `pull_request` events), `edited`/`deleted` comment actions, reactions.
- No delivery dedupe on `X-GitHub-Delivery` (redeliveries are rare and handlers should be idempotent; revisit if it bites).
- No GitHub Enterprise base-URL support (go-github's `WithEnterpriseURLs` makes this a small later addition).

## Architecture

### Packages touched

| Path | Change |
|---|---|
| `internal/core/core.go` | Append `GitHubBotType` to the `BotType` iota block (end of list — values are ordinal) and add `case GitHubBotType: return "github"` to `String()`. No lifecycle/dispatch changes. |
| `internal/github/` | New: `github.go` (Config, adapter, `New`, accessors), `server.go` (Connect/Disconnect/handler/drain), `send.go` (Send, channelID parsing), `message.go` (typed `Message`, `RawEvent`, `toMessage`), tests. The split mirrors the Teams layout. |
| `github/` | New public facade: `github.go` (aliases, `New`, `Client`, `RawEvent`, `Addr`), `imports_test.go`, `wrapper_test.go`. |
| `botbooter.go` | Re-export `GitHubBotType = core.GitHubBotType`. Stays SDK-free. |
| `isolation_deps_test.go` | Add `"github"` to `allPlatforms`; add a case row for `github.com/lao/botbooter/github`; add the new third-party modules (`google/go-github`, `ghinstallation`) to every other platform's `absent` list. Use version-agnostic substrings (e.g. `"google/go-github"`, not `.../v88`) so a major bump doesn't silently unguard — note ghinstallation pins its own go-github major, so the github row's `present` closure may contain two go-github majors. |
| `{cli,slack,discord,telegram,whatsapp,teams}/imports_test.go` + root `imports_guard_test.go` | go-github is a platform SDK (unlike golang-jwt, a crypto lib the Teams guards deliberately exclude), so the **direct-import** guard layer must learn it too: append the version-agnostic substring `"google/go-github"` to the `CheckBannedImports` banned list in all six sibling public packages and in the root guard. Without this, the direct-import half of the isolation mechanism named in CLAUDE.md would silently not cover the new SDK. |
| `go.mod` | Add `github.com/google/go-github/v88` and `github.com/bradleyfalzon/ghinstallation/v2`. |

### Config

```go
// internal/github/github.go
type Config struct {
        // Auth — exactly one mode must be set.
        Token string // PAT mode: a personal access token / fine-grained token.

        AppID          int64  // App mode: GitHub App ID.
        InstallationID int64  // App mode: installation ID (also visible in webhook payloads).
        PrivateKey     []byte // App mode: the App's RSA private key, PEM-encoded.

        // Inbound webhook.
        WebhookSecret string // HMAC secret for X-Hub-Signature-256. Required.
        Addr          string // Listen address, e.g. ":8080", "8080", "127.0.0.1:0". Required.
        Path          string // Webhook path. Default "/webhook"; "/" prepended if missing.

        // HTTPClient is the base client for outbound GitHub API calls.
        // Defaults to &http.Client{Timeout: 30 * time.Second}. In App mode its
        // Transport (or http.DefaultTransport when nil) becomes the inner
        // transport of the ghinstallation transport.
        HTTPClient *http.Client
}
```

Validation and normalization in `newAdapter(cfg)` (mirrors `whatsapp.go:153-174` / `teams.go:118-135`):

- `var ErrMissingConfig = errors.New("github: missing required config field")`; `var ErrAmbiguousAuth = errors.New("github: configure either Token or AppID/InstallationID/PrivateKey, not both")`.
- PAT mode = `Token != ""`. App mode = any of `AppID`, `InstallationID`, `PrivateKey` set (then all three required, else `ErrMissingConfig`). Both modes set → `ErrAmbiguousAuth`. Neither → `ErrMissingConfig`.
- `WebhookSecret` and `Addr` required → `fmt.Errorf("%w: WebhookSecret and Addr are required", ErrMissingConfig)`.
- Bare-port `Addr` (`strconv.Atoi` succeeds) → prefix `":"`. Empty `Path` → `defaultPath = "/webhook"`; missing leading `/` → prepend (a pattern without one panics ServeMux at Connect).
- Default `HTTPClient` as above.

### Auth modes → one `*github.Client`

`newAdapter` builds the client once; the rest of the adapter is auth-agnostic:

- **PAT:** `github.NewClient(github.WithHTTPClient(cfg.HTTPClient), github.WithAuthToken(cfg.Token))`.
- **App:** ghinstallation does **not** nil-check its inner transport (`Transport.RoundTrip` and `AppsTransport.RoundTrip` call `t.tr.RoundTrip` directly — a nil RoundTripper panics), and the default `HTTPClient` above has a nil `Transport`, so `newAdapter` must normalize explicitly:

  ```go
  tr := cfg.HTTPClient.Transport
  if tr == nil {
          tr = http.DefaultTransport
  }
  itr, err := ghinstallation.New(tr, cfg.AppID, cfg.InstallationID, cfg.PrivateKey)
  ```

  then `github.NewClient(github.WithHTTPClient(&http.Client{Transport: itr, Timeout: cfg.HTTPClient.Timeout}))`. The same normalized `tr` is what Connect passes to the one-shot `ghinstallation.NewAppsTransport` used for self-identity (store `tr`, or recompute identically). ghinstallation caches the ~1h installation token and refreshes it inside `RoundTrip`; the adapter never touches token lifecycle.

Note: go-github v88's `NewClient` is functional-options and returns `(*Client, error)` — the old `NewClient(nil)` two-arg style no longer exists; do not copy pre-v73 examples.

For test repointing (the `baseURL`/`tokenURL` pattern in the siblings), the adapter keeps the built `*github.Client` as a field `client *github.Client`; tests construct it against an `httptest.Server` via `github.WithURLs`.

### Adapter struct

```go
type adapter struct {
        cfg    Config
        client *github.Client

        mu             sync.Mutex
        selfID         int64  // numeric user ID of the bot's own account; 0 = unresolved
        selfLogin      string // login of the bot's own account
        srv            *http.Server
        boundAddr      string // set with srv, cleared with it; enables Addr ":0"
        detachedCancel context.CancelFunc

        inflight atomic.Int64
}
```

`selfID`/`selfLogin` are **mu-guarded** like `srv`: Connect writes them (under `a.mu`, in the same critical section that installs `srv`) and the handler's loop-prevention check reads them under `a.mu`. The mutex — not goroutine-start ordering — is the happens-before edge, so handler goroutines still draining from a superseded connection can never observe a torn write during a reconnect. They are **not cleared on Disconnect** and are **re-resolved on every Connect** (a reconnect may follow a token/App credential change); between Disconnect and the next Connect the stale values are harmless because no server is accepting requests. If `selfID` is 0 (unresolvable in practice, since Connect fails rather than install a server without it), the self-ID comparison never matches and the `type == "Bot"` check remains the only guard.

Constants mirror the siblings: `maxRequestBytes = 1 << 20` (public endpoint; cap bodies against memory exhaustion), `shutdownTimeout = 5 * time.Second`, `drainTimeout = 5 * time.Second`.

`New(cfg Config) (*core.Bot, error)` = `newAdapter(cfg)` then `core.New(core.GitHubBotType, a)`. The server does **not** start in `New` — only on `Connect` (core contract; also matches WhatsApp/Teams).

### Connect

Identical skeleton to `whatsapp.go:177-233` / `teams/server.go:40-89`:

1. `detachedCtx, detachedCancel := context.WithCancel(context.WithoutCancel(ctx))` — the per-connection dispatch context: `WithoutCancel` lets an already-acked dispatch finish during shutdown drain; `WithCancel` lets Disconnect abort stragglers after the drain.
2. **Self-identity resolution**, using `ctx` (Connect is allowed brief startup I/O before returning):
   - PAT mode: `client.Users.Get(ctx, "")` → resolve `selfID`/`selfLogin`.
   - App mode: an installation token cannot call `GET /user` or `GET /app`. Build a one-shot `ghinstallation.NewAppsTransport(...)` client (over the same normalized inner transport as the main client — never a nil RoundTripper), call `Apps.Get(ctx, "")` for the app `slug`, then `client.Users.Get(ctx, slug+"[bot]")` for the numeric bot-user ID.

   **Any self-identity resolution failure — in either mode — → `detachedCancel()` and return the error** (a bot that can't recognize itself is a reply-loop hazard; fail loudly at startup, not silently at dispatch). The resolved values are held locally and written to `a.selfID`/`a.selfLogin` under `a.mu` in step 6, alongside `srv`.
3. `mux := http.NewServeMux()`; single `mux.HandleFunc(a.cfg.Path, ...)`; POST only, else 405 (GitHub webhooks are always POST — no GET handshake, unlike WhatsApp).
4. `net.Listen("tcp", a.cfg.Addr)`; on error `detachedCancel()` and return.
5. `&http.Server{Handler: mux, ReadHeaderTimeout: 10s, ReadTimeout: 20s, WriteTimeout: 20s, IdleTimeout: 60s}`.
6. Under `a.mu`: `a.selfID, a.selfLogin = resolvedID, resolvedLogin; a.srv = srv; a.boundAddr = ln.Addr().String(); a.detachedCancel = detachedCancel`.
7. `go srv.Serve(ln)`; a non-`http.ErrServerClosed` error → `deps.Done(err)`.
8. Watcher goroutine: `<-ctx.Done()`; under lock compare `a.srv == srv` (identity — a stale watcher from a superseded connection must not tear down its replacement); only if current, `_ = deps.Disconnect()`.
9. Return nil (non-blocking, per core contract; this adapter falls under the `watcherAdapter` shape in `internal/core/lifecycle_contract_test.go`).

### Inbound flow (handler)

`handleWebhook(dispatchCtx context.Context, w http.ResponseWriter, r *http.Request, deps core.AdapterDeps)`:

1. Read the body explicitly, then verify — two steps with two distinct failure codes, matching the siblings (`internal/whatsapp/whatsapp.go:248-254`, `internal/teams/server.go:98-121`):
   - `payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))` — read error (including oversize, which MaxBytesReader surfaces as a read error) → **400** and return.
   - `github.ValidateSignature(r.Header.Get("X-Hub-Signature-256"), payload, []byte(a.cfg.WebhookSecret))` — constant-time HMAC-SHA256 compare; failure or absent header → **403** and return. (Deliberately *not* the one-shot `github.ValidatePayload`, whose single undifferentiated error cannot distinguish an unreadable body from a bad signature; no legacy SHA-1 fallback — the hook is ours to configure with SHA-256.)
2. `github.WebHookType(r)`; anything other than `"issue_comment"` → 200 and return (GitHub sends `ping` and whatever else the hook is subscribed to; unknown events are not errors).
3. `github.ParseWebHook("issue_comment", payload)` → `*github.IssueCommentEvent`. Parse error → log, 200, return (a malformed body from an authenticated GitHub is not the sender's problem; never 500 on parse, matching WhatsApp).
4. Filter, then ack, then dispatch:
   - `event.GetAction() != "created"` → drop.
   - Loop prevention (below) → drop.
   - `w.WriteHeader(http.StatusOK)` **before** dispatching — GitHub times out slow hook deliveries; the ack must not wait on handler execution.
   - `a.inflight.Add(1); go func() { defer a.inflight.Add(-1); deps.Dispatch(dispatchCtx, msg) }()` — the increment lands before `Shutdown` returns, so `drainDispatch` always observes it. `dispatchCtx` is the detached context, not `r.Context()`.

Dropped events are also acked 200 (GitHub disables hooks that error persistently).

### Loop prevention

Belt-and-braces, applied to the *comment author*:

- `event.GetComment().GetUser().GetType() == "Bot"` → drop. This silences all GitHub Apps (including this bot in App mode, whose author is `<slug>[bot]` with type `"Bot"`), matching how Slack (`isBotMessage`) and Discord (`Author.Bot`) drop other bots wholesale.
- `event.GetComment().GetUser().GetID() == selfID` (read from `a.selfID` under `a.mu`, per the adapter-struct rules above) → drop. This is what actually protects **PAT mode**, where the bot's own comments arrive as `type == "User"` under the token owner's account; the numeric-ID compare is the robust check (logins can be renamed).

### Send flow

`Send(ctx context.Context, channelID, text string) error` (`send.go`):

- ChannelID encoding: **`"owner/repo#123"`**. Parse with a `parseChannelID(channelID) (owner, repo string, number int, err error)` helper: split on the last `#`, require a positive integer after it, require exactly one `/` in the left part with non-empty owner and repo. Malformed → `fmt.Errorf("%w: %q", ErrBadChannelID, channelID)` with `var ErrBadChannelID = errors.New("github: channel ID must be \"owner/repo#number\"")`.
- `_, resp, err := a.client.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{Body: github.Ptr(text)})`. This posts to `POST /repos/{owner}/{repo}/issues/{number}/comments`, which works for both issues and PRs (PRs are issues for commenting purposes).
- go-github returns typed errors with the response embedded; wrap with `fmt.Errorf("github: create comment on %s: %w", channelID, err)`. No retry logic in v1 — GitHub's content-creation secondary limit (80 comment-creating requests/min) surfaces as 403/429 with `Retry-After`, and backoff policy belongs to the consumer for now (see Open questions).

`Disconnect` and `Attachments` complete the `core.Adapter` interface:

- `Attachments(m *core.Message) ([]core.Attachment, error)` → `return nil, nil` (explicit non-goal).

### Disconnect + drain

Byte-for-byte the sibling pattern (`whatsapp.go:277-337` / `teams/server.go:149-208`):

1. Under lock, snapshot `srv := a.srv; cancelDispatch := a.detachedCancel`; `srv == nil` → return nil (idempotent).
2. `srv.Shutdown(shutCtx)` under its **own** 5s budget, then `a.drainDispatch(drainCtx)` under a **separate** 5s budget — a slow Shutdown must not consume the drain deadline and drop an already-acked message.
3. `a.inflight.Load() > 0` after the drain → log and set `drainErr = fmt.Errorf("github: dispatch drain timed out with %d in-flight dispatch(es)", n)`.
4. Under lock, clear `srv`/`boundAddr`/`detachedCancel` **only if `a.srv == srv`** (a reconnect during the up-to-10s Disconnect must not be clobbered).
5. `cancelDispatch()` unconditionally after the drain (a stuck handler must not leak); return the Shutdown error if any, else `drainErr`.
6. `drainDispatch` polls `a.inflight` every 20ms bounded by its context — deliberately not a `WaitGroup` (an `Add` racing `Wait` risks a misuse panic).

## Message mapping

`internal/github/message.go` defines the typed raw message and the mapping into `core.Message`:

```go
// Message is the typed raw payload stored in core.Message.Raw.
type Message struct {
        Event *github.IssueCommentEvent
}

func RawEvent(m *core.Message) (*Message, bool) {
        v, ok := m.Raw.(*Message)
        return v, ok
}
```

`toMessage(event *github.IssueCommentEvent) *core.Message`:

| `core.Message` field | Source | Example |
|---|---|---|
| `ID` | `strconv.FormatInt(event.GetComment().GetID(), 10)` | `"1234567890"` |
| `UserID` | `strconv.FormatInt(event.GetComment().GetUser().GetID(), 10)` | `"58394"` |
| `AuthorName` | `event.GetComment().GetUser().GetLogin()` | `"octocat"` |
| `ChannelID` | `event.GetRepo().GetFullName() + "#" + strconv.Itoa(event.GetIssue().GetNumber())` | `"lao/botbooter#42"` |
| `Content` | `event.GetComment().GetBody()` | `"/deploy staging"` |
| `Timestamp` | `event.GetComment().GetCreatedAt().Time` | — |
| `Raw` | `&Message{Event: event}` | — |

`ChannelID` round-trips through `Send` by construction: what dispatch hands the handler is exactly what `parseChannelID` accepts, so `bot.Send(ctx, msg.ChannelID, "done")` replies on the same issue/PR. A consumer can distinguish PR comments from issue comments via `RawEvent`: `ev.Event.GetIssue().IsPullRequest()` (the payload's `issue.pull_request` field).

## Public API

`github/github.go` (facade, template: `teams/teams.go`):

```go
package github // import "github.com/lao/botbooter/github"

type Config = githubint.Config   // githubint = internal/github
type Message = githubint.Message

var (
        ErrMissingConfig = githubint.ErrMissingConfig
        ErrAmbiguousAuth = githubint.ErrAmbiguousAuth
        ErrBadChannelID  = githubint.ErrBadChannelID
)

// New returns a Bot that serves a GitHub webhook and replies as issue comments.
func New(cfg Config) (*botbooter.Bot, error)

// RawEvent returns the typed issue_comment event behind a dispatched message.
func RawEvent(m *botbooter.Message) (*Message, bool)

// Client returns the underlying go-github client, or nil for a non-GitHub bot.
func Client(b *botbooter.Bot) *github.Client // github = google/go-github/v88/github

// Addr returns the webhook server's bound address ("" when not connected).
// It resolves the OS-assigned port when Config.Addr is ":0".
func Addr(b *botbooter.Bot) string
```

`Client` and `Addr` recover the adapter via `core.AdapterAs[*githubint.adapter](b)` — implemented as package-level accessor funcs in `internal/github` (`func Client(b *core.Bot) *github.Client`, `func Addr(b *core.Bot) string`, the latter returning the mutex-guarded `boundAddr`) that the facade wraps, matching `whatsapp.go:132-139`.

Root package: `botbooter.go` gains `GitHubBotType = core.GitHubBotType` in the const block; nothing else.

Naming note: the public package is `github`, which collides with the SDK package name inside the facade's own files — the facade imports the SDK as `gogithub` (or the internal package as `githubint`) locally. Consumers are unaffected: they import `botbooter/github` and never both.

## Error handling

- **Constructor** (`New`/`newAdapter`): sentinel-wrapped errors, checked with `errors.Is` — `ErrMissingConfig` (missing WebhookSecret/Addr, incomplete App triple, no auth mode), `ErrAmbiguousAuth` (both modes). ghinstallation key-parse errors are returned wrapped, not swallowed.
- **Connect**: listen failure and self-identity resolution failure return the error directly (after `detachedCancel()`); core surfaces it to the caller of `bot.Connect`/`Run`.
- **HTTP handler**: 400 on unreadable/oversized body (the `MaxBytesReader` read step), 403 on signature failure (the `ValidateSignature` step), 405 on non-POST, 200 for everything else including unknown events, non-`created` actions, filtered bots, and parse failures (logged). Never 500 on payload content.
- **Send**: `ErrBadChannelID` for malformed channel IDs; API errors wrapped with the `github:` prefix and channelID context, `%w`-chained so callers can unwrap `*github.ErrorResponse` / `*github.RateLimitError` / `*github.AbuseRateLimitError` from go-github.
- **Disconnect**: Shutdown error takes precedence, then the drain-timeout error; nil when never connected.
- **Dispatch panics**: core already recovers and logs; the adapter adds nothing.

## Testing strategy

All hermetic by default; `internal/asserts` helpers only (`Equal`, `NoError`, `ErrorIs`, `True`, …), no testify. Everything must pass `make all` (fmt+vet+lint+test-race).

- **Config/constructor** (`internal/github`): table tests for validation — missing secret/addr, PAT+App both set → `ErrAmbiguousAuth`, partial App triple → `ErrMissingConfig`, bare-port Addr normalization, default and slash-prepended Path, default HTTPClient; App mode with the default (nil-Transport) HTTPClient constructs successfully and the built client's transport chain bottoms out in `http.DefaultTransport`, not nil (guards the ghinstallation nil-RoundTrip panic).
- **Handler in-process, no server** (WhatsApp pattern, `whatsapp_test.go:183-294`): `httptest.NewRequest` POSTs with hand-signed `X-Hub-Signature-256` headers (an `hmac.New(sha256.New, secret)` helper over canned payloads) into `a.handleWebhook(dispatchCtx, httptest.NewRecorder(), r, deps)` with `core.AdapterDeps{Dispatch: ...}` closures capturing channels + a 2s `awaitDispatch` helper. Canned package-level const JSON payloads: `issueCommentCreated`, `prCommentCreated` (with `issue.pull_request` set), `commentEdited`, `botAuthoredComment`, `selfAuthoredComment` (PAT-shape, `type:"User"`, matching selfID), `pingEvent`. Assert: valid signature + created → dispatch with correctly mapped `core.Message` (all seven fields, ChannelID `"owner/repo#N"`); bad/absent signature → 403, no dispatch; GET → 405; `edited` action, bot author, self author, `ping` → 200, no dispatch; malformed JSON → 200, no dispatch; oversized body → 400, no dispatch. Handler-level self-author cases inject identity directly: the test constructs the adapter via `newAdapter(cfg)` and sets `a.selfID`/`a.selfLogin` under `a.mu` before calling `handleWebhook` — no `Connect` (and hence no self-identity network fetch) is involved.
- **Send** (`httptest.NewServer` + `github.WithURLs` pointing the client at it): asserts the request path `/repos/owner/repo/issues/42/comments`, the JSON body, and error wrapping on 403/500 responses; `parseChannelID` table test including `"owner/repo"`, `"owner#1"`, `"owner/repo#0"`, `"owner/repo#abc"`, `"a/b#12"` cases.
- **Auth-mode wiring**: PAT mode test asserts the `Authorization` header the client sends against a local httptest server. App mode test asserts `newAdapter` builds a ghinstallation transport from a test RSA key (generate with `rsa.GenerateKey` in-test, PEM-encode) — token *refresh* behavior is ghinstallation's job, not re-tested here; the Connect-time self-identity fetch is served by the same httptest server (`/app`, `/users/slug%5Bbot%5D`, `/user`).
- **Lifecycle** (WhatsApp pattern, `whatsapp_test.go:490-559, 618-668`): bind `Addr: "127.0.0.1:0"`, call `a.Connect(ctx, deps)`/`a.Disconnect()` directly. Cover: Addr resolves then clears; Disconnect idempotent and nil when never connected; bind error surfaces; stale watcher ignores a replaced server; detached ctx canceled after drain; slow-Shutdown-doesn't-starve-drain. A real-time ~5s drain-timeout test, if written, is env-gated behind `BOTBOOTER_GITHUB_DRAIN_TIMING_TEST` (matching the WhatsApp gate).
- **Facade** (`github/wrapper_test.go`, template `teams/wrapper_test.go`): `TestNew` asserts `bot.BotType == botbooter.GitHubBotType` (needs a Config valid without network — self-identity runs at Connect, not New, so `New` needs no server); `TestNewMissingConfig` asserts `ErrorIs(err, ErrMissingConfig)`; `TestRawEvent` round-trips `&botbooter.Message{Raw: want}`.
- **Isolation**: `github/imports_test.go` calls `asserts.CheckBannedImports(t, ".", []string{"discordgo", "slack-go/slack", "go-telegram/bot"}, "github")` (direct-import ban of *other* SDKs). The **existing** direct-import guards must gain the new SDK: append `"google/go-github"` (version-agnostic — survives major bumps) to the banned lists in `cli/`, `slack/`, `discord/`, `telegram/`, `whatsapp/`, `teams/` `imports_test.go` and the root `imports_guard_test.go`. Root `isolation_deps_test.go`: `"github"` added to `allPlatforms`; new row `{pkg: ".../github", present: []string{gogithub, ghinstallation}, absent: [all other SDKs], internalOwn: "github"}`; `gogithub` and `ghinstallation` appended to every other platform's `absent` list (the jwtv5 precedent, lines 41-45), again as version-agnostic substrings — ghinstallation additionally pulls its own pinned go-github major into the github closure, which a versioned string would miss. Note ghinstallation depends on `golang-jwt/jwt` — Teams' row already lists jwtv5 as `present`, and the GitHub row must expect whichever jwt major ghinstallation pulls; verify with `go mod graph` at implementation time and encode the actual module path.

## Dependencies

| Module | Version | Why |
|---|---|---|
| `github.com/google/go-github/v88` | v88.0.0 | Webhook validation/parsing (`ValidateSignature`, `ParseWebHook`, `WebHookType`), typed `IssueCommentEvent`, `Issues.CreateComment`, `Apps.Get`/`Users.Get` for self-identity. User explicitly chose the SDK over plain HTTP. |
| `github.com/bradleyfalzon/ghinstallation/v2` | v2.19.0 | App-mode `http.RoundTripper` with cached, auto-refreshing installation tokens; `AppsTransport` for the one-shot `GET /app` slug lookup. Handles App JWTs internally, so no direct `golang-jwt` import in this adapter. |

Both enter only the `internal/github` (and facade `github/`) build graph; the isolation tests enforce that no other public package pulls them. go-github cuts majors every few weeks — the version is pinned in `go.mod` and bumping is a routine chore, not a design concern.

## Open questions

1. **Retry/backoff on content-creation rate limits.** GitHub caps comment creation at 80/min, 500/hr; a chatty bot will see 403/429 with `Retry-After`. v1 surfaces the error to the handler. If real deployments hit it, decide then between adapter-side single-retry-with-`Retry-After` (Teams' 401 self-heal precedent) and leaving policy to consumers — leaning consumer-side, per the repo's simplicity preference.
2. **Command trigger ergonomics.** On busy repos every comment hits dispatch; regex no-matches fall to the unknown-command handler if set, which may be noisy for GitHub specifically (most comments aren't commands). Consumers can simply not set an unknown-command handler, so v1 ships nothing — but a `Config.CommandPrefix` pre-filter is the likely first feature request. Deliberately deferred, not designed.
