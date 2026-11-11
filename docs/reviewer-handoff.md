# Biralobase second reviewer briefing

**The outcome of this assignment is in [the second review](second-review.md).** Its fixes change the storage adapter, so the soak results described below no longer qualify the current code. The text below is the original briefing.

Review and patch this prerelease before any production cutover. Correctness tests have uncovered and fixed several real defects, and a private Konnex copy now runs in Coolify. **The requested write-performance improvement has not been demonstrated; the measured HTTP workload was slower than PocketBase. Long-duration qualification is still pending.**

Repository: `git@github.com:biralo-studio/biralobase.git`. Runtime source under review: `4d7b3f0a968f05d82e91f7b7ef464eccb643cbc9`. Subsequent documentation commits do not change that runtime. The inherited compatibility target is PocketBase v0.40.6; the actual Konnex production binary used as the additional reference is v0.40.4.

## Assignment

Reproduce failures, fix their causes, add meaningful regression tests, and rerun the affected gates. Work on a review branch. Commit cohesive fixes and report the exact tested commit and deployed image digest. Do not mark a gate passed because it was started, because an earlier commit passed, or because a process remains alive.

Start with `README.md`, `docs/application.md`, `docs/deployment.md`, and `docs/testing.md`. Prioritize:

1. **Performance:** explain the regression below with profiles and a longer representative workload on dedicated hardware. Compare equal durability settings, realistic JSON/view queries, files, functions, and OAuth/MCP traffic. Do not obtain an apparent win by silently reducing durability or replaying hooks with external side effects.
2. **Storage and recovery:** finish the continuous gates, add native Linux ARM64 extended qualification, repeat imported-SQLite backup/restore and migration-failure recovery, and examine the adapter workarounds against the pinned Turso 0.8.3 driver. Turso MVCC is experimental here. Caller-driven `async=1` failed bootstrap and is disabled. This product is not lock-free or a multi-writer cluster.
3. **Authorization:** independently review consent/CSRF, PKCE, resource binding, token and refresh replay, account-policy changes, token-key rotation, cross-user rules, MCP field/action isolation, and dynamic registration abuse. OAuth account eligibility uses a fresh PocketBase OAuth2 request context with POST and empty body/headers; rules requiring password-form inputs fail closed.
4. **Lifecycle and concurrency:** review restore locking, worker cancellation, shutdown during async hooks, jobs/lease expiry, and notification watcher cleanup. Specifically probe simultaneous settings/collection notifications and lifecycle reentry from reload hooks. These are review targets, not claimed covered by the Konnex exercise.
5. **Application compatibility:** run Konnex frontend user journeys against the private copies. The completed real-data checks exercise backend APIs, not the entire frontend. Production Konnex's hooks directory was empty; no real Goja-to-Node hook migration was demonstrated.

The current vulnerability scan found no reachable vulnerable symbols, but reports gRPC advisory `GO-2026-6443` (imported v1.83.1; fix v1.83.2) and unused OpenPGP advisory `GO-2026-5932`. Confirm reachability independently and update transitive dependencies with regression checks; do not describe the dependency graph as vulnerability-free.

Keep production Konnex untouched. The existing authorization covers copying it and deploying disposable Biralobase test instances, not cutting over production, changing its credentials, or deleting its volume.

## Fixes found during this review

| Defect | Patch and regression |
| --- | --- |
| A disconnected HTTP caller killed the shared Node worker and server | `5d9a8dce`; `TestDisconnectedHTTPCallerDoesNotKillSharedWorker` |
| Narrowed multi-value selections or relation target changes escaped destructive-migration acknowledgment | `5d9a8dce`; `TestNarrowingMultiValueFieldRequiresDestructiveAcknowledgement` |
| OAuth grants survived collection-wide token rotation or a changed auth rule | `fbe65627`; eligibility and credential-revocation tests in `internal/oauth` |
| PocketBase view aliases acquired random field IDs, generating spurious migrations and replay mismatches | `eb06e75f`; `TestViewSchemaGenerationIsDeterministicAndDeployable` |
| Restore moved the held lock inode, allowing a second owner during the file swap | `a18aa67a`; `TestBackupRestoreKeepsTheDataDirectoryLocked` |
| Notification callbacks read database handles during cleanup | `a18aa67a`; joined watcher shutdown and serialized debounce callbacks; race checks over restore, termination and notify tests |
| Auth records containing `types.JSONRaw` broke function calls and bypassed bridge map envelopes | `a18aa67a`; `TestHTTPFunctionPreservesAuthJSONFields`, including forged `$ref` content |
| Turso backup reformatted imported SQLite DDL and folded catalog index names, causing valid restores to fail drift detection | `bd0c4c7f`, `4d7b3f0a`; imported SQLite backup/reopen regression, token/literal distinction, legacy-upgrade checks and Unicode identifier preservation |

