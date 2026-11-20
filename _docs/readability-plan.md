# Readability implementation plan

Baseline and source evidence are in the [review](readability-review.md). All seven increments are implemented as draft pull requests against main. The links below contain each diff and its checks.

| Increment | Change | Acceptance check |
| --- | --- | --- |
| 1 · [PR #60](https://github.com/lao/botbooter/pull/60) | Name WhatsApp Cloud and Web in the public API, shorten the root introduction and internal webhook package summaries. | Both constructor families return their original numeric and string values. Old public identifiers compile. |
| 2 · [PR #61](https://github.com/lao/botbooter/pull/61) | Move connection management into lifecycle.go. Use the connection context for cancellation and retain the once guard for adapter shutdown. Condense repeated ownership explanations. | Existing stale-callback, concurrent disconnect, setup failure and sweeper tests pass under the race detector. |
| 3 · [PR #63](https://github.com/lao/botbooter/pull/63) | Share the bounded wait for an atomic dispatch count across the six transports. Name the read and dispatch slots and consolidate the duplicated wait tests. | Cover idle, completed and canceled waits. Exercise the independent HTTP shutdown and dispatch budgets with all three opt-in timing suites. |
| 4 · [PR #62](https://github.com/lao/botbooter/pull/62) | Express the transitive import test as the SDKs owned by each platform. | Test every existing public package, exclusive dependency, shared WebSocket exception and internal adapter path. |
| 5 · [PR #64](https://github.com/lao/botbooter/pull/64) | Share GitHub App credential loading in the examples and simplify basic's handler registration. | Verify PAT and App inputs, competing invalid inputs, and each example's existing startup choices. |
| 6 · [PR #65](https://github.com/lao/botbooter/pull/65) | Fill behavior-test gaps in public wrappers, offline adapters and the example commands. Run the example tests on PRs. | Test real CLI routing, multi-bot shutdown and SQLite archiving with a local GitHub endpoint. Compare module coverage and run the project gate on the integrated tree. |
| 7 · [PR #66](https://github.com/lao/botbooter/pull/66) | Follow the coverage review finding about flow validators and answer ownership. Remove stale shard documentation and redundant copies. | Reusing a conversation key preserves both sets of completed answers. Test validation and callback execution under the race detector. |

Independent reviewers checked the report before implementation, then checked increments 1–2, 3–4, 5–6 and 7. Reviews removed a duplicate shutdown signal, preserved explicit cancellation coverage, clarified channel lifetimes, and tightened tests for error precedence, GitHub requests, ordered signup prompts and retained answers. The local integration branch contains all seven PRs. A Git merge simulation verified they merge in the listed order and produce the same source tree.
