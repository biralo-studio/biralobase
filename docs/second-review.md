# Second review, 2026-11-11

Outcome of the assignment in [the reviewer briefing](reviewer-handoff.md). Branch `review/second-reviewer`, based on `3a8be03c`.

**Not ready for a stable release.** Eight defects were reproduced and fixed with regression tests. One engine limitation that cannot be fixed in this repository was found and measured. The patched image is built but **not deployed**, and the extended Linux gates have **not** been rerun on the patched storage code; both need a decision recorded under [Open](#open).

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

**Memory retained per committed write (engine, open).** The v0.8.3 engine keeps about 6 bytes resident for every committed write transaction when a single row is updated repeatedly (22 MB after two million commits, still linear) and 15-24 bytes in the soak workload. Closing the database does not release it. Reads do not grow, SQLite does not grow, and the Go driver releases its handles. A million writes a day is roughly 6-24 MB a day; a hundred writes a second is 50-200 MB a day. A write-heavy deployment needs memory headroom and a restart cadence sized for this until the engine is upgraded. It should be reported upstream.

**The soak could not see either storage defect.** It reopened its database after every 800 writes and never took a backup. It now runs a restarted lane and a long-lived lane with backups under load, bounds the MVCC log, verifies after a final reopen, paces writes to 100 a second per lane, and reports peak memory growth per acknowledged write. Unpaced it exhausts 1 GiB within hours because of the retention above.

Out-of-space behaviour was probed with a 14 MB volume at one and eight writers: every acknowledged row was present in the same process and after reopening, and no failed write was visible.

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
| `main` at `3a8be03c` (runtime `4d7b3f0a`) | Passed: run `43098720205` |
| Linux amd64 6h and 24h soaks | Running on storage source `5d9a8dce`. They no longer qualify the patched adapter. At 3.5 hours they held 795 MB and 735 MB of a 1 GiB limit with a 16 KB database |
| macOS arm64 24h soak | **Failed at 2h31m** with `no storage space`: the workstation disk filled during this review. Not a storage verdict. Restarted on `da6ad4f5` at 2026-11-11 23:30 UTC; see `.artifacts/reviewer2-soak-arm64-24h.json` |
| Konnex copy on the patched image | **Not run** |

## Open

- **Deploy and requalify.** `biralobase:review-da6ad4f5` (image ID `sha256:c069617a22319069c156f319a42e346884adf785f679067c133ead8c1fff5aad`) and `biralobase-qualification:review-da6ad4f5` (`sha256:33267acce70312c849c8329071f8451a4a063d017e6054f001ad16a152c9eb8a`) are built on the lab host from `source-da6ad4f5`. The Coolify service still runs `review-4d7b3f0a`. Changing it, rerunning the comparison, authorization and restore scripts, and starting 6h and 24h soaks on the patched adapter were not done because they modify a shared host.
- The lab host runs many unrelated services. It is not dedicated hardware.
- Native Linux ARM64 extended qualification needs a runner to be provisioned.
- Konnex frontend journeys were not run.
- The engine memory retention above, and an explicit performance decision.
