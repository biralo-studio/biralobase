# Compatibility and release gates

The compatibility target is the pinned PocketBase v0.40.6 behavior for REST/auth, record rules, files, realtime, validation, and the official SDKs. Production administrative schema/settings mutations intentionally move to Git/configuration. New Node functions are an additional runtime; existing synchronous Goja hook scripts are not automatically converted.

## Automated coverage

| Area | Checks |
| --- | --- |
| Upstream behavior | Entire inherited Go suite, SQLite reference and Turso build tag |
| SDKs | Official JS 0.29.0 and Dart 0.26.0: login/refresh, CRUD, rules, realtime; JS also relations, uploads/downloads and batch |
| Transactions/storage | Two simultaneous write transactions, autocommit/prepared writes, failed commit cleanup, ordered commits without CPU spin, backup consistency, checkpoints after a backup, forced process-death recovery |
| Schema | Generated replay, stable-ID rename with data retention, destructive guard, altered history, metadata/physical drift, development reconciliation, malformed input fuzzing |
| Provisioning/bundles | Secret-file handling, idempotent admin rotation, encrypted settings, tampered bundles, extra files, symlinks, platform/version mismatch, single-instance lock |
| Functions | Complete reflected hook inventory, nested async hooks, caller rollback, expired references, forged protocol markers, worker timeout, secret environment filtering |
| Background work | Atomic outbox rollback/commit, claims/leases, stale acknowledgment, retry/backoff, dead-lettering of unacknowledged deliveries, restart, deduplicated/missed cron ticks |
| OAuth/MCP | PKCE, CSRF/consent bound to browser, user and collection, redirect/resource/client binding, code/refresh replay, token-type separation, restart persistence, account rotation and deletion, scope restrictions, registration limits, row/field isolation, official Go MCP client |

The inherited tests retain their behavior assertions. Engine-dependent formatting exceptions are explicit: stored SQL is compared without keyword-case/spacing differences; the `_params` default expression allows Turso's surrounding parentheses; SQL error responses require the error/status rather than SQLite's internal error wording. No upstream test is disabled to hide a functional failure.

The adapter also corrects engine differences found by these tests: index identifier case, invalid-view detection, computed view ID casting, eager JSON evaluation, SQL console result semantics, the snapshot left behind by `VACUUM INTO` (which otherwise blocks every later checkpoint), and committers that poll for their turn inside the engine (commits are ordered in Go instead). These workarounds are pinned to Turso Go driver/platform libraries 0.8.3 and must be reevaluated when upgrading.