Physical fingerprints now have a version. Existing legacy hashes upgrade only after an exact match. An old backup already affected by the formatting mismatch still fails closed; do not delete its migration ledger or disable drift checks. The lab was rebuilt from the preserved pristine snapshot and re-adopted with the corrected runtime, then the complete restore drill passed.

## Evidence from the Konnex copy

Results dated 2026-11-11:

- Final local regression: **45 packages passed on SQLite and 45 on Turso**; all **10 internal/storage race-test packages** passed. Official JS and Dart SDK fixtures passed in development and production modes. Logs are `.artifacts/review-final-full-sqlite.log`, `review-final-full-turso.log`, `review-final-all-race.log`, and `review-*-sdk-*.log`.

- An online SQLite backup captured the running source without stopping it. Source storage hashes before/after capture matched the copied files. The pristine snapshot remains separate from all serving copies.
- Initial reference comparison: **27 application collections, five views, 658 records and 41 API-downloaded files**, with no differences. Later comparisons explicitly exclude only auth-record `updated` timestamps changed by disposable credential tests.
- A copied verified user received identical REST results across all 27 collections. All 27 delegated Node function calls preserved those permissions. OAuth registration, consent, PKCE exchange, MCP initialization/tools, row/field restrictions, ungranted-tool rejection and password-revocation checks passed.
- A backup/restore drill changed a copied audit, restored the backup through the real API and process restart, then verified the complete schema/record/file baseline: **658 records and 41 files**.
- A generated bundle added `bb_review_items` without rebuilding the runtime image. Two synthetic rows with duplicate keys were added. A further generated unique-index migration failed on that data. The previous bundle reopened with identical data/schema/files; a corrected non-unique-index migration then applied. All **658 original records and 41 files** remained unchanged. The failure drill used another isolated copy, not the serving reference or production.
- Final deployed-image reruns also passed: **28 user-permission checks, 28 function-delegation checks, OAuth/MCP and revocation**, plus restore verification of **660 records and 41 files**. Reports: `.artifacts/review-auth-deployed.json` and `.artifacts/review-restore-deployed.json`.

The current serving bundle is `konnex-v2`, with the original 27 collections plus the synthetic test collection and two synthetic rows. The corrected v3 migration was tested separately and is not the serving bundle.

Reports in the original workspace are ignored by Git: `.artifacts/review-copy-comparison.json`, `review-copy-comparison-final.json`, `review-auth-copy-final.json`, `review-restore-copy-final.json`, `review-migration-drill.log`, and the final deployment checks. They contain counts and statuses, not customer records. The initially failed restore log is retained alongside successful reruns.

## Performance evidence

A bounded, local-to-container HTTP workload performed create/read/update/read, with JSON-value verification, at 1, 8 and 16 clients. Each engine processed **4,000 measured requests plus 1,000 cleanup deletes**, with no request failures, wrong reads or cleanup failures. No requests were retried. Both containers had a two-CPU and 1 GiB limit; the host also ran storage soaks. This is a short product-default comparison, not a capacity certification. PocketBase uses NORMAL synchronous durability by default; Biralobase uses FULL.

| Clients | PocketBase requests/sec | Biralobase requests/sec | PocketBase p95 ms | Biralobase p95 ms |
| --- | ---: | ---: | ---: | ---: |
| 1 | 435.6 | 287.0 | 3.00 | 5.81 |
| 8 | 898.8 | 395.8 | 12.75 | 57.83 |
| 16 | 877.2 | 229.8 | 27.24 | 165.36 |

