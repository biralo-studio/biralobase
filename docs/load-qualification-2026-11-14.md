# Coolify rebuild and load qualification, 2026-11-14

The private Konnex lab and storage qualification service were rebuilt and
redeployed. Production Konnex was not changed. **Long-run locking and sustained
memory growth remain open. Concurrent API writes and backups OOM-killed the
candidate within its 1 GiB limit. This is not a stable-release pass.**

## Exact deployments

| Service | Source and image |
| --- | --- |
| `biralobase-konnex-review`, `zrwai4gaecurfbdf3uzqgyom` | Runtime `41af1a5b`, `biralobase:review-41af1a5b`, image `sha256:023579b56e01b86496137f327496331184b94cf5f7333f52536daacc22d0f976` |
| `biralobase-storage-qualification-c02b76e1`, `b7exyocu6nitpzi3njpqmlo9` | Diagnostics `c02b76e1`, `biralobase-qualification:review-c02b76e1`, image `sha256:915ec8c01e3280c4b092e222b76f29f3d97513dca8b740c7e0a2b6f8d556e690` |

The diagnostics commit changes tests only. The Turso 0.8.3 adapter, single
committer policy and FULL durability are unchanged. The runtime serves the
Biralobase black-cat dashboard and passed its health checks after deployment.
The reference remains PocketBase 0.40.4, image
`sha256:5bc872da09a321fd9008edcbb9b69c8ed33ddbecaae71a6140fa2122110fda27`.

A backup was created before redeploying. Old soak logs/state were archived before
Coolify replaced the containers. Running image-holder containers protected the
new images, rollback image and reference tag from Coolify's unused-image pruning.
The lab retains its internal network, private mounts and two-CPU/1-GiB limits.

## Completed checks

- `go test -p 2 ./storage/turso -count=1` and its race-detector equivalent passed.
- The 90-second unpaced macOS ARM64 smoke run passed 2,308,529 acknowledged
  inserts with rollback/read verification, 17 backups and 17 reopens. Peak RSS
  was 218 MiB; final Go heap was 3.5 MiB. Its reported post-settling peak-RSS
  growth was 18.5 bytes per insert. This does not qualify Linux or long runs.