The pinned engine keeps a few bytes of memory for every committed write transaction and does not release them when the database is closed. See [the second review](second-review.md#storage) for measurements and the consequence for sizing.

Turso's library-driven I/O mode is used behind Go's database/sql API. Caller-driven `async=1` failed bootstrap in the pinned native driver and is not enabled. Go requests and Node handlers are concurrent; this is not a claim that every native call is nonblocking. MVCC uses `BEGIN CONCURRENT` after migrations. Conflicts are returned rather than replaying application hooks with possible external side effects.

## CI and release policy

`compatibility` runs native Linux amd64 and arm64 on both engines. It includes race detection, schema fuzzing, official SDK scripts, and container builds. `runtime-images` can publish only prereleases and depends on the reusable regression gate. Container smoke tests must validate non-root/read-only startup with a mounted bundle and secrets.

`extended-stability` requires disposable dedicated self-hosted Linux runners labeled `biralobase-soak` plus `X64` or `ARM64`: nightly continuous six-hour storage workloads, and manually triggered 24-hour release-candidate runs. The soak exercises a database that is restarted every thirty seconds and one that stays open with backups under load, paced to 100 writes a second each; `BIRALOBASE_SOAK_WRITES_PER_SECOND` changes the pace. Require its final `memory:` line as well as `PASS`. These long jobs cannot fit within GitHub-hosted job time limits. Provisioning these runners is an infrastructure step outside this repository. Do not run untrusted pull-request code on them.

A stable release requires successful native matrix runs, crash/recovery, container verification, representative application migration/restore tests, the six-hour and 24-hour gates, and a review of Turso's current MVCC limitations. The soak measures correctness under a storage workload, not whole-system capacity or an SLA. Add application-specific HTTP/function/MCP load and realistic file/database sizes before sizing production.

## Local evidence

During implementation on macOS arm64: the SQLite reference suite and the full Turso regression suite passed; all new feature suites passed; official JS and Dart SDK tests passed in development and immutable production modes; forced-kill recovery and a 30-second, eight-writer storage soak passed (85,600 acknowledged inserts with read/rollback verification). Race detection and a 30-second schema fuzz run passed. The Linux ARM64 runtime/builder images built successfully; the non-root, read-only container served compiled TypeScript functions and remained healthy after a persistent-volume restart. After updating Go and transitive OAuth dependencies, the reachable-vulnerability scan found zero vulnerabilities. Full final-matrix results belong to CI artifacts and must not be inferred from these local checks.

Long-duration and native Linux qualification are not claimed complete by the local smoke run. No production release is certified merely because this code exists.

## Qualification review on 2026-11-11

The second-reviewer assignment, exact deployment, regressions, private fixture locations and open gates are in [the reviewer briefing](reviewer-handoff.md).

The real Konnex comparison passed across 27 application collections (five views), 658 records and 41 downloaded files. Copied-user REST permissions, Node delegation, OAuth consent/PKCE, MCP scopes/fields/rows and credential revocation passed. A real backup restore verified all records/files after restart. A generated migration deliberately failed on duplicate values; reopening the previous bundle preserved its baseline, and a corrected migration applied successfully using the same runtime image. The final deployed image is built from `4d7b3f0a`; its repeated auth/function/MCP checks and restore passed with 28 collections and 660 records, including the synthetic qualification fixture.

The final local SQLite and Turso suites both passed (45 packages each), along with all internal/storage race checks (10 packages) and official JavaScript and Dart SDK checks in development and production modes. These final local runs used macOS ARM64 and runtime source `4d7b3f0a`; they do not replace native Linux qualification. The vulnerability scan reported no reachable vulnerable symbols, but retained a gRPC package advisory (GO-2026-6443) and an unused OpenPGP module advisory (GO-2026-5932); this is not a claim that every dependency is advisory-free.

The six-hour and 24-hour Linux amd64 storage tests and the additional macOS ARM64 24-hour test are running. Native Linux ARM64 extended qualification and the final native CI gate remain open. The bounded five-minute local storage run passed with 2,553,335 verified acknowledged writes. None of these partial results certifies a stable release.

The reproducible storage microbenchmark is `go test ./storage/turso -run '^$' -bench BenchmarkIndexedInserts -benchtime 5s -count 3`. The review's FULL-durability measurements on Apple M4 had medians of 46.4 µs/insert for SQLite, 51.3 µs for Turso single-writer and 51.4 µs for the Turso concurrent pool. A separate shared-host HTTP CRUD test processed 4,000 requests per engine without errors but was slower under Biralobase: at 16 clients, 229.8 requests/sec versus PocketBase's 877.2. The product defaults differ in synchronous durability, and this short shared-host test does not isolate engine performance. **The requested performance improvement is unproven and remains a release decision.** See the briefing for the workload, complete measurements and profiler follow-up.

## Second review on 2026-11-11

[The second review](second-review.md) lists the defects fixed on `review/second-reviewer`, the explanation of the HTTP regression with measurements, and what remains open. In short: commits no longer spin while waiting, backups no longer stop checkpoints, refresh tokens are no longer accepted as bearer tokens, and the reachable-vulnerability scan is clean. The native Linux matrix passed for the patched runtime. The patched image is not deployed and the extended gates have not been rerun on the patched adapter, so the earlier soak results do not qualify it. `go test ./internal/runtime -run '^$' -bench BenchmarkHTTPRecordCRUD -benchtime 2000x` reproduces the HTTP comparison at equal and default durability.