Measured runtime: `bd0c4c7f`. The later runtime patch preserves Unicode identifiers in fingerprinting; it does not change the storage or HTTP execution path. Host reports: `load-pocketbase.json` and `load-biralobase.json` under the lab root below.

A separate FULL-durability indexed-insert microbenchmark on Apple M4, three five-second repetitions, had median **46.4 µs/insert SQLite**, **51.3 µs Turso single writer**, and **51.4 µs Turso concurrent pool**. That experiment also provides no speedup evidence. The earlier 1,000-insert smoke numbers in the initial implementation report should not be used as a performance claim.

## Deployment and private fixture access

Use the installed Coolify skill and existing authentication. Dashboard: <https://biralofy.dev.biralo.studio>. Project **Biralobase**, environment **qualification**:

| Resource | Identifier |
| --- | --- |
| Konnex comparison service | `zrwai4gaecurfbdf3uzqgyom` / `biralobase-konnex-review` |
| Storage qualification service | `nngfstj2yqz7qzyyav4uo7w0` / `biralobase-storage-qualification` |
| Server | `nue1`, `49.13.170.136` |
| Lab root | `/srv/biralobase-review/20261111` |
| Runtime image | `biralobase:review-4d7b3f0a` |
| Runtime image ID | `sha256:fd8f6c7fe93f3abb379b56f5aef09f256d28404058cf0d7ded06bffdcb8ac6bc` |
| Reference image | `biralobase-pocketbase-reference:0.40.4`, pinned to the original image locally |

The serving containers run non-root with read-only roots, dropped capabilities and private mounts. Their Docker network is internal, with no public ingress or outbound network access. Copied SMTP/S3/backup delivery is disabled, inherited credentials are disabled, and original hooks are not mounted. This deployment is deliberately not an internet-accessible customer service. `https://biralobase-review.invalid` is its private OAuth origin, not a reachable public hostname.

Lab directories:

- `original/`: pristine snapshot. Do not modify or serve it.
- `data-v2/`: current candidate data; `reference/`: disposable PocketBase reference.
- `data/`: earlier failed-restore reproduction, preserved for diagnosis.
- `releases/konnex-v1`, `konnex-v2`, `konnex-v3-bad`, `konnex-v3-good`: immutable test bundles.
- `app`, `app-v2`, `app-v3-bad`, `app-v3-good`: corresponding fixture sources and generated migrations.
- `migration-drill-data/`: recovered v3 fixture; `migration-drill-report.json` and `migration-failure.log`: results.
- `secrets/`: secret files readable by UID 10001. Do not print or commit them.

The original workspace has the private SSH key at `.artifacts/review-private/coolify-nue1.key` and the disposable admin password at `.artifacts/review-private/admin_password`. Test identity: `review-admin@example.invalid`. These are ignored local files, not repository artifacts.

Resolve container IPs after every Coolify recreation; they can swap. Forward localhost ports 18090 and 18091 through SSH to the candidate's internal port 8090 and reference's internal port 8080. Do not assume the declared Docker loopback port bindings actually work on this internal network. Check the image, readiness and expected service before running mutation scripts.

## Reproduce the checks

Use Go 1.27.4 and Node 26.13.1. The official SDK fixtures pin JS 0.29.0 and Dart 0.26.0; CI uses Dart 3.9.4. On the original Mac, Node is available under `.artifacts/node-v26.13.1-darwin-arm64/bin`.

```sh
go test -p 2 -timeout 25m ./...
go test -tags turso -p 2 -timeout 25m ./...
go test -race -p 2 -timeout 15m ./internal/... ./storage/...
go test -race ./core -run TestNotifyWatcher -count=3
go test ./internal/schema -run '^$' -fuzz FuzzDecodeSchema -fuzztime 30s -parallel 2
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./cmd/biralobase
go build -trimpath -o bin/biralobase ./cmd/biralobase
BIRALOBASE_TEST_PORT=18093 ./scripts/test-js-sdk.sh
BIRALOBASE_TEST_PORT=18093 BIRALOBASE_TEST_MODE=dev ./scripts/test-js-sdk.sh
BIRALOBASE_TEST_PORT=18094 ./scripts/test-dart-sdk.sh
BIRALOBASE_TEST_PORT=18094 BIRALOBASE_TEST_MODE=dev ./scripts/test-dart-sdk.sh
```

