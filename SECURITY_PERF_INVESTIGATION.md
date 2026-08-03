# Security & Performance Investigation Plan

Goal: produce a report with a security and performance overview of botbooter and a
prioritized list of improvements. This document is the investigation plan — what will
be examined, how, and with which tools. Findings land in a separate report
(`SECURITY_PERF_REPORT.md`).

## Scope

The whole library: `internal/core` (lifecycle + dispatch), all ten adapters
(`internal/{cli,slack,discord,telegram,whatsapp/cloud,whatsapp/whatsmeow,teams,github,gitlab,signal}`),
the public facade packages, and the dependency graph. `_examples` only insofar as it
demonstrates unsafe usage patterns.

## Method

1. Static pass: read the code paths listed below, file:line notes per finding.
2. Tool pass: run automated scanners (see Tooling) and triage output.
3. Dynamic pass: race detector + targeted benchmarks on hot paths.
4. Write report: each finding gets severity (high/med/low), evidence, and a concrete fix.

---

## Security investigation

### 1. Webhook ingress authentication (highest-value target)

Four adapters run their own HTTP servers exposed to the internet. Verify each one:

- **GitHub** (`internal/github`): HMAC signature verification — constant-time compare?
  Verified before body is trusted? Body size cap before reading? Behavior on missing
  signature header.
- **GitLab** (`internal/gitlab`): `X-Gitlab-Token` compare — confirm `subtle.ConstantTimeCompare`
  (docs claim constant-time; verify). Confirm auth happens before body read as documented.
- **Teams** (`internal/teams`): JWKS JWT verification — algorithm pinning (alg confusion),
  issuer/audience claim checks, JWKS cache poisoning/refresh behavior, clock skew handling.
- **WhatsApp Cloud** (`internal/whatsapp/cloud`): `X-Hub-Signature-256` verification,
  verify-token handshake on GET.

Cross-cutting for all four (the "copied scaffolding" — CLAUDE.md says a bug in one
probably applies to siblings; sweep all):

- `http.Server` timeouts set? (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
  `IdleTimeout`) — missing ReadHeaderTimeout = Slowloris.
- Request body caps (`http.MaxBytesReader` or equivalent) — consistent across adapters?
- Concurrency shedding — can a flood of authentic-looking requests exhaust goroutines/memory?
- TLS story: servers appear to be plain HTTP (reverse-proxy assumption). Is that assumption
  documented per adapter? Any option to serve TLS directly?
- Error responses: do 4xx/5xx bodies leak internal details (paths, versions, Go errors)?
- Method/path filtering: non-POST, wrong paths — rejected cheaply and early?

### 2. Secrets handling

- The `Secret` builder in core — what does it protect against (logging? String()?)?
  Are all Config secret fields routed through it?
- Grep all logging (`slog`/adapters) for token/secret/header leakage, incl. debug paths.
- whatsmeow SQLite store: chmod 0600 claimed — verify at creation *and* that a pre-existing
  looser-perm file isn't silently reused. Path traversal on the configured store path?
- Signal adapter: phone number + container URL — anything sensitive logged?

### 3. Outbound request surfaces (SSRF-adjacent)

- `ResolveAttachmentURL` / `AttachmentResolver` implementations: are returned URLs
  attacker-influenced (from inbound payloads)? A consumer fetching them is the SSRF risk —
  is that documented?
- `whatsmeow.Download`: media proto fields come from the wire — validate before fetch?
- Signal adapter dial-out: WebSocket + REST to a configured container URL — scheme
  restrictions? Credentials in URL?
- Teams `conversationID→serviceUrl` map: serviceUrl comes from *inbound* Activities.
  A forged/compromised activity could redirect replies to an attacker host — how is this
  bounded (JWT verification scope covers it? serviceUrl allowlist?). This is the single
  most interesting security question in the codebase.

### 4. Untrusted-input parsing

- Regex command patterns: consumer-supplied, but matched against *attacker-supplied* text.
  Go's RE2 = no catastrophic backtracking, so ReDoS is off the table — confirm no
  third-party regex engine snuck in.
- JSON decoding of webhook payloads: decoder limits, unknown-field behavior, deeply
  nested payloads.
- GitLab `internalNote` raw-body side-parse: hand-rolled parsing of untrusted JSON — check
  for confusion between top-level and nested keys.
