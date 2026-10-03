# Resume latency: measurement and improvement journal

Branch `perf/resume-latency` on the fork. Started 2026-10-02. Nothing here is a
PR; the "Candidates to submit" section lists what is worth upstreaming.

## Goal

Lowest possible resume latency for a coding-agent workload (the agent-session
script: ~40 CPU-seconds of work per lap, 20 suspend/resume cycles per lap,
long idle gaps). Scale, throughput and oversubscription come later. Only
Substrate components are changed; gVisor and the micro-VM stack are treated as
fixed.

## Method

- Cluster `resume-lat`, project `gke-ai-eco-dev`, zone `us-central1-a`,
  GKE 1.36.4-gke.1495000.
  - System pool: 2x c3-standard-8 (ate-system, locust).
  - Worker pool `bench-workers`: 1x c3-highmem-88 (88 vCPU, 704 GiB), tainted
    `ate.dev/benchmark-workers=true:NoSchedule`; only the benchmark WorkerPool
    tolerates it. "One big node" means one worker node; the control plane sits
    elsewhere so its noise does not land on the measured node.
  - Later phases add worker nodes to the same pool.
- Workload: `agentsession` user class, glutton template at 1Gi actor memory,
  100 users, one actor each. Resume mode `implicit` (the router wakes the actor
  on first touch) so the measured number is what a client sees.
- Lifecycle: both `pause` (node-local checkpoint) and `suspend` (durable
  snapshot in GCS) are measured; each run states which.
- Primary metric: `WakeFirstTouch` p50/p90/p99 from the boomer stats, the
  client-observed time from first request to first byte after an idle gap.
  Secondary: `ResumeActor` RPC time, atelet `Restore timing breakdown`,
  ateom `Restore timing breakdown`, ateapi `Resume timing breakdown`.
- Each change is measured against the previous run on the same cluster, same
  user count, same script. A change is kept only if the delta is outside
  run-to-run noise (two baseline runs establish the noise band).

## Critical path (as found on main @ bec46812)

Router ext_proc -> `ResumeActor` RPC (no deadline; 5s park budget, 10s Envoy
ext_proc timeout) -> ateapi workflow: GetActor, lease INSERT, GetActor +
GetActorTemplate, schedule (in-memory scan, power-of-two), bind tx (4 stmts),
UpdateActor (SELECT+UPDATE), atelet dial (LRU conn, lazy mTLS), atelet
`Restore`, GetActor + UpdateActor -> RUNNING, lease DELETE. About 13
autocommit statements plus one 4-statement transaction per cold resume.

atelet `Restore`: reset dirs, mount volumes, manifest GET (serial), then
download || (sandbox assets, SystemInfo register, OCI prep with a registry
HEAD per tag ref), dial ateom, `RestoreWorkload`, write sandbox record.

ateom-gvisor `RestoreWorkload`: reset, tunnel deactivate, PrepareEgress (cert
mint: ateom -> atelet -> ateapi -> Postgres), netns + veth + nftables, durable
dirs, overlay rootfs, `runsc create` + `runsc restore` for pause then each app
container serially, wakeup probe (1ms poll), tunnel activate.

Blind spots before this work: ateom-gvisor had no timing at all; atelet's
phases did not partition the total (#1646); ateapi had no per-step durations
outside spans; the cert-mint chain was untimed.

## Already in flight upstream (not redone here)

| What | Where | Status 2026-10-02 |
|---|---|---|
| Node-local shared snapshot cache | #1551 (dberkov) | open |
| HasRoom without reparsing quantities | #2016 (Oneimu) | open |
| Local pause restore straight from LocalSnap | #1876 (chw120) | open |
| PostgreSQL statement tracing | #1462 (git286) | approved, unmerged |
| Restore phase breakdown pipeline | #1941 merged; #2009, #2148 open | |
| `sandbox_record` restore phase | #1975 (lubingtan) | open |
| Power-of-two worker choice | #1915 | merged |
| Replaced-snapshot release after SUSPENDED commit | #2006 | merged |
| Lease cleanup off the acquire path | #1880 | merged |

## Runs

Each run: id, date, build, config, results table, notes. Appended as they
happen.

## Changes

Each change: what, why, measured effect, verdict (keep / drop), submit?

### C0. Instrumentation (no latency effect intended)

Added before the baseline so the baseline itself shows where time goes.

- **ateom-gvisor phase breakdown.** `RestoreWorkload` and `CheckpointWorkload`
  now emit the same joinable `Restore timing breakdown` / `Checkpoint timing
  breakdown` log records ateom-microvm already had (keys
  `ateom.actor.restore.duration.<phase>`, float seconds). Restore phases, all
  sequential: `prep`, `egress_prepare` (remote cert mint), `net_setup`,
  `durable_dir`, `pause_rootfs`, `pause_create`, `pause_restore`,
  `app_rootfs`, `app_create`, `app_restore`, `wakeup_probe`, `activate`,
  `total`, plus `ate.actor.container.count`. The shared rendering moved to
  `internal/ateomphaselog` so both ateoms use one implementation.
  Files: `cmd/ateom-gvisor/{main.go,phaselog.go}`, `cmd/ateom-microvm/*`,
  `internal/ateomphaselog/*`. Submit: yes (closes the gVisor half of the
  restore blind spot; pairs with #1941).
- **atelet restore gaps.** New `ate.snapshot.phase` values `dirs_reset`,
  `sysinfo_register`, `ateom_dial`, `sandbox_record` (restore) and
  `local_prune`, `volume_unmount` (checkpoint), recorded in the histogram and
  the breakdown log. With these the restore phases partition the total (the
  #1646 gap). `sandbox_record` uses the name from the open #1975 so the two
  converge. Registry updated; weaver check passes. Submit: yes, after #1975
  lands or folded into it.
- **ateapi resume step timing.** One `Resume timing breakdown` Info record per
  non-no-op resume with `ate.actor.resume.duration.{get_actor, lease_acquire,
  load, volumes_create, assign, schedule, bind, assign_update, volumes_attach,
  atelet_dial, atelet_restore, finalize, lease_release, total}` and
  `ate.actor.resume.assign_attempts`. Same shape as the suspend workflow's
  existing store-call duration log. Submit: yes.
- **router flight result.** `ResumeActor result` now logs `elapsed_seconds`
  (headers arrival to result), `rpc_attempts`, `first_rpc_delay_seconds`,
  `outcome`, on success and failure. Submit: yes.
- **benchmark tooling.** `benchmarking/workloads/deploy.sh` gained
  `--worker-node-selector` and `--worker-toleration` so the WorkerPool can be
  pinned to a dedicated node pool. Submit: yes (small, independent).

## Candidates to submit

(Filled in as changes prove out.)