The alternate SDK ports avoid the real-copy SSH forwards. Run the scripts below **sequentially**: authentication rotates copied-user credentials, and restore deliberately rolls the whole candidate back to its just-created backup.

```sh
python3 scripts/compare-disposable-copies.py \
  --email review-admin@example.invalid \
  --password-file .artifacts/review-private/admin_password \
  --ignore-auth-updated --output .artifacts/reviewer-comparison.json
python3 scripts/exercise-disposable-auth.py \
  --email review-admin@example.invalid \
  --password-file .artifacts/review-private/admin_password \
  --allow-user-credential-changes --output .artifacts/reviewer-auth.json
python3 scripts/restore-disposable-copy.py \
  --email review-admin@example.invalid \
  --password-file .artifacts/review-private/admin_password \
  --allow-restore-and-record-changes --output .artifacts/reviewer-restore.json
```

`load-disposable-copy.py` requires the synthetic collection and explicit `--allow-synthetic-writes`. The recorded load ran on the host through `nsenter -t CONTAINER_PID -n python3 ...`, using that container's loopback address; SSH latency was not part of its timings. Private lab preparation and migration-drill scripts are in `.artifacts/review-private/`; inspect and choose fresh scratch directories before repeating them. Never substitute the production volume path.

## Gate status and completion criteria

The final local and deployment results are recorded above and in [testing](testing.md). The full native Linux matrix must be checked for the final code commit; an older green run does not qualify later patches. The original full passing gate was run `43083650319` at `6b3bacdc`. At handoff preparation, the code-commit Actions run `43097908612` was reporting queued with no check results; the final reviewer must resolve that status and retain the actual gate result.

Three extended storage tests started around **2026-11-11 20:00 UTC** and were still running at handoff preparation:

- Native Linux amd64 **6h**, container `soak6h-nngfstj2yqz7qzyyav4uo7w0`.
- Native Linux amd64 **24h**, container `soak24h-nngfstj2yqz7qzyyav4uo7w0`.
- macOS ARM64 **24h**, original workspace PID recorded in `.artifacts/review-soak-arm64-24h.json`, log `review-soak-arm64-24h.log`.

The Linux containers use `biralobase-qualification:review-5d9a8dce`, image ID `sha256:202ab24bc384951071af6e7d05ebdca82714d67dd9aa9a511e32ca422f0788f3`. Storage code is unchanged by the later runtime patches. They run synthetic data only, with no network and bounded tmpfs storage. Their forced-kill crash-recovery precheck passed. Progress counters are not a final PASS. Expect the six-hour result around 2026-11-12 02:01 UTC and 24-hour results around 20:00 UTC, if uninterrupted.

```sh
# On the lab host; do not restart these containers while qualifying.
docker logs --tail 20 soak6h-nngfstj2yqz7qzyyav4uo7w0
docker logs --tail 20 soak24h-nngfstj2yqz7qzyyav4uo7w0
docker inspect --format '{{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}}' \
  soak6h-nngfstj2yqz7qzyyav4uo7w0 soak24h-nngfstj2yqz7qzyyav4uo7w0
```

Require both a final test `PASS` and successful process exit, without OOM or restarts. A Coolify `running:unknown:excluded` status is expected for these non-HTTP test containers. The Mac process is not a native Linux ARM64 qualification. Provision a dedicated disposable Linux ARM64 runner for the remaining extended matrix in `.github/workflows/soak.yaml`.

Do not publish a stable release until final native regression/container/SDK/race/vulnerability gates, both extended durations on required architectures, realistic migration/recovery, an independent authorization review, and an explicit performance decision are complete. Node functions remain trusted application code, background/OAuth retention still needs an operational policy, and a single-volume deployment has planned restart downtime.
