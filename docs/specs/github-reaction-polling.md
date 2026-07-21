# Spec: GitHub reaction polling (opt-in OnReaction ingress)

Implements docs/ideas/github-reaction-polling.md. Branch `feat/github-reaction-polling`,
worktree `.claude/worktrees/github-reaction-polling`, branched from
`worktree-github-adapter-investigation` (carries the form-encoded webhook fix).

## Objective

Make `bot.OnReaction` fire for GitHub bots. GitHub ships no reaction webhook, so the
adapter gains an **opt-in poller**: when `Config.ReactionPollRepos` is non-empty,
`Connect` starts a goroutine that periodically diffs reactions on each repo's newest
issue comments and feeds new ones through the existing `deps.DispatchReaction` seam.
Zero-config behavior is unchanged. No `core` changes, no new dependencies.

User story: a GitHub issue-ops bot replies (threaded = same issue thread) when someone
reacts 👍 to one of its comments, using the same `OnReaction` handler code that works
on Slack/Discord/Telegram/WhatsApp.

## Tech Stack

Existing only: Go 1.25, `google/go-github/v88`, in-repo `internal/asserts` for tests.

## Commands

```
make all        # fmt + vet + lint + test-race — gate before done
make test       # go test ./...
make test-race  # race detector (poller is concurrency-sensitive)
go test -race -run TestPollReactions ./internal/github/
```

## Design

### Config additions (`internal/github/github.go`)

```go
// ReactionPollRepos lists repositories ("owner/name") whose newest issue
// comments are polled for emoji reactions. Empty (default) disables polling
// and OnReaction never fires. Each entry costs at least one API request per
// poll cycle.
ReactionPollRepos []string
// ReactionPollInterval is the delay between poll cycles; it defaults to 30s.
ReactionPollInterval time.Duration
// ReactionStore dedups reactions across poll cycles; defaults to an
// in-process store (restart forgets seen reactions). Provide a persistent
// implementation together with ReactionLookback to catch reactions added
// while the bot was down.
ReactionStore ReactionStore
// ReactionLookback widens the dispatch cutoff: reactions created after
// (Connect time - ReactionLookback) are dispatched if unseen. Default 0.
ReactionLookback time.Duration
```

Validation in `newAdapter`: each repo entry must be `owner/name` (non-empty halves,
single `/`) → else `fmt.Errorf("%w: ...", ErrMissingConfig)`-style error using new
sentinel `ErrBadRepo`. Negative interval/lookback → `ErrBadRepo`? No — negative
durations rejected via `ErrMissingConfig` wrap is wrong too; decide: negative values
rejected with `ErrBadRepo` renamed broader `ErrBadReactionConfig`. Zero interval →
30s default. Nil store → in-memory default constructed at `New` (survives reconnects,
so a reconnect does not re-reply).

### Store (one method, `internal/github/reactions.go`)

```go
// ReactionStore records which reaction IDs have been handled. MarkSeen is an
// atomic check-and-set: it records id and reports whether it was fresh
// (unseen until now). Implementations must be safe for concurrent use.
// Reaction IDs are globally unique across GitHub, so one store serves all
// polled repositories.
type ReactionStore interface {
	MarkSeen(ctx context.Context, id int64) (fresh bool, err error)
}
```

Default: unexported `memoryReactionStore` (map[int64]struct{} + sync.Mutex).

### Poll cycle (one goroutine for all repos, started by `Connect`)

Per repo each cycle:
1. `Issues.ListComments(owner, repo, 0, sort=created desc, PerPage=10)` — 1 call.
2. For each comment: skip if `reactions.total_count` equals cached count (in-memory
   `map[int64]int` in the poller, not the store); update cache. If count changed and
   is > 0 → `Reactions.ListIssueCommentReactions` (PerPage 100).
3. For each reaction: skip unless `CreatedAt > cutoff` (cutoff = Connect time −
   lookback); skip if `User.Type == "Bot"` or `User.ID == selfID` (mirror
   `isSelfOrBot`); skip unless `store.MarkSeen` returns fresh (store error → log
   Warn, skip dispatch — conservative, no double-reply risk).
4. Dispatch: `a.inflight.Add(1); go func(){ defer a.inflight.Add(-1);
   deps.DispatchReaction(detachedCtx, r) }()` — same detached-context + drain
   pattern as the webhook path, so Disconnect's drain covers reaction handlers.

Lifecycle: first cycle runs immediately after Connect (before first tick); loop
selects on `ctx.Done()` (the Connect run context) vs `ticker.C`. API list calls use
`ctx`, so teardown aborts an in-flight poll. Poll errors are logged via `a.log()`
and the loop continues — a failing poll must not kill the connection (`deps.Done`
is never called from the poller); the webhook half keeps working.

MarkSeen is called for every fresh-looking reaction *after* the cutoff/bot filters,
so old reactions are never inserted into the store (keeps persistent stores small);
the CreatedAt cutoff is what prevents startup replay, replacing the demo's silent
baseline cycle.

### Mapping (`toReaction`)

