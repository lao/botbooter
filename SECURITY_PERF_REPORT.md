# Security & Performance Report

Result of the investigation in `SECURITY_PERF_INVESTIGATION.md`. Scope: `internal/core`,
all ten adapters, the public facade, and the dependency graph. Method: static read of
every path in the plan, automated scanners (`govulncheck`, `gosec`, `golangci-lint`,
`go vet`, `go test -race`), and cross-adapter consistency checks.

## 1. Executive overview

**Security posture: strong.** The four internet-facing webhook adapters (GitHub, GitLab,
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
| S1 | Low | DoS / consistency | `internal/teams/server.go:109-121`, `internal/whatsapp/cloud/cloud.go:302-313` | Teams & Cloud lacked the pre-auth read-concurrency semaphore (`readSem`, cap 16) that GitHub & GitLab have — the copied-scaffolding drift CLAUDE.md warns to sweep (locations are the fix as landed) | **Fixed** |
| S2 | Info | Defense-in-depth | `internal/whatsapp/cloud/cloud.go:527-529` | Wire-derived `media.ID` interpolated into the Graph API URL path unescaped (location is the fix as landed) | **Fixed** |
| S3 | Low | Dependencies | `go.mod:7` (stdlib `crypto/tls`) | `govulncheck` GO-2026-5856 (ECH privacy leak), fixed in go1.25.12 / go1.26.5; pinned toolchain is go1.25.11 — one patch behind | Accepted (see §4) |

### S1 — pre-auth read-concurrency parity (fixed)

GitHub and GitLab bound concurrent inbound body reads at `maxConcurrentReads = 16` before
the body is buffered, capping pre-auth read memory at 16 × the body cap. Teams and Cloud
bounded only *post-auth dispatch* (256), so pre-auth read buffering was limited by
connection count × 1 MiB rather than a hard 16 × 1 MiB. Impact is minor (each read is
already capped at 1 MiB and lifetime-bounded by `ReadTimeout`), but it is a genuine
correctness/consistency gap in the deliberately-duplicated webhook scaffolding.

Fix: added the same `readSem` acquire/`defer` release at the top of `teams.handleMessages`
and `cloud.handleWebhook`, mirroring the siblings exactly. Both shed with 503 (their
platforms retry), matching each adapter's existing dispatch-shed response. Sweeps to: all
four webhook adapters now hold the identical `readSem` + `dispatchSem` shape.

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
  body value (`auth.go:108-116`); `isAllowedServiceHost` enforces https + Bot-Framework
  host allowlist (`auth.go:277-292`). JWT is alg-pinned to RS256, checks aud/iss/exp with
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
- **`govulncheck`** — of 5 reported, only `crypto/tls` (S3) is reachable; the other four
  (x/text infinite-loop, slack-go `SecretsVerifier`, os symlink, openpgp) are confirmed
  **not called** by our code.
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

**Quick wins (out of scope for this PR — dependency/toolchain, not code):**
- Bump the pinned toolchain go1.25.11 → go1.25.12 (or go1.26.5+) to pick up the
  crypto/tls fix and clear GO-2026-5856.
- Opportunistically bump `slack-go` (`SecretsVerifier` CVE, uncalled) and `x/text` on the
  next dependency sweep.

**Structural (only if evidence demands):** LRU/TTL on the GitHub reaction dedup set;
a `readSem`-style cap on Signal dispatch. Neither is warranted at v1 loads.
