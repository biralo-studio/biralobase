# Compatibility and release gates

The compatibility target is the pinned PocketBase v0.40.6 behavior for REST/auth, record rules, files, realtime, validation, and the official SDKs. Production administrative schema/settings mutations intentionally move to Git/configuration. New Node functions are an additional runtime; existing synchronous Goja hook scripts are not automatically converted.

## Automated coverage

| Area | Checks |
| --- | --- |
| Upstream behavior | Entire inherited Go suite, SQLite reference and Turso build tag |
| SDKs | Official JS 0.29.0 and Dart 0.26.0: login/refresh, CRUD, rules, realtime; JS also relations, uploads/downloads and batch |
| Transactions/storage | Two simultaneous write transactions, autocommit/prepared writes, failed commit cleanup, backup consistency, forced process-death recovery |
| Schema | Generated replay, stable-ID rename with data retention, destructive guard, altered history, metadata/physical drift, development reconciliation, malformed input fuzzing |
| Provisioning/bundles | Secret-file handling, idempotent admin rotation, encrypted settings, tampered bundles, extra files, symlinks, platform/version mismatch, single-instance lock |
| Functions | Complete reflected hook inventory, nested async hooks, caller rollback, expired references, forged protocol markers, worker timeout, secret environment filtering |
| Background work | Atomic outbox rollback/commit, claims/leases, stale acknowledgment, retry/backoff, restart, deduplicated/missed cron ticks |
| OAuth/MCP | PKCE, CSRF/consent, redirect/resource validation, code/refresh replay, restart persistence, account rotation, scope restrictions, row/field isolation, official Go MCP client |

The inherited tests retain their behavior assertions. Engine-dependent formatting exceptions are explicit: stored SQL is compared without keyword-case/spacing differences; the `_params` default expression allows Turso's surrounding parentheses; SQL error responses require the error/status rather than SQLite's internal error wording. No upstream test is disabled to hide a functional failure.

The adapter also corrects engine differences found by these tests: index identifier case, invalid-view detection, computed view ID casting, eager JSON evaluation, SQL console result semantics, and stale connections after `VACUUM INTO`. These workarounds are pinned to Turso Go driver/platform libraries 0.8.3 and must be reevaluated when upgrading.

Turso's library-driven I/O mode is used behind Go's database/sql API. Caller-driven `async=1` failed bootstrap in the pinned native driver and is not enabled. Go requests and Node handlers are concurrent; this is not a claim that every native call is nonblocking. MVCC uses `BEGIN CONCURRENT` after migrations. Conflicts are returned rather than replaying application hooks with possible external side effects.

## CI and release policy

`compatibility` runs native Linux amd64 and arm64 on both engines. It includes race detection, schema fuzzing, official SDK scripts, and container builds. `runtime-images` can publish only prereleases and depends on the reusable regression gate. Container smoke tests must validate non-root/read-only startup with a mounted bundle and secrets.

`extended-stability` requires disposable dedicated self-hosted Linux runners labeled `biralobase-soak` plus `X64` or `ARM64`: nightly continuous six-hour storage workloads, and manually triggered 24-hour release-candidate runs. These long jobs cannot fit within GitHub-hosted job time limits. Provisioning these runners is an infrastructure step outside this repository. Do not run untrusted pull-request code on them.

A stable release requires successful native matrix runs, crash/recovery, container verification, representative application migration/restore tests, the six-hour and 24-hour gates, and a review of Turso's current MVCC limitations. The soak measures correctness under a storage workload, not whole-system capacity or an SLA. Add application-specific HTTP/function/MCP load and realistic file/database sizes before sizing production.

## Local evidence

During implementation on macOS arm64: the SQLite reference suite and the full Turso regression suite passed; all new feature suites passed; official JS and Dart SDK tests passed in development and immutable production modes; forced-kill recovery and a 30-second, eight-writer storage soak passed (85,600 acknowledged inserts with read/rollback verification). Race detection and a 30-second schema fuzz run passed. After updating Go and transitive OAuth dependencies, the reachable-vulnerability scan found zero vulnerabilities. Full final-matrix results belong to CI artifacts and must not be inferred from these local checks.

Long-duration and native Linux qualification are not claimed complete by the local smoke run. No production release is certified merely because this code exists.

The reproducible storage microbenchmark is `go test ./storage/turso -run '^$' -bench BenchmarkIndexedInserts -benchtime 5s -count 5`. It compares FULL-durability indexed inserts under SQLite's single-writer policy and Turso's single/concurrent pools, verifying the stored count. A 1,000-insert smoke measurement on the implementation machine gave approximately 51.6 µs/insert for SQLite, 39.4 µs for Turso with one connection, and 44.3 µs for Turso with the concurrent pool. This small run does not establish scalability or a large speedup; use representative API workloads on dedicated hardware.
