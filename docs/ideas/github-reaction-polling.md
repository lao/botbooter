# GitHub reaction polling in adapter

## Problem Statement

How might we make `bot.OnReaction` fire on GitHub — a platform that ships no reaction
webhook — within a safe API rate budget and botbooter's thin-adapter philosophy?

## Recommended Direction

Move the polling pattern from `_examples/github-reactions` into `internal/github` as an
**opt-in** feature activated by new `Config` fields. Zero-config behavior is unchanged
(no polling, `OnReaction` silent as today). `Connect` starts one poller goroutine;
`Disconnect` stops it through the adapter's existing per-connection teardown. No
`core` changes: the poller feeds the existing `deps.DispatchReaction` seam, the same
optional-capability path every other reaction-bearing adapter uses.

```go
// Config additions
ReactionPollRepos    []string      // "owner/name"; empty = no polling (default)
ReactionPollInterval time.Duration // default 30s
ReactionStore        ReactionStore // default: in-memory
ReactionLookback     time.Duration // default 0; >0 + persistent store catches downtime reactions

// One-method store: atomic check-and-set, in-memory default, persistence pluggable.
type ReactionStore interface {
    MarkSeen(ctx context.Context, reactionID int64) (fresh bool, err error)
}
```

Poll cycle per repo: list newest 10 comments (1 API call, sorted created desc) → for
comments whose cached `reactions.total_count` changed, list reactions → `MarkSeen` +
`CreatedAt.After(connectTime - ReactionLookback)` cutoff (replaces the demo's silent
baseline cycle) → map to `core.Reaction` (content name → unicode emoji so it renders
as-is in GitHub markdown; `Raw` holds the go-github reaction) → drop self
(`selfID`) and `User.Type == "Bot"` reactors, mirroring the message-path filter →
`deps.DispatchReaction`.

Steady-state cost: 1 API call per cycle per repo (count-diff gate skips unchanged
comments), well inside a PAT's 5000 req/h at the 30s default even with several repos.

## Key Assumptions to Validate

- [ ] Adding a reaction does NOT bump the comment's `updated_at` — verify against the
      REST API docs; this is why `Since`-based polling can't work and newest-created
      page is the only cheap window.
- [ ] Count-diff gate misses a remove+add between polls (count unchanged, new ID) —
      accept the miss, or fall back to the demo's weaker zero-count skip.
- [ ] Poller goroutine fits the adapter's per-connection teardown pattern without
      racing a reconnect (same discipline as `detachedCancel`).
- [ ] Reaction IDs are globally unique across repos — one store, no repo-keying.

## MVP Scope

- Comment reactions only (symmetric with `issue_comment` message ingress).
- Explicit repo list; interval + documented cost formula.
- `ReactionStore` one-method interface with in-memory default.
- Count-diff gate; `CreatedAt` cutoff instead of a baseline cycle.
- `github.RawReaction` accessor, matching sibling adapters.
- Update `_examples/reactions` github note (fires via polling, opt-in) and fold or
  retire `_examples/github-reactions`.
- Tests via `httptest` GitHub API stub, in-repo `asserts` style.
- Implementation happens in a NEW worktree + branch under `.claude/worktrees/`.

## Not Doing (and Why)

- Issue/PR-body reactions — message ingress is comment-only; keep ingress symmetric.
- Repo auto-discovery (App installations / PAT repos) — unbounded API budget, magic.
- Adaptive rate-limit backoff — fixed interval + documented cost model suffice for v1.
- Reaction removals — core's reaction contract is added-only.
- Full coverage — reactions on comments outside the newest-page window go unseen
  forever; no fix at sane API cost. Documented contract, not a bug.

## Open Questions

- None blocking — persistent-store baseline semantics resolved by the `CreatedAt`
  cutoff + `ReactionLookback` knob.
