# Second review, 2026-11-11

Outcome of the assignment in [the reviewer briefing](reviewer-handoff.md). Branch `review/second-reviewer`, based on `3a8be03c`.

**Not ready for a stable release.** Eight defects were reproduced and fixed with regression tests. Sustained writes also showed growing memory use. The patched image is deployed to the private Konnex copy and passed the comparison, authorization and restore checks there. Verification on 2026-11-13 found that the patched Linux six-hour soak passed, but the 24-hour soak **failed after about 4h45m with database-lock errors**. The fixes can be integrated into experimental `main`; this does not qualify the storage engine for a stable release. See [Gates](#gates) and [Open](#open).

## Fixes

Every regression test listed fails on `3a8be03c` and passes on the branch.

| Defect | Patch and regression |
| --- | --- |
| Committers waiting for another transaction spun inside the engine, using 9-30 times the CPU per committed row; on two CPUs throughput fell as clients were added | `ac298ffd`; `TestWaitingCommittersDoNotConsumeCPU`, `TestContendedCommitsWaitOutsideTheEngine`, `TestCommitAbandonedWhileWaitingRollsBack` |
| After one backup a process never checkpointed again: the database file stopped changing and the MVCC log grew without bound | `ac298ffd`; `TestBackupDoesNotStopCheckpoints`, `TestBackupLeavesBothDatabasesAbleToCheckpoint` |
| A refresh token was accepted as a bearer token on `/mcp` | `38270859`; `TestGrantsAreBoundToClientRedirectScopeAndTokenType` |
| Reaching 1000 registered OAuth clients disabled registration permanently | `38270859`; `TestAbandonedRegistrationsDoNotExhaustTheClientLimit`, `TestEarlierClientTableIsUpgradedInPlace` |
| One IPv6 host could evade the OAuth rate limit and fill its table against everyone else | `38270859`; `TestAddressesOfOneIPv6NetworkShareARateLimit` |
| A settings change and a collection change within 50 ms dropped one of the two reloads | `4af59e6b`; `TestNotifyWatcher_SettingsAndCollectionsTogether` |
| Restarting from a reload hook deadlocked the notification watcher and any later shutdown | `4af59e6b`; `TestNotifyWatcher_ReloadHookMayTerminate` |
| A job that takes the process down while being handled was redelivered after every restart, indefinitely | `f046ec8e`; `TestJobThatNeverReturnsIsDeadLettered` |

`4acec28c` updates gRPC to 1.83.2 and x/crypto to 0.59.0. Neither advisory was reachable: no `openpgp` package is linked, and the gRPC packages arrive only through OpenTelemetry exporter configuration types. `govulncheck` now reports no findings at symbol, package or module level.

## Performance

The regression is explained. The requested improvement is still not demonstrated: at equal durability Biralobase is slower than SQLite with one client and close to it under concurrency.

`BenchmarkHTTPRecordCRUD` in `internal/runtime` reproduces the qualification workload in process (create, read, update, read with verification; no retries) against the production runtime and against PocketBase request handling on SQLite at FULL and NORMAL durability. Numbers below are from the lab host's real disk in a two-CPU, 1 GiB container, median of four interleaved runs of 2,000 requests. The host is shared and ran both soaks, so treat differences under about 10% as noise. This is not dedicated hardware.

| Clients | Before, requests/s (p95) | After, requests/s (p95) | SQLite FULL | SQLite NORMAL |
| --- | ---: | ---: | ---: | ---: |
| 1 | 457 (4.6 ms) | 455 (4.5 ms) | 626 (3.3 ms) | 1,577 (1.1 ms) |
| 8 | 683 (52.9 ms) | 803 (25.1 ms) | 912 (35.5 ms) | 2,376 (13 ms) |
| 16 | 635 (86.2 ms) | 795 (50.9 ms) | 929 (75 ms) | 2,465 (27 ms) |

SQLite NORMAL is from two runs. Three causes account for the gap reported in the briefing:

1. **Durability default, about 2.5-3 times.** Biralobase syncs its log on every commit; PocketBase's default does not. On this disk a sync costs about 1.5 ms. SQLite itself loses the same factor when set to FULL. Both FULL engines issue exactly one `fsync` per write.
2. **Commit contention, fixed.** The engine admits one committer and the rest poll inside the native call. At the storage layer in a local two-CPU Linux VM, one writer committed 4,492 rows/s and two writers 599 rows/s, with CPU per row rising from 56 µs to 1.7 ms. With commits ordered in Go the rate stays near 4,450 rows/s for 1-16 writers.
3. **Engine cost per write, not fixed.** With one client and no contention the runtime reaches 0.73 of SQLite FULL.

Connection-pool churn was suspected and ruled out: opening a connection costs about 55 µs.

**Decision left to the owner.** Without the commit order the engine coalesces syncs (738 for 4,000 commits at 16 writers), which is where it could beat SQLite. On the same disk, storage-level writes per second and CPU per write at 1, 4 and 16 writers were:

| Commit policy | 1 writer | 4 writers | 16 writers | CPU per write at 16 |
| --- | ---: | ---: | ---: | ---: |
| Unordered (before) | 562 | 797 | 449 | 4.4 ms |
| One committer at a time (chosen) | 532 | 604 | 558 | 0.56 ms |
| Four committers at a time | 569 | 782 | 707 | 2.8 ms |

Admitting four committers gains 27% at 16 writers and keeps both CPUs saturated, which the HTTP layer needs. It may pay off on a host with many idle cores; that has to be measured on dedicated hardware before it is offered as a setting. Durability was not changed anywhere.

Not measured: realistic JSON and view queries, files, functions, and OAuth/MCP traffic.

## Storage

**Growing memory use under sustained writes (open).** The initial v0.8.3 probes measured about 6 bytes of resident growth per committed write when updating one row repeatedly (22 MB after two million commits), and 15-24 bytes in shorter soak runs. Closing the database did not release that memory; corresponding read-only and SQLite probes did not grow. However, the completed patched Linux runs measured **161.0 and 189.6 bytes per acknowledged insert after settling**, with process peak RSS reaching 918 and 878 MiB respectively. These are whole-process high-water measurements across two lanes, rollback probes, backups and reopened handles, not isolated engine allocations. The earlier figures are insufficient for production sizing, and the larger growth needs allocation profiling before attributing all of it to the engine. A restart cadence has not been validated as an operational solution.

**The soak could not see either storage defect.** It reopened its database after every 800 writes and never took a backup. It now runs a restarted lane and a long-lived lane with backups under load, bounds the MVCC log, verifies after a final reopen, paces writes to 100 a second per lane, and reports peak memory growth per acknowledged write. Unpaced it exhausts 1 GiB within hours because of the retention above.

Out-of-space behaviour was probed with a 14 MB volume at one and eight writers: every acknowledged row was present in the same process and after reopening, and no failed write was visible.

**24-hour failure and diagnostic correction, 2026-11-13.** The long-lived lane reported six `database is busy: database is locked` errors, then 5,001 stored rows versus 5,000 acknowledged since pruning. The harness incremented its acknowledgment counter only after subsequent rollback/read probes, so a successful insert followed by a failed probe was not counted. The mismatch therefore does not establish a lost, duplicated or falsely acknowledged write. The harness now counts each successful insert immediately, labels errors with the operation and synthetic row ID, and stops before pruning a failed round. All lock and verification errors still fail the test; there are no retries. The original lock failure is unresolved and the new diagnostics do not turn that run into a pass. Additional regression cases verify that failed backups to an existing file or missing directory leave writes and automatic checkpoints working.

## Authorization

Probed and found correct: consent bound to the browser cookie, to an application user and to the configured collection; superusers refused; authorization codes and refresh tokens refused for another client; redirect mismatch; scopes not widened on refresh; revocation by another client ignored; deleted users refused. These are now covered in `internal/oauth/review_test.go`.

Not changed, worth knowing: consent shows scope identifiers, not the fields behind them, so adding a field to an existing exposure reaches existing grants without new consent. Clients that a user has authorized are never expired, so an account holder can still fill the client table.

## Lifecycle

Shutdown waits for a running job for up to two minutes. `deploy/compose.yaml` allows three; a platform with a shorter stop timeout will kill the process instead. A function that does not call the bridge for 30 seconds, or a job that runs past two minutes, stops the whole server by design. Both are documented behaviour and were left alone.

## Gates

| Gate | Result |
| --- | --- |
| Native Linux matrix, runtime `4acec28c` | **Passed**: run `43115472902`, amd64 and arm64, both engines, race, fuzz, vulnerabilities, SDKs, containers |
| Native Linux matrix, `da6ad4f5` (adds the soak changes; the commit the images are built from) | **Passed**: run `43120200389` |
| Native Linux matrix, `89302606` (documentation only) | **Failed in one job**: run `43121398608`, `test (ubuntu-24.04, turso)`. The inherited `TestRealtimeRecordResolve` collected events for a fixed 250 ms that started before three durable writes; the same code passed in the two runs above. Made independent of write latency in `ac0fb382` |
| Native Linux matrix, `b845f850` | **Passed**: [run `43138891621`](https://github.com/biralo-studio/biralobase/actions/runs/43138891621), all four engine/architecture jobs, both container jobs and the aggregate gate |
| `main` at `3a8be03c` (runtime `4d7b3f0a`) | Passed: run `43098720205` |
| Linux amd64 6h soak, storage source `5d9a8dce` | **Passed** 2026-11-12 02:00 UTC: exit 0, no OOM, no restarts, 18,966,400 verified writes. It ran the earlier adapter and the earlier soak (reopen every round, no backups, tmpfs), so it does not qualify the patched adapter |
| Linux amd64 24h soak, storage source `5d9a8dce` | **OOM-killed** at 2026-11-12 08:42 UTC, after about 12h42m; exit 137, OOM true, no restarts |
| Linux amd64 6h soak, patched source `da6ad4f5` | **Passed** at 2026-11-12 08:03 UTC: `PASS`, exit 0, no OOM or restarts. 4,304,244 verified writes across both lanes, 674 backups, 670 reopens. Peak RSS 918 MiB; growth after settling 161.0 bytes per acknowledged insert |
| Linux amd64 24h soak, patched source `da6ad4f5` | **Failed** at 2026-11-12 06:48 UTC, after about 4h45m: exit 1, no OOM or restarts. Long-lived lane database-lock errors; see the diagnostic correction above. Peak RSS 878 MiB; growth after settling 189.6 bytes per acknowledged insert |
| macOS arm64 24h soak | Original run **failed at 2h31m** when the workstation disk filled. The replacement on `da6ad4f5` started at 2026-11-11 23:30 UTC. As checked on 2026-11-13, its recorded PID no longer exists and its log ends with about 6h20m remaining, without a final result or exit record: **incomplete**, cause unknown. Neither run qualifies native Linux arm64 |
| Konnex copy on the patched image, 2026-11-12 01:52 UTC | **Passed** on `biralobase:review-da6ad4f5`, image ID `sha256:86ad7a88209c9084071b552c040ee688ad44db1dbbfc4d2865aa4c4e20bd9209`: 28 collections, 660 records and 41 files identical to the PocketBase reference before and after the drill; 28 user-permission and 28 function-delegation checks, OAuth/MCP and revocation; backup, restore through the API and restart with all 660 records and 41 files. Reports: `.artifacts/reviewer2-comparison.json`, `reviewer2-auth.json`, `reviewer2-restore.json`, `reviewer2-comparison-after-restore.json` |

## Lab notes

Stopping or restarting a Coolify service prunes every local image that no running container uses. The first restart for this review removed the freshly built runtime image, the unused earlier `biralobase:review-*` images and the `biralobase-pocketbase-reference:0.40.4` tag, and the start then failed at once because the services use `pull_policy: never`. Nothing was lost: the images were rebuilt from `source-da6ad4f5` (hence new image IDs), and the reference tag was restored from the same image, `sha256:5bc872da09a321fd9008edcbb9b69c8ed33ddbecaae71a6140fa2122110fda27`, which another service on the host keeps in use. Before starting a stopped lab service, check that its images exist; rebuild or re-tag first. The service restart endpoints require `POST`.

## Integration verification, 2026-11-13

The branch changes and exact-head CI results were reviewed independently. Local race checks passed for OAuth, jobs and storage; notification watcher, backup and shutdown checks passed three repeated runs with the race detector. The new failed-backup cases also passed with the race detector. A 90-second macOS arm64 soak on the corrected harness passed 19,200 writes and rollback/read checks, two backups and two reopens on local disk; this is only a diagnostic smoke check. The reachable-vulnerability scan found no vulnerabilities. A fresh read-only Konnex comparison also passed: 660 records and 41 files, no differences, with copied-authentication `updated` timestamps explicitly excluded as in the reviewer comparison.

Raw local results are in `.artifacts/merge-review-*.log`; private fixture reports remain ignored by Git. Runtime behavior and the one-committer policy were not changed during integration review; amendments concern the soak diagnostics, regression coverage and qualification record.

## Open

- **Resolve the patched 24-hour lock failure, then rerun the extended gates with the corrected diagnostics.** Coolify service `b7exyocu6nitpzi3njpqmlo9` (`biralobase-storage-qualification-da6ad4f5`), image `biralobase-qualification:review-da6ad4f5` (`sha256:b0917b64c3f0a4d187e85602623a928c5c5cd3aed0f5d01fe7d8dec56f32fe89`), contains the exited runs and their logs. Their 1 GiB memory and 512 MiB tmpfs limits must be recorded when comparing results. They do not exercise real disk sync; add a persistent-disk run. Require `PASS`, exit 0, no OOM, and read the final `memory:` line.
- The lab host runs many unrelated services. It is not dedicated hardware.
- Native Linux ARM64 extended qualification needs a runner to be provisioned.
- Konnex frontend journeys were not run.
- Profile the larger memory growth above and make an explicit performance decision.
