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

### B1. Baseline, gVisor, pause lifecycle, think-scale 5 (2026-10-03 04:17-04:24 UTC)

Build 20d10da3 (main bec46812 + instrumentation). 100 users, spawn 5/s,
`resume_mode=implicit lifecycle_mode=pause agentsession_think_scale=5`,
1 worker pod (600Gi) on the c3-highmem-88, glutton 1Gi. Ran 7 minutes.

Client side (ms, boomer stats):

| series | n | p50 | p90 | p99 | max | fail |
|---|---|---|---|---|---|---|
| WakeFirstTouch | 1482 | 220 | 690 | 10000 | 10000 | 34 |
| PauseActor | 1609 | 3900 | 21000 | 44000 | 53000 | 34 |

WakeFirstTouch at the 60 s mark, before pause backed up: p50 200, p95 320,
p99 460. The p99 of 10 s and the 34 failures at the end are the router's park
budget expiring behind queued pauses.

Server side, local (pause) restores only, p50 / p90 / p99 seconds:

| layer | total | biggest phases |
|---|---|---|
| router flight (`elapsed_seconds`, outcome triggered) | 0.218 / 0.322 / 0.830 | |
| ateapi `Resume timing breakdown` | 0.211 / 0.319 / 0.506 | atelet_restore 0.200; assign 0.004 (bind 0.002, assign_update 0.002); finalize 0.002; lease_acquire 0.002; lease_release 0.002 |
| atelet `Restore timing breakdown` | 0.187 / 0.291 / 0.457 | ateom_restore 0.184; everything else < 1 ms at p50 |
| ateom-gvisor `Restore timing breakdown` | 0.204 / 0.329 / 0.640 | app_restore 0.095; pause_create 0.041; app_create 0.021; pause_restore 0.018; net_setup 0.005; egress_prepare 0.004; prep 0.003; wakeup_probe 0.003 |
| ateom-gvisor `Checkpoint timing breakdown` | 4.39 / 9.20 / 10.73 | checkpoint (runsc) 4.29 / 9.10 / 10.64; teardown 0.09 |

First activation from the golden (100 samples): atelet total 0.249 p50, of
which ateom_restore 0.157, download 0.049, manifest_fetch 0.029 (serial GCS
GET before the download starts).

Findings:

1. On the pause/resume path about 90% of a 200 ms wake is `runsc`
   (create + restore of the pause and app containers, 175 ms of 204 ms in
   ateom). Substrate's own share is about 25 ms: ateapi 12 ms of store work,
   router about 7 ms, atelet about 3 ms, ateom outside runsc about 15 ms.
   The hypothesis that the sandbox layer would not matter does not hold for
   gVisor pause/resume at this scale.
2. Pause is disk-bound. Each local checkpoint is about 167 MB on disk; at
   3.8 pauses/s the worker's boot disk (hyperdisk-balanced, 890 MiB/s
   provisioned) sat at about 905 MB/s written with I/O pressure `full`
   avg60 at 50%. Checkpoints queued behind each other (p50 4.4 s, p99 11 s
   in ateom; 44 s at the client once the queue built up) and the backlog
   eventually starved resumes.
3. The ateapi path is already cheap: 13 statements cost about 12 ms total.
   Collapsing them is worth a few milliseconds, not tens.
4. atelet's restore phases now partition the total (`total` minus
   `ateom_restore` is about 3 ms, all accounted).
5. Side findings. A PAUSED actor is not deletable (`FailedPrecondition: not
   in a deletable state`); the load generator's shutdown could not clean up
   after the overload, leaving 100 paused actors to suspend by hand. The
   runsc on the node (release-20260824.0-120) checkpoints with
   `-compression none` by default (pages.img is the resident set, 175 MB
   here) and offers `-direct` (O_DIRECT), `-exclude-committed-zero-pages`,
   and on restore `-background` (return before all pages are loaded,
   uncompressed images only). None are passed today. These are flags on how
   Substrate drives the sandbox, not changes to gVisor; listed here for the
   user to rule in or out.


### B2. Baseline, gVisor, pause lifecycle, think-scale 15 (04:29-04:41 UTC)

Same build and cluster as B1; only `agentsession_think_scale=15` (about 4%
duty cycle, the intended coding-agent profile). 12 minutes, no failures.
This is the reference configuration for the pause path from here on.

| series | n | p50 | p90 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1298 | 190 | 240 | 250 | 290 | 400 |
| PauseActor (ms) | 1392 | 230 | 620 | 1000 | 2500 | 5100 |

Server side p50 / p90 / p99 (s):