- CLI adapter: any-token-that-is-a-file becomes an attachment — documented as trusted-local
  only; verify the warning is prominent and consider a constructor-level opt-in.

### 5. Loop & abuse resistance

- Self-message filters per platform (documented matrix) — verify each against code;
  the known gaps (Slack reactions, GitLab other-bots, Signal, WhatsApp) should appear
  in the report as accepted risks with mitigations for consumers (idempotency, rate limits).
- No built-in rate limiting on dispatch: a hostile chat can drive unbounded handler
  concurrency? Check goroutine-per-message vs. serialized dispatch per adapter.
- GitHub reaction poller: API budget logic (3000 req/h) — can a malicious repo list
  or wildcard expansion blow the budget or amplify requests?

### 6. Dependency & supply chain

- `govulncheck ./...` — known CVEs in the graph.
- Review go.mod: versions of `golang-jwt/jwt/v5` (history of CVEs), `gorilla/websocket`,
  `whatsmeow`, `modernc.org/sqlite`, `go-github`, GitLab client-go.
- Confirm isolation tests (`isolation_deps_test.go`) actually prevent SDK bleed — they're
  a supply-chain blast-radius control.

---

## Performance investigation

### 1. Dispatch hot path (`internal/core`)

- Per-message allocations: Message struct copies, middleware closure chain (composed
  per-dispatch or once at registration?).
- Regex matching: first-match-wins over N patterns = O(N) regex runs per message —
  fine for small N; note scaling behavior, no action unless evidence.
- `recover` wrapper cost: negligible, confirm no defer-in-loop patterns.
- Add micro-benchmarks: `BenchmarkDispatch` with 1/10/100 handlers, with/without
  middleware. Currently no benchmarks in repo (verify with grep).

### 2. Lock contention & lifecycle

- `b.mu` held across `adapter.Connect` — intentional (documented), but verify no adapter's
  Connect does blocking I/O under the lock that would stall concurrent API calls.
- Connection-scoped `sync.Once` teardown — race-detector coverage exists; confirm
  `make test-race` clean on this branch.

### 3. Unbounded memory growth (the real perf risk in a long-running bot)

- Teams `conversationID→serviceUrl` map: grows per conversation, never evicted?
- GitHub reaction dedup store: in-memory, documented as such — bounded (LRU/TTL) or
  unbounded set?
- GitLab/GitHub/Teams/WhatsApp in-flight drain tracking: WaitGroup vs. map — leak on
  abnormal paths?
- Flow state (`HandleFlow` multi-step forms): per-user state — TTL/eviction on abandoned
  flows?

### 4. Webhook adapter throughput

- Goroutine-per-delivery dispatch: shedding thresholds, drain behavior under load.
- Body read: full `io.ReadAll` vs. streaming decode; double-buffering (GitLab reads raw
  body for `internalNote` side-parse *and* typed parse — one read reused, or two?).
- Response latency: GitLab must ack within ~10s or delivery counts as failed — is dispatch
  fully async from the handler (ack-then-dispatch) in all webhook adapters?

### 5. Poller efficiency (GitHub reactions)

- Steady-state cost documented as 1 req/repo/cycle — verify; check per-cycle allocations
  and whether comment listing reuses conditional requests (ETags) to cut rate-limit spend.

### 6. Send path

- Per-Send client allocations? Connection reuse (`http.Client` shared, keep-alives)?
- Retry behavior: none documented — note as reliability/perf tradeoff, not a defect.

---

## Tooling

```bash
govulncheck ./...
gosec ./...                     # triage aggressively; webhook servers are the signal
golangci-lint run               # already in make all; check enabled linters cover gosec-lite
go test -race ./...             # baseline
go test -bench . -benchmem ./internal/core/...   # after adding benchmarks
go vet ./...
```

## Deliverable

`SECURITY_PERF_REPORT.md` with:

1. Executive overview (posture summary, one paragraph each for security and performance).
2. Findings table: ID, severity, area, file:line, one-line description.
3. Detail per finding: evidence, impact, concrete fix (smallest change that works),
   and whether it sweeps to sibling adapters.
4. Accepted risks / non-findings worth stating (RE2 no-ReDoS, documented filter gaps,
   deliberate scaffolding duplication).
5. Prioritized improvement list: quick wins vs. structural work.
