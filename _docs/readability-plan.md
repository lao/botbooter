# Readability implementation plan

Baseline and source evidence are in the [review](readability-review.md). Each row is a separate pull request against main. The last check runs on the combined changes.

| Increment | Change | Acceptance check |
| --- | --- | --- |
| 1 | Name WhatsApp Cloud and Web in the public API, shorten the root introduction and internal webhook package summaries. | Both constructor families return their original numeric and string values. Old public identifiers compile. |
| 2 | Move connection management into lifecycle.go. Name the two completion signals and once guards by role. Condense repeated ownership explanations. | Existing stale-callback, concurrent disconnect, setup failure and sweeper tests pass under the race detector. |
| 3 | Share the bounded wait for an atomic dispatch count across the six transports. Consolidate only the duplicated primitive tests. | Cover idle, completed and canceled waits. Exercise the independent HTTP shutdown and dispatch budgets with all three opt-in timing suites. |
| 4 | Express the transitive import test as the SDKs owned by each platform. | Test every existing public package, exclusive dependency, shared WebSocket exception and internal adapter path. |
| 5 | Share GitHub App credential loading in the examples and simplify basic's handler registration. | Verify PAT and App inputs, competing invalid inputs, and each example's existing startup choices. |
| 6 | Fill behavior-test gaps in public wrappers, offline adapters and the example commands. Run the example tests on PRs. | Test real CLI routing, multi-bot shutdown and SQLite archiving with a local GitHub endpoint. Compare module coverage and run the project gate on the integrated tree. |

Independent code review checkpoints follow increments 2, 4 and 6. Review focuses on behavior, error handling, API compatibility and unnecessary complexity. Draft PRs keep the merge decision with the maintainers.