```go
core.Reaction{
	Emoji:      reactionEmoji[content] or content,  // "+1"→"👍" … unicode renders as-is in GitHub markdown
	UserID:     strconv.FormatInt(user.GetID(), 10), // numeric, mirrors toMessage
	AuthorName: user.GetLogin(),
	ChannelID:  "owner/repo#N",                      // N parsed from comment.IssueURL (last path segment)
	MessageID:  strconv.FormatInt(comment.GetID(), 10),
	Raw:        &ReactionPayload{Reaction: reaction, Comment: comment},
}
```

`ReactionPayload` is the typed Raw carrier (pattern: `Message`); accessor
`RawReaction(r *core.Reaction) (*ReactionPayload, bool)` in internal, re-exported in
public `github` package alongside `ReactionStore` alias.

## Project Structure

```
internal/github/reactions.go       → store iface + default, poller, mapping, RawReaction
internal/github/reactions_test.go  → poller tests against httptest GitHub API stub
internal/github/github.go          → Config fields, validation, poller start in Connect (server.go)
github/github.go                   → ReactionStore/ReactionPayload aliases, RawReaction wrapper
github/wrapper_test.go             → accessor round-trip
_examples/reactions/main.go        → github case gains GITHUB_REPO → ReactionPollRepos; caveat text updated
_examples/github-reactions/        → RETIRED (deleted) — feature subsumes the demo
CLAUDE.md                          → reactions section: GitHub polled ingress note
```

## Code Style

Match adapter conventions exactly: doc comments explain constraints not mechanics,
errors `%w`-wrapped sentinels checked with `errors.Is`, locking commented with what
mu protects, `asserts` helpers in tests. Example:

```go
// pollOnce diffs one repository's newest comments against the count cache and
// dispatches reactions not yet seen. Errors abort the repo's cycle, not the
// poller: GitHub hiccups and rate limits must not tear down the connection.
func (a *adapter) pollOnce(ctx context.Context, owner, repo string, ...) error
```

## Testing Strategy

Hermetic, in `internal/github/reactions_test.go`, `httptest.Server` faking the GitHub
REST API (override `client.BaseURL`); `asserts` style; race-clean. Cases:

1. Poll dispatches a new reaction → `core.Reaction` fields all mapped (emoji unicode,
   ChannelID from IssueURL, numeric UserID).
2. CreatedAt before cutoff → no dispatch, store untouched.
3. Seen reaction (MarkSeen not fresh) → no dispatch.
4. Bot-type / selfID reactor → dropped.
5. Count cache: unchanged total_count → no detail API call (assert stub hit counts).
6. API error → logged, poller continues next cycle; deps.Done never called.
7. Teardown: ctx cancel stops poller; dispatched handler counted by inflight drain.
8. Config validation: bad repo entry / negative durations → New fails; empty repos →
   no poller goroutine.
9. Unknown reaction content falls back to raw content string.
10. Accessor round-trip (public wrapper_test).

## Boundaries

- **Always:** `make all` before done; keep `botbooter.go` SDK-free; hermetic tests;
  sweep sibling webhook adapters only if a shared-scaffolding bug is touched.
- **Ask first:** any `core` change (none expected); new dependency (none expected);
  changing existing webhook behavior.
- **Never:** commit `_examples/botbooter-test.2026-07-12.private-key.pem` or any
  secret; delete failing tests; widen `core.Adapter`.

## Success Criteria

- [ ] `github.New` with `ReactionPollRepos` set → reactions on newest comments reach
      `bot.OnReaction` within one poll interval (proven by hermetic test).
- [ ] Zero-config `github.New` behavior byte-identical to today (no poller goroutine).
- [ ] Steady-state API cost: 1 list call per repo per cycle when no counts changed.
- [ ] No `core` diff; no new module requirements; isolation tests updated for the
      new package's deps and green.
- [ ] `make all` green (fmt, vet incl. _examples, lint, test-race).
- [ ] `_examples/reactions github` works with `GITHUB_REPO` set; `_examples/github-reactions` removed.
- [ ] CLAUDE.md reactions paragraph updated (GitHub: polled, opt-in, added-only).

## Resolved Decisions (spec gate, 2026-07-12)

1. New sentinel `ErrBadReactionConfig` for malformed reaction config (bad repo entry,
   negative durations) — fields present but malformed is not "missing".
2. `_examples/github-reactions` is deleted; `_examples/reactions` github case gains
   `GITHUB_REPO` → `ReactionPollRepos`.
3. Typed Raw carrier is named `ReactionPayload`.
4. Verified: go-github v88 `Reaction` carries `CreatedAt *Timestamp` — the cutoff
   design is implementable as specced.
5. (2026-07-21) The poller additionally requires an `OnReaction` handler
   registered before connect: `core.AdapterDeps` gained `HasReactionHandlers`
   (snapshot at Connect), and the GitHub adapter starts no poller — and spends
   no API requests — when it is false, even with `ReactionPollRepos` set. This
   supersedes the original "No `core` diff" success criterion.
6. (2026-07-21) Reaction dedup is in-memory only for v1: the pluggable
   `ReactionStore` interface and `Config.ReactionStore`/`Config.ReactionLookback`
   are dropped in favor of an unexported in-process store hardwired at `New`.
   Lookback goes with the store — without persistence it only replays recent
   reactions on every restart. This supersedes the store/lookback design above;
   re-introduce the seam if a persistent-store consumer ever materializes.
