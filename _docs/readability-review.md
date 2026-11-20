# Go readability review

Assessed revision `7ed72e5` on main. A new contributor encounters three different reading tasks here. A bot author needs a short path from a constructor to a handler. An adapter maintainer needs the protocol rules beside the code that enforces them. A core maintainer needs to know which connection owns a callback during a reconnect. Much of the current prose mixes those audiences.

## Implementation results

The seven implementation PRs merge together cleanly in the [plan order](readability-plan.md). The library has 420 fewer production Go lines and 367 fewer standalone comment lines. The removed prose included duplicated dependency lists, repeated shutdown explanations, and instructions about shard locks that no longer exist.

| Measure | Baseline | Combined implementation |
| --- | ---: | ---: |
| Library statements covered | 95.3% | 96.6% |
| Core engine statements covered | 98.2% | 100.0% |
| Root facade statements covered | 60.0% | 100.0% |
| Examples module statements covered | 7.7% | 68.6% |
| Library production lines | 10,388 | 9,968 |
| Library standalone comment lines | 3,064 | 2,697 |

Both modules built and passed their race suites on Go 1.25.14 and 1.26.4. The combined changes passed formatting, vet, lint, and the GitHub, Teams and WhatsApp Cloud shutdown timing suites. Every library function in the final coverage profile executed. The tests verify observable behavior including reply targets, stale connection callbacks, authentication errors, signup prompt order and GitHub polling retries.

Coverage remains partial for real SDK sessions, process startup and uncommon error paths. Tests used local services and temporary storage. Live Slack, Azure, WhatsApp pairing and LLM service checks were outside the run.

