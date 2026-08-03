# Security & Performance Report

Result of the investigation in `SECURITY_PERF_INVESTIGATION.md`. Scope: `internal/core`,
all ten adapters, the public facade, and the dependency graph. Method: static read of
every path in the plan, automated scanners (`govulncheck`, `gosec`, `golangci-lint`,
`go vet`, `go test -race`), and cross-adapter consistency checks.

> **Archived point-in-time snapshot (this PR's branch, 2026-08-03) — not a standing
> guarantee.** Scanner output and the dependency/toolchain state drift as the tree and its
> graph move; re-run the tools listed in `SECURITY_PERF_INVESTIGATION.md` rather than treating
> these results as current. The `file:line` references below are illustrative of where a
> concern was observed on this branch and *will* go stale as code moves — treat them as
> pointers, not as authoritative locations. Specific advisory IDs and toolchain version
> numbers are deliberately kept out of this report so a stale copy can't imply assurance it no
> longer has.
>
> **Open action (not resolved by this PR):** `govulncheck` flagged a *reachable* `crypto/tls`
> advisory that requires a Go toolchain bump. It is out of scope for this two-line hardening
> change and is tracked as an open action (see §4), not covered by the "strong posture"
> summary below.

## 1. Executive overview

**Security posture: strong for the reviewed application code, with one open dependency
action (the reachable `crypto/tls` advisory above, unresolved here).** The four
internet-facing webhook adapters (GitHub, GitLab,
Teams, WhatsApp Cloud) are uniformly hardened: full `http.Server` timeout sets, body-size
caps before `io.ReadAll`, cheap method/path rejection, and no internal detail in error
responses. Authentication is adapter-specific: GitHub verifies the body HMAC
(`X-Hub-Signature-256` via go-github's constant-time `ValidateSignature`); GitLab compares
`X-Gitlab-Token` with `subtle.ConstantTimeCompare` before reading the body; WhatsApp Cloud
verifies the body HMAC with `hmac.Equal` (and its GET verify-token handshake with
`subtle.ConstantTimeCompare`); Teams verifies an RS256-signed JWT against the Bot
Connector JWKS (alg-pinned, aud/iss/exp checked) rather than a shared secret. The highest-risk question — whether a forged inbound
Teams Activity can redirect the bot's outbound replies to an attacker host — is **fully
mitigated**: the reply `serviceUrl` is bound to the RS256-signed `serviceurl` claim *and*
constrained to an https Bot-Framework host allowlist, both enforced before the value is
stored or used. No secret is logged anywhere. Two small real gaps were found and fixed
(below); everything else in the plan proved to be a false positive or a documented,
accepted limitation.

**Performance posture: strong.** Every adapter reuses one pooled `http.Client`
(keep-alives preserved); no client or transport is allocated per `Send`. The dispatch hot
path composes the middleware chain and compiles every command regex **once** (at
connect/registration), not per message; `Message` is passed by pointer with no per-message
deep copies. No unbounded memory growth, with one accepted exception: the Teams
conversation map is FIFO-capped (10k), flow state is TTL-swept, in-flight drain tracking
is a symmetric, panic-safe atomic counter, and the GitHub reaction dedup set is unbounded
in principle but negligible at poller rates (accepted, §3). No benchmarks were added —
the static read found no per-message allocation hotspots, but allocation behavior remains
unmeasured; `BenchmarkDispatch` was judged speculative absent evidence of a problem.

## 2. Findings

| ID | Severity | Area | Location | Description | Status |
|----|----------|------|----------|-------------|--------|
| S1 | Low | DoS / consistency | `internal/teams/server.go` (`handleMessages`), `internal/whatsapp/cloud/cloud.go` (`handleWebhook`) | Teams & Cloud lacked the pre-auth read-concurrency semaphore (`readSem`, cap 16) that GitHub & GitLab have — the copied-scaffolding drift CLAUDE.md warns to sweep | **Fixed** |
| S2 | Info | Defense-in-depth | `internal/whatsapp/cloud/cloud.go` (`ResolveAttachmentURL`) | Wire-derived `media.ID` interpolated into the Graph API URL path unescaped | **Fixed** |

### S1 — pre-auth read-concurrency parity (fixed)

GitHub and GitLab bound concurrent inbound body reads at `maxConcurrentReads = 16` before
the body is buffered, capping the *synchronous read* window at 16 × the body cap. Teams and
Cloud bounded only *post-auth dispatch* (256), so pre-auth read buffering was limited by
connection count × 1 MiB rather than a hard 16 × 1 MiB. Impact is minor (each read is
already capped at 1 MiB and lifetime-bounded by `ReadTimeout`), but it is a genuine
correctness/consistency gap in the deliberately-duplicated webhook scaffolding.

Fix: added the same `readSem` acquire/release at the top of `teams.handleMessages` and
`cloud.handleWebhook`. Cloud uses the sibling `defer` release, since its synchronous
portion (HMAC + parse) is fast. Teams deliberately diverges: it releases the slot *before*
`validateInbound`, because that call can block on a cold JWKS fetch — so the 16-slot gate
bounds the read itself, **not** any body still referenced during the JWKS window. This
divergence is documented in-code. Both shed with 503 (their platforms retry), matching each
adapter's existing dispatch-shed response. Sweeps to: all four webhook adapters now hold the
`readSem` + `dispatchSem` shape.

### S2 — escape wire-derived media ID (fixed)

`ResolveAttachmentURL` built `{base}/{version}/{media.ID}` with `media.ID` (from the
HMAC-verified webhook body) unescaped. It could not escape the pinned `graph.facebook.com`
host, so this is defense-in-depth only. Fix: `url.PathEscape(media.ID)`, matching the
`url.PathEscape(channelID)` already used on the Teams send path.

### Non-findings verified (proved false-positive or accepted)

- **Webhook Slowloris / error-body leaks / body caps / method filtering** — all four
  servers set the full timeout set, cap bodies, reject non-POST cheaply, and write only
  `WriteHeader` (errors logged, never returned in the response body).
- **Teams serviceUrl SSRF** — `serviceurl` claim from the signed token is compared to the
  body value; `isAllowedServiceHost` (`auth.go`) enforces https + Bot-Framework
  host allowlist. JWT is alg-pinned to RS256, checks aud/iss/exp with
  5-min leeway, and binds signing keys to the channel. JWKS cache has max-age refresh,
  min-refresh-interval (unknown-kid DoS guard) and a stale-fallback ceiling.
- **Secrets & logging** — no config-secret redaction wrapper exists, but no log or format
  call anywhere emits a token, secret, signature, private key, phone number or request
  body. (`Secret` is a flow-answer marker, not a config wrapper — it never claimed to
  protect config.) whatsmeow tightens a pre-existing loose-perm store to 0600
  unconditionally; Signal rejects credentials-in-URL and unwraps parse errors to strip
  embedded passwords.
- **Memory growth** — Teams map FIFO-capped at 10k; flow state TTL-swept every minute with
  immediate delete on complete/cancel; drain tracking is a deferred atomic counter (runs
  during panic unwinding); GitHub reaction dedup set is documented-unbounded but grows one
  int64 per *dispatched* reaction at poller rates (accepted).
- **Send / dispatch perf** — shared pooled `http.Client` per adapter; middleware and regex
  composed once; no defer-in-loop; webhook dispatch bounded + shed. Signal's dispatch
  goroutine is uncapped but serially fed by a single receive socket (accepted).
- **`govulncheck`** — triaged reachable-vs-not: advisories against uncalled symbols
  (x/text infinite-loop, slack-go `SecretsVerifier`, os symlink, openpgp) are confirmed
  **not called** by our code. Exception: a *reachable* `crypto/tls` advisory requiring a
  toolchain bump is **not** mitigated here and is tracked as an open action (§4). Re-run
  before relying on this; the advisory set moves.
- **`gosec`** — 5 issues, all pre-existing `//nolint`'d with correct justifications (OAuth
  scope URL, header name, plain-text challenge echo, adapter-owned file paths). No new
  issues introduced by the fixes.
- **ReDoS** — Go's RE2 has no catastrophic backtracking; no third-party regex engine in the
  graph. Off the table.

## 3. Accepted risks (stated, not fixed)

- **GitHub reaction dedup set** unbounded in principle; negligible at poller rates and
  documented in-code. Upgrade path: LRU keyed on the connect cutoff, YAGNI today.
- **Flow-secret durable-store exclusion** is aspirational — no `Store` implementation
  exists yet, so nothing durably persists secrets; harmless until one is added.
- **Signal dispatch** is uncapped but bounded by a single serial receive socket.
- **Other-bot filter gaps** — every platform drops the bot's *own* messages (self filters
  verified on all ten adapters); what cannot be filtered is *other* bots where the
  platform carries no bot marker (Slack `reaction_added`, GitLab notes, Signal, WhatsApp).
  Documented platform limitations; consumers must be idempotent and rate-limit.

## 4. Prioritized improvements

**Done (this PR):** S1 (readSem parity), S2 (media.ID escape).

**Open actions (out of scope for this PR — dependency/toolchain, not code):**
- **Reachable `crypto/tls` advisory (toolchain bump) — unresolved.** `govulncheck` flagged a
  reachable advisory in `crypto/tls` that needs a Go toolchain upgrade. This PR does not
  address it; it must be tracked and fixed separately rather than assumed clean.
- Run `govulncheck` and a toolchain/dependency sweep at review time and act on whatever else
  it reports then. Specific advisory IDs and toolchain versions are intentionally omitted here
  so this report can't drift into wrong as the graph moves.

**Structural (only if evidence demands):** LRU/TTL on the GitHub reaction dedup set;
a `readSem`-style cap on Signal dispatch. Neither is warranted at v1 loads.