| layer | total | phases |
|---|---|---|
| router flight | 0.196 / 0.239 / 0.278 | |
| ateapi resume | 0.196 / 0.239 / 0.283 | atelet_restore 0.185; assign 0.004; finalize 0.002; bind 0.002; lease_acquire 0.002 |
| atelet restore (local) | 0.177 / 0.220 / 0.269 | ateom_restore 0.176 |
| ateom-gvisor restore | 0.175 / 0.196 / 0.223 | app_restore 0.084; pause_create 0.035; app_create 0.018; pause_restore 0.016; net_setup 0.004; wakeup_probe 0.004 |
| ateom-gvisor checkpoint | 0.172 / 0.262 / 0.393 | checkpoint (runsc) 0.127; teardown 0.045 |

Pause at 2.4/s writes about 420 MB/s, under the disk's 890 MiB/s, so the
uncontended checkpoint shows: 127 ms of runsc for a 175 MB image plus 45 ms
of teardown. The 2.5 s p99 is still disk queueing when pauses coincide.

Harness note: the agent-session shutdown fan-out runs only when the boomer
process quits (`cmd/benchmarking/boomer-worker/main.go` calls it after
SIGTERM or a master quit), not on a web-UI stop, so every interactive run
leaves its actors PAUSED and a web-UI restart creates 100 more. Cleanup here
is a suspend+delete loop. A `boomer:stop` subscription that runs the same
fan-out would fix it; harness-only, listed under candidates.


### B3. Baseline, gVisor, suspend lifecycle, think-scale 15 (04:44-04:55 UTC)

Same build and cluster; `lifecycle_mode=suspend` (durable snapshot to GCS
on every idle gap, download on every wake). 11 minutes.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1084 | 1200 | 2000 | 2300 | 3400 | 10000 | 2 |
| SuspendActor (ms) | 1184 | 2000 | 11000 | 12000 | 18000 | 23000 | 2 |

At the 4-minute mark, before the backlog built: WakeFirstTouch p50 770,
p95 1200, p99 2100; SuspendActor p50 1200.

Server side p50 / p90 / p99 (s), whole run:

| layer | total | phases |
|---|---|---|
| router flight | 1.112 / 1.887 / 3.329 | |
| ateapi resume | 1.356 / 2.116 / 3.811 | atelet_restore 1.346 |
| atelet restore (latest) | 1.179 / 1.931 / 3.347 | download 0.927 / 1.633 / 3.057; ateom_restore 0.187; manifest_fetch 0.058 |
| atelet checkpoint (external) | 1.947 / 10.681 / 17.474 | persist (zstd + GCS PUT) 1.585 / 8.869 / 11.477; ateom_checkpoint 0.211 / 1.859 / 8.458; dirs_reset 0.065 / 0.549 / 5.588 |

First 4 minutes only, atelet restore: total 0.782, download 0.551,
ateom_restore 0.166, manifest_fetch 0.060.

Findings:

1. A suspend/resume wake is 4 to 6 times a pause/resume wake, and the whole
   difference is Substrate-side: the GCS download of the actor's own 175 MB
   checkpoint (0.55 s uncontended, 0.93 s p50 over the run) plus the serial
   manifest GET (0.06 s). The sandbox restore is the same 0.17 to 0.19 s.
2. The node already had those bytes: suspend streams them from
   `checkpoint-state/` to GCS and then deletes them. Keeping that copy on the
   node and restoring from it when the same node is picked turns this path
   into the pause path (change C3 below).
3. Suspend is upload-bound and disk-bound: `persist` 1.6 s p50 and
   `dirs_reset` (deleting the uploaded files) p99 5.6 s under contention.
   Renaming the files into a retained copy instead of deleting them removes
   the delete from the suspend path too.
4. Download speed itself is 190 to 320 MB/s per stream (one GET, one zstd
   decoder). That matters once actors move between nodes; upstream #1955
   (client pool spread) is the in-flight work there.


### B4a. C1+C2+C3 build, pause lifecycle, think-scale 15 (05:12-05:23 UTC)

Build 0ccab235. Same configuration as B2.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1111 | 190 | 240 | 260 | 310 | 760 | 0 |
| PauseActor (ms) | 1211 | 250 | 890 | 2000 | 3600 | 5800 | 0 |

ateom-gvisor restore p50 0.164 (B2: 0.175); `egress_prepare` still 4 ms of
wall time but now overlapped, `egress_join` 0. ateapi total 0.190 (B2
0.196). Client-visible wake unchanged within noise, as expected: the pause
path has no download and the mint was 2% of it. C2 kept (it is pure
overlap); C1 has no effect on this path.

### B4b. C1+C2+C3 build, suspend lifecycle, think-scale 15 (05:25-05:36 UTC)

Same configuration as B3.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1096 | 190 | 250 | 270 | 340 | 420 | 0 |
| SuspendActor (ms) | 1242 | 1800 | 4700 | 18000 | 60000 | 60000 | 48 |