GitHub's Linux runner also passed the library and examples race suites on the test PR. The vulnerability check remains red for five findings in the baseline versions of Gorilla WebSocket and golang.org/x/net. The scanner identifies fixes in WebSocket v1.5.3 and x/net v0.60.0. The latter requires Go 1.26, which would change Botbooter's published Go 1.25 minimum. [Linux CI results](https://github.com/lao/botbooter/actions/runs/44719768816/job/136232239845), [scanner findings](https://github.com/lao/botbooter/actions/runs/44717752165/job/136225346295), [x/net minimum Go version](https://github.com/golang/net/blob/v0.60.0/go.mod)

## Practices from large Go projects

I selected three projects with large Go contributor and user communities and inspected tagged source. On November 20, 2026, their GitHub repositories had approximately 140k, 129k and 68k stars respectively. The examples below show how each project handles a specific reading task. [Go](https://github.com/golang/go), [Kubernetes](https://github.com/kubernetes/kubernetes), [Prometheus](https://github.com/prometheus/prometheus)

| Project and example | What a reader can infer | Application here |
| --- | --- | --- |
| Go, `strings.Reader` | `Len` and `Size` explain a meaningful distinction. `ReadAt` carries a short explanation of its state invariant. The receiver and short-lived offsets have short names. | Name the two WhatsApp transports explicitly. Give long-lived lifecycle fields names that distinguish their roles. Keep conventional local names such as `ctx` and `err`. [Go source](https://github.com/golang/go/blob/go1.24.0/src/strings/reader.go#L12-L58) |
| Kubernetes, `wait.Backoff` | Field docs explain units and interactions. `Step` and `DelayFunc` document mutation, zero values and concurrency where callers choose an operation. | Keep configuration constraints, handler-registration timing and shutdown ownership at the declaration or decision that needs them. [Kubernetes source](https://github.com/kubernetes/kubernetes/blob/v1.32.0/staging/src/k8s.io/apimachinery/pkg/util/wait/backoff.go#L27-L77) |
| Prometheus, label matchers | Named match modes and a small switch make the alternatives visible. Construction and matching have distinct jobs, and the regexp case gets its own preparation. | Keep BotType's explicit alternatives. Factor repeated preparation out of the examples while leaving their different routing choices visible. [Prometheus source](https://github.com/prometheus/prometheus/blob/v3.2.0/model/labels/matcher.go) |

Go's review guidance also ties variable-name length to scope and recommends documenting declarations from a caller's perspective. A comment quota would reward removing precisely the details that callers need. [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)

## Scope and measurements

The inventory covers all 132 tracked Go files across the library and the separate examples module. The figures count physical lines and lines whose first non-space characters are `//`, including Go docs. They guided source sampling. The close reading followed public constructors, connection and dispatch paths, dependency guards and example startup code. It was not a line-by-line review of all 28,368 lines.

| Baseline area | Files | Lines | Comment-only lines |
| --- | ---: | ---: | ---: |
| Examples including example tests | 21 | 2,027 | 412 |
| Public API and platform wrappers | 12 | 803 | 382 |
| Root-module tests | 67 | 15,953 | 1,166 |
| Adapters and internal assertions | 29 | 8,280 | 2,228 |
| Core engine | 3 | 1,305 | 454 |

Baseline statement coverage with the three local shutdown timing suites enabled was **95.3% in the library** and **7.7% in the examples**. The public threading-option constructors, Teams address accessor and Slack Connect had zero direct statement coverage. The basic, multi and PR-to-SQLite examples each reported zero. The whatsmeow implementation had the lowest internal adapter coverage at 71.7%. Statement execution cannot establish that a reply went to the right thread or that an older callback spared a new connection.

## Findings and decisions

Source references below use the assessed revision so they remain usable after code moves.

1. **Public naming and package docs.** The root introduction takes 29 lines to explain which packages to import and enumerates transitive dependencies. The exported constants need a separate explanation for Cloud and Web. Give the two transports explicit names, preserve old public spellings as typed constants, and retain their numeric and string representations. Put SDK isolation in one sentence. Trim implementation inventories in the internal webhook package headers. Preserve operator-facing setup and security guidance in the platform guide, public docs or adjacent code. [Root API](https://github.com/lao/botbooter/blob/7ed72e5/botbooter.go#L1-L71), [platform guide](platforms.md)
2. **Connection ownership.** Core mixes API declarations, message dispatch, send options and lifecycle in 730 lines. A reader must distinguish two channels and two once guards by reading lengthy comments on teardown, connect and disconnect. Keep the lifecycle together, name each signal and guard for its job, and explain the stale-callback, lock and drain invariants once at the relevant operation. Preserve callback capture, lock boundaries, cancellation order and errors. [Core lifecycle](https://github.com/lao/botbooter/blob/7ed72e5/internal/core/core.go#L270-L313), [regression tests](https://github.com/lao/botbooter/blob/7ed72e5/internal/core/lifecycle_contract_test.go)
3. **Duplicated dispatch waiting.** Bitbucket, GitHub, GitLab, Teams, Cloud and Signal have the same atomic-count polling loop, including the interval and cancellation behavior. Share that operation only. The counter belongs to the adapter or Signal socket that currently owns it. HTTP shutdown, capacity limits, acknowledgements and error handling are separate decisions per transport. Four adapters also repeat the two tests for the loop itself. Keep adapter integration tests and cover the common wait at its new home. [Cloud drain](https://github.com/lao/botbooter/blob/7ed72e5/internal/whatsapp/cloud/cloud.go#L376-L436), [Signal socket](https://github.com/lao/botbooter/blob/7ed72e5/internal/signal/signal.go#L548-L582)
4. **Test readability and SDK isolation.** Twelve rows list allowed and forbidden SDKs separately. Adding a single SDK entails editing nearly every row. Express the owner once for each exclusive dependency, then derive presence and absence for every public entry point. Gorilla WebSocket is shared transitively and needs a presence-only assertion for Signal. Keep the separate first-party package isolation check for adapters that use the standard library. [Dependency guard](https://github.com/lao/botbooter/blob/7ed72e5/isolation_deps_test.go#L30-L143)
5. **Example wiring.** Four commands load the same GitHub App key and parse the same IDs. The basic example also wraps a Bot just to recover a value already passed to its handler. Share GitHub construction within the examples module, keep each command's error precedence and webhook defaults, and register ordinary handler functions in basic. The basic and LLM platform switches intentionally expose different supported platforms and CLI hints. [Basic example](https://github.com/lao/botbooter/blob/7ed72e5/_examples/basic/main.go#L27-L67), [GitHub configuration](https://github.com/lao/botbooter/blob/7ed72e5/_examples/reactions/main.go#L166-L209)
6. **Coverage and repeatability.** Exercise uncovered public entry points and previously untested example behavior using the real CLI adapter, temporary SQLite storage and local transports. Exercise Slack connection startup without external credentials. Cover configuration errors alongside success, and run example tests in CI, which currently only vets that module. Measure both modules again after integration. [CI](https://github.com/lao/botbooter/blob/7ed72e5/.github/workflows/ci.yml), [examples module](https://github.com/lao/botbooter/blob/7ed72e5/_examples/go.mod)

7. **Conversation state ownership.** During the coverage review, we found that the validator documentation still described striped locks. The store uses one mutex across all conversations. Each completed conversation relinquishes its answer map before invoking the completion callback. Name the active map, describe the actual lock scope, and remove the per-answer and completion copies. Verify that a later flow for the same user leaves the previous answers intact. [Baseline conversation code](https://github.com/lao/botbooter/blob/7ed72e5/internal/core/conversation.go#L212-L351), [flow PR](https://github.com/lao/botbooter/pull/66)

## Other areas examined

- The Slack, Telegram, Discord and CLI package introductions are already short. Keep platform-specific mention, attachment and source-trust rules. Signal's private-network requirement and whatsmeow's pairing, key storage and reconnect rules belong close to the affected API.
- The GitHub reaction poller, Teams JWT validation, conversation expiry and Cloud webhook paths contain meaningful budget, security or ordering decisions. File length is insufficient grounds to rewrite them. The review of the remaining flow code uncovered stale locking guidance, detailed in finding 7.
- The thin public wrappers give each adapter an isolated import path. The direct-import tests, transitive dependency tests and behavior tests protect different contracts. The module boundary around examples lets demos share configuration without affecting library users.

## Review and reproduction

An independent review checked the six wait implementations, the SDK ownership matrix, and the example constructors against the baseline. It added compatibility checks for both legacy integer constants and for the first error returned when multiple example settings are invalid.

Inventory starts with `git ls-files '*.go'`. Group the examples first, then root-module test files, core, other internal packages and public packages. Count physical lines with `wc -l` and comment lines with `awk '/^[[:space:]]*\/\// {n++} END {print n}'` on each group's files. Coverage uses the statement-weighted total from each module's own profile. Baseline toolchain was Go 1.26.4 on darwin/arm64.

```sh
BOTBOOTER_WHATSAPP_DRAIN_TIMING_TEST=1 BOTBOOTER_TEAMS_DRAIN_TIMING_TEST=1 \
BOTBOOTER_GITHUB_DRAIN_TIMING_TEST=1 go test -race -coverprofile=/tmp/lib.cover ./...
go tool cover -func=/tmp/lib.cover | tail -1
(cd _examples && go test -coverprofile=/tmp/examples.cover ./... && go tool cover -func=/tmp/examples.cover | tail -1)
```
