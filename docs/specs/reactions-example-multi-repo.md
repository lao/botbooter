# Spec: comma-separated GITHUB_REPO in _examples/reactions

Amends docs/specs/github-reaction-polling.md (example wiring only; the library
already accepts N repos in `Config.ReactionPollRepos`).

## Objective

Let the reactions example poll several repositories: `GITHUB_REPO` accepts a
comma-separated list (`"lao/botbooter,lao/other"`) and every entry lands in
`Config.ReactionPollRepos`. Single-repo values behave exactly as today.

## Tech Stack

Existing only: Go 1.25 stdlib (`strings`), no new dependencies.

## Commands

```
make all                      # fmt + vet (incl. _examples) + lint + test-race
go vet ./_examples/reactions  # the example has no tests; vet is its gate
```

## Project Structure

```
_examples/reactions/main.go → github case: split GITHUB_REPO on ",", trim
                              entries, drop empties; doc comment updated
```

## Code Style

Match the example's existing env parsing:

```go
for _, repo := range strings.Split(repos, ",") {
    if repo = strings.TrimSpace(repo); repo != "" {
        cfg.ReactionPollRepos = append(cfg.ReactionPollRepos, repo)
    }
}
```

Entries are trimmed because `" c/d"` would pass the library's "owner/name"
validation with a leading-space owner and silently poll the wrong repo; empty
entries are dropped so `"a/b,"` is not a config error. Malformed entries
(`"noslash"`) still fail at `github.New` with `ErrBadReactionConfig` — the
example adds no validation of its own.

## Testing Strategy

No example tests exist (examples are exercised by `go vet ./_examples/...` in
`make all` and by live smoke runs); keep it that way. Library-side list
handling (multiple repos, duplicate collapse, validation) is already covered
by `internal/github` tests.

## Boundaries

- Always: `make all` before done; keep the change inside `_examples/reactions`.
- Ask first: renaming the env var, adding example tests, touching the library.
- Never: change `Config.ReactionPollRepos` semantics; commit secrets from
  `_examples/.env`.

## Success Criteria

- [ ] `GITHUB_REPO="lao/botbooter,lao/other"` → both repos polled (both in
      `cfg.ReactionPollRepos`).
- [ ] `GITHUB_REPO="lao/botbooter"` → identical behavior to today.
- [ ] `GITHUB_REPO="lao/botbooter, lao/other,"` → trimmed, empty tail dropped.
- [ ] Usage comment in the example documents the list form.
- [ ] `make all` green.

## Open Questions

None.