| layer | B3 (s) | B4b (s) |
|---|---|---|
| atelet restore (latest) total p50 / p90 / p99 | 1.179 / 1.931 / 3.347 | 0.182 / 0.236 / 0.327 |
| of which download | 0.927 / 1.633 / 3.057 | (retained: hard links, under 1 ms) |
| ateapi resume total p50 / p99 | 1.356 / 3.811 | 0.195 / 0.339 |

`ate.actor.restore.source`: 987 retained, 16 download (the 16 are first
activations of replacement actors after crashes, see below). A same-node
suspend/resume wake is now the pause wake: 6x at p50, 10x at p99 against
B3, and 0 wake failures.

Suspend itself did not improve and shows two problems that need separating
from C3: (a) one minute (05:27) in which every GCS upload took 20 to 58 s
(`persist` p50 19.7 s in that minute, 1.0 to 1.4 s in every other minute),
which produced the 60 s deadline failures; B3 showed the same shape at a
smaller scale and it is on the GCS side, not the node (disk I/O pressure was
under 5% at the time); (b) 16 `runsc checkpoint` exits with status 128 that
crashed their actors; investigated below.

The 16 checkpoint failures all fall in 05:28, the minute after the GCS
stall, and nowhere else in the day (Cloud Logging over every run since
04:10). runsc's stderr for them is `connecting to control server: connection
refused`: the sandbox was already being torn down when `runsc checkpoint`
ran, which is what a suspend whose context was canceled by the 60 s client
deadline (contexts propagate ateapi to atelet to ateom) looks like while the
backlog of 20 to 58 s uploads drained. C3 is not on that path (it runs after
a successful upload) and the actors restored from retained copies all
restored cleanly. Verdict: keep C3. Separate finding: a suspend canceled by
its caller's deadline leaves the actor CRASHED rather than RUNNING or
SUSPENDED (upstream #1665 family).

C1 could not be measured here: with retained hits there is no manifest GET
left to overlap. It will show on the multi-node runs, where misses download.

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

### C1. atelet: manifest fetch and ateom dial off the serial path (590efbb5)

The snapshot manifest GET ran before the asset/OCI preparation leg although
that leg needs only the request; it now heads the download leg so both legs
start together. The ateom dial and workload spec build moved ahead of the
fan-out. Error attribution unchanged (first error wins; collateral phases
zeroed). Expected: about the manifest GET (29 ms p50 on golden restores in
B1) off EXTERNAL restores when the prep leg is the longer one; nothing on
pause restores. Measured: pending (B4).

### C2. ateom-gvisor: egress certificate mint concurrent with sandbox setup (590efbb5)

`PrepareEgress` (ateom to atelet to ateapi to PostgreSQL) ran first and
serially; its result is only consumed by `tunnel.Activate` at the end. It now
runs in a goroutine started where it used to block and is joined just before
activation; a new `egress_join` phase reports what is left on the critical
path. Failure paths wait for the goroutine before cleanup. Same change in
`RunWorkload`. Expected: about 4 ms p50, 13 ms p99 off every gVisor restore.
Measured (B4a vs B2): ateom restore p50 0.175 to 0.164 s, egress_join 0;
client wake unchanged within noise. Kept (pure overlap, no cost).

### C3. atelet: restore from the node-local copy of the just-uploaded snapshot (0ccab235)

Suspend renamed nothing and deleted everything: the checkpoint files were
streamed to GCS from `checkpoint-state/` and then removed by the actor dir
reset. They are now renamed into `<actor>/retained-snapshot/` together with
the manifest and the exact snapshot URI; an EXTERNAL restore whose URI
matches byte for byte hard-links them into `restore-state/` and skips both
the manifest GET and the download. Any mismatch (other URI, missing file,
unreadable manifest) logs a miss and downloads as before. The copy is
replaced on each upload (old one removed in the background), left in place
after a hit, removed on terminate, and gated by `--retain-uploaded-snapshots`
(default on in this branch). Only `latest` snapshots are retained; goldens are
restored by other actors. The restore log carries
`ate.actor.restore.source=retained|download|local`.

Expected: a same-node suspend/resume wake drops from about 0.78 s to about
the pause figure (0.19 s), and suspend loses the `dirs_reset` delete of the
uploaded files. Multi-node benefit depends on the scheduler picking the same
node, which it does not try to do today (#1761/#589). Disk: one extra
snapshot per suspended actor on its last node, no cap; an upstream version
needs an age or budget GC. Measured (B4b vs B3): wake p50 1.2 s to 0.19 s, p99 3.4 s to 0.34 s,
98.4% retained hits, zero wake failures. Kept.

## Candidates to submit

(Filled in as changes prove out.)
