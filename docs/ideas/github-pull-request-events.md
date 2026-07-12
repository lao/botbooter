# GitHub pull_request "opened" ingress in adapter

## Problem Statement

How might we let a GitHub bot react when a pull request is CREATED — not when someone
comments on it? Today the adapter's webhook handler acks and drops every event that is
not `issue_comment` (`internal/github/server.go`), so a `pull_request` "opened"
delivery never reaches dispatch even when the repo webhook is already subscribed to it.

## What Works Today (proven by `_examples/github-prs`)

- **Egress needs nothing**: `bot.SendMessageContext(ctx, "owner/repo#N", text)` posts
  an issue comment, and PRs are issues for commenting purposes — the welcome comment
  lands on the PR conversation.
- **Ingress is consumer-built**: the demo polls the repo's newest open PRs through
  `github.Client(bot)` (1 API call per cycle, newest-created page, `CreatedAt`
  cutoff + seen-set — the reaction-poller shape) and welcomes each new PR.
- The gap is push delivery: unlike reactions, **GitHub DOES ship a webhook for this**;
  the adapter just refuses to route it.

## Recommended Direction

Route `pull_request` events with action `opened` through the existing webhook handler
as an **opt-in**, synthesized as a `core.Message` on the existing `deps.Dispatch` seam
— no `core` changes, no poller. A PR body is message-shaped: it has an author, a
conversation (`owner/repo#N`), text, and a timestamp, so command regexes and the
unknown-command handler work unchanged (e.g. match PR-template markers).

```go
// Config addition
DispatchPullRequests bool // route pull_request "opened" webhooks as messages (default off)
```

Mapping: `ID` = PR node/number, `UserID`/`AuthorName` = PR author,
`ChannelID` = "owner/repo#N" (replies land on the PR), `Content` = title + "\n\n" +
body, `Raw` = a new `*PullRequestPayload`; `github.RawPullRequest(m)` accessor
distinguishes PR-opened messages from comment messages. Reuse `isSelfOrBotUser` on the
PR author (a bot opening PRs + a handler replying is the same loop class as comments).
Consumer must also subscribe the repo webhook to `pull_request` — a config
requirement, like Slack's `reaction_added` subscription.

## Key Assumptions to Validate

- [ ] Opt-in flag means zero behavior change for existing consumers (their regexes
      never see PR bodies unless they ask).
- [ ] `issue_comment`-style self/bot filtering on `pull_request` payloads: App-mode
      PRs arrive with `User.Type == "Bot"`, PAT-mode self needs `selfID`.
- [ ] The 1 MiB `maxRequestBytes` cap fits real `pull_request` payloads (they carry
      the full PR object; larger than comment payloads).
- [ ] First-match-wins commands: consumers can distinguish PR-opened from comments via
      `RawPullRequest` inside a shared handler without footguns worth a doc warning.

## MVP Scope

- Action `opened` only (symmetric with `issue_comment` action "created").
- One bool `Config` flag; webhook-subscription requirement documented like the Slack
  reaction gotcha.
- `github.RawPullRequest` accessor beside `github.RawEvent`.
- Same ack-before-dispatch, inflight-drain, and self/bot-filter discipline as the
  comment path.
- Retire the polling half of `_examples/github-prs` in favor of the flag (keep the
  example as the PR-welcome showcase).
- Tests via signed `httptest` webhook deliveries, in-repo `asserts` style.

## Not Doing (and Why)

- Other actions (`synchronize`, `closed`, `reopened`, `edited`) — "react on PR
  creation" is the ask; each extra action multiplies loop and semantics questions.
- A generic `Config.Events []string` / raw-event callback — an escape hatch with no
  cross-platform meaning; the bot's value is the uniform Message/Reaction surface,
  and `github.Client` already covers bespoke API needs.
- A core `OnPullRequest` ingress — PRs are GitHub-only; core stays platform-agnostic.
- `issues` "opened" events — same shape, separate idea if ever asked for.

## Open Questions

- Should `Content` include the body or just the title? Title-only is loop-safer for
  regex commands; title+body enables PR-template matching. Leaning title+body with
  the accessor as the precise tool.