- [CI on `c02b76e1`](https://github.com/biralo-studio/biralobase/actions/runs/43627667343)
  passed all seven jobs, including native Linux AMD64/ARM64 and both engines.
- The rebuilt Konnex copy matched the reference: 28 collections, 660 records,
  41 files, zero differences with copied-auth `updated` timestamps excluded.
- Authorization passed 28 user-permission and 28 function-delegation checks,
  OAuth/MCP and revocation.

Private reports remain in `.artifacts/rebuild-20261114-*`.

## API load failure

The synthetic CRUD client exercised concurrency 1, 8 and 16, first with 200
iterations per client through the private SSH tunnel. A second run used 1,000
iterations on host loopback inside the candidate's network namespace, overlapping
the first run's cleanup. Four API backups were planned, spaced by 30 seconds
after each completion. The first two returned 204 after 11.469 and 27.676 seconds.
The third lost its connection; the fourth was never attempted.

At **18:49:14 UTC**, the kernel recorded `CONSTRAINT_MEMCG` and killed the
candidate process with approximately 980 MiB of anonymous RSS. Docker recorded
exit 137 and an automatic restart. Both load clients were interrupted. The
reference and storage probes did not restart. The current Docker `OOMKilled`
field reset after restart; the retained kernel and Docker journals establish the
historical OOM. This was a container-limit OOM, not evidence that the host ran out
of memory. No stability or throughput pass is claimed for either HTTP run.

The original load script also lost its report when cleanup raised a transport
exception. It now checkpoints completed rounds, retains its unique synthetic-row
prefix, records interrupted cleanup, stops scheduling writes after a transport
failure, and exits unsuccessfully. Three regression tests cover unavailable
writes, interrupted cleanup and a complete successful run; CI runs them too.
These reporting changes do not fix the runtime OOM. The interrupted runs' lost
request/latency counters cannot be reconstructed reliably.

The two interrupted runs left 9,889 synthetic rows, inventoried by their two
unique prefixes before cleanup. Recovery verification is recorded below once
complete. The OOM needs a separate application-level allocation profile and a
controlled comparison of CRUD alone, idle backups and concurrent CRUD/backups;
the storage probes' Go heap measurements cannot identify the API process's
allocations.

## Persistent-disk storage workloads

All five native Linux AMD64 containers started around **18:43:50 UTC on November
14**. Each has one CPU, 1 GiB RAM, no network, no automatic restart and a read-only
root filesystem. `/evidence` is a persistent host directory, not tmpfs. Only
`/tmp` uses tmpfs, limited to 128 MiB for native-library extraction. Every run
first passed forced-process-death recovery. Writers are never retried.

| Workload | Duration | Lanes | Inserts/s/lane | Idle connections | Backup/reopen cycle |
| --- | --- | --- | ---: | ---: | --- |
| `soak6h` | 6h | restarted + long-lived | 100 | 2 of 8 | 30s |
| `soak24h` | 24h | restarted + long-lived | 100 | 2 of 8 | 30s |
| `probe-churn` | 10m | long-lived | unpaced | 2 of 8 | backups every 5s |
| `probe-pooled` | 10m | long-lived | unpaced | 8 of 8 | backups every 5s |
| `probe-no-backup` | 10m | long-lived | unpaced | 8 of 8 | backups disabled |

The three probes finished at about **18:53:51 UTC**, all with `PASS`, exit 0,
no OOM and zero restarts:

| Probe | Verified inserts | Verified backups | Peak RSS | Final Go heap |
| --- | ---: | ---: | ---: | ---: |
| churn | 266,414 | 107 | 250 MiB | 2.0 MiB |
| pooled | 267,295 | 107 | 283 MiB | 3.0 MiB |
| no backup | 271,776 | 0 | 199 MiB | 0.9 MiB |

No lock errors or checkpoint-limit failures occurred in these short runs.
Increasing the idle pool did not eliminate RSS growth. The backup-free probe
also grew; backups cannot explain all growth. Go heap was small relative to RSS,
but these measurements do not distinguish live native allocations from allocator
retention. The sample is too short for a reliable long-run bytes/write estimate.
At ten minutes, both full gates had acknowledged 120,000 inserts without an
error; current RSS was approximately 391 and 402 MiB. Both were still running.

Each insert is followed by an update/rollback probe and a verified read. Working
sets are pruned after verification, so retained customer rows are not what makes
memory grow. Backups are checked for consistency and the MVCC log must remain
bounded. The full lanes verify acknowledged rows after reopening.

The six-hour gate is due approximately **November 15 at 00:44 UTC (01:44 CET)**;
the 24-hour gate is due **November 15 at 18:44 UTC (19:44 CET)**. A final `PASS`,
exit 0, no OOM and no restart are required for functional success. Even a
functional pass does not establish that memory is bounded: review the resident
trend and memory growth before making a stability decision.

## Evidence and follow-up

On the lab host, `/srv/biralobase-review/20261114/` contains:

- `evidence/<workload>/soak-*`: database, last backup, minutely `metrics.jsonl`,
  five-minute Go heap/goroutine profiles and final profiles when available.
- `telemetry/containers.jsonl`: minutely process RSS/anonymous/file/shared memory,
  cgroup memory/current/peak/events, CPU counters and container state.
- `telemetry/<container>.log` and `.state.json`: saved logs and latest exit/OOM
  state. `previous-results/` preserves the earlier qualification runs.
- Build logs, exact image IDs, runtime source archives and HTTP reports.
- `candidate-oom-kernel.log` and `candidate-restart-docker.log`: independent
  historical evidence of the API container's OOM and exit 137. Later samples
  and events are under `candidate-telemetry/` and `candidate-live-events.jsonl`.

[`monitor-soak-containers.py`](../scripts/monitor-soak-containers.py) runs on the
host independently of the workstation and SSH session. It stops after all five
workloads exit or after 26 hours. Sampling a vanished container is an error, not
a pass. Go heap profiles exclude Rust/native allocations; process RSS also
includes mappings, while cgroup usage includes file cache. Do not equate these
figures or attribute the whole difference to an engine leak without profiling.

If a lock occurs, preserve the failing database and use the operation-specific
error, acknowledged/verified counters and last goroutine profile. Do not restart
the failed workload before collecting its evidence. Compare the three probes by
acknowledged writes and settled RSS growth, not just elapsed time or peak RSS.
They share the host/disk and do not establish production capacity. Native Linux
ARM64 extended qualification, Konnex frontend journeys and the performance
decision remain separate open gates.
