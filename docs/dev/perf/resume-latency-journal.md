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


### B6a. Three worker nodes, suspend lifecycle, C1+C2+C3, no node preference (05:42-05:52 UTC)

Worker pool grown to 3x c3-highmem-88, WorkerPool replicas 3 (one 600Gi
worker per node), 100 users, suspend, think-scale 15. Build 0ccab235.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 943 | 730 | 1400 | 1500 | 2100 | 3100 | 0 |
| SuspendActor (ms) | 1031 | 1600 | 2200 | 2500 | 3900 | 6600 | 0 |

`ate.actor.restore.source`: 265 retained (29%), 655 download. Retained
restores total 0.165 s p50; downloads 0.964 s p50 (manifest 0.055 +
download 0.726 + ateom 0.164). As predicted, the scheduler's load-only
choice (#1915) finds the copy about one time in N. Suspend is healthier than
on one node (uploads spread over three disks and three NICs): no 60 s
stalls this time.

C1 note: on a download restore the fetch leg (manifest + download) is the
longer leg, so overlapping the manifest GET with the prep leg saves nothing
here; it only helps when assets/OCI prep is cold. Kept as harmless; not a
measured win.


### B7a. Three worker nodes, suspend lifecycle, C1 to C4 (05:54-06:04 UTC)

Build 2f4d9d46 (ateapi redeployed with C4). Same configuration as B6a.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1082 | 180 | 200 | 210 | 240 | 300 | 0 |
| SuspendActor (ms) | 1180 | 1600 | 2300 | 2700 | 4000 | 9500 | 0 |

`ate.scheduler.node_preference`: 551 hit, 0 miss, 60 none (first
activations have no producing node). `ate.actor.restore.source`: 1093
retained, 101 download (the 100 first activations plus one). atelet restore
p50 0.165 s; ateapi total p50 0.180 s.

Against B6a (same nodes, no preference): wake p50 0.73 s to 0.18 s, p99
2.1 s to 0.24 s. Against the single-node B4b: slightly better at every
percentile because three nodes share the checkpoint and upload load. The
suspend/resume wake on three nodes is now indistinguishable from the pause
wake on one.


### B7b. Three worker nodes, pause lifecycle, C1 to C4 (06:06-06:15 UTC)

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 1038 | 180 | 200 | 210 | 240 | 350 | 0 |
| PauseActor (ms) | 1136 | 210 | 290 | 370 | 1200 | 2700 | 0 |

ateapi resume total p50 0.179 s, p99 0.267 s. Pause p99 drops from 2.5 s
(one node, B2) to 1.2 s with the checkpoint writes spread over three disks,
which is the disk ceiling from B1 again, not Substrate.

gVisor summary after C1 to C4, 100 actors, think-scale 15, wake p50 / p99
in ms: pause 1 node 190 / 290 (B2) to 190 / 310 (B4a); suspend 1 node
1200 / 3400 (B3) to 190 / 340 (B4b); suspend 3 nodes 730 / 2100 (B6a) to
180 / 240 (B7a); pause 3 nodes 180 / 240 (B7b). What is left in a wake is
about 165 ms of runsc create+restore and about 25 ms of Substrate.


## Micro-VM phase (bare metal)

Worker pool `uvm-metal`: 1x c3-highmem-192-metal (192 vCPU, 1.5 TiB),
UBUNTU_CONTAINERD, hyperdisk-balanced 500 GB (890 MiB/s), tainted
`ate.dev/sandboxClass=microvm`. Same build (2f4d9d46: C1 to C4), same
glutton template at 1Gi, class microvm. The gVisor pool stays up but idle.

### B8a. Micro-VM, one metal node, pause lifecycle, think-scale 15 (06:20-06:30 UTC)

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 917 | 460 | 2400 | 2900 | 4500 | 7200 | 0 |
| PauseActor (ms) | 1005 | 560 | 9000 | 11000 | 15000 | 18000 | 0 |

First 90 s, before the disk filled up: WakeFirstTouch p50 220, p95 320,
p99 490; PauseActor p50 240, p95 470.

Server side p50 / p90 / p99 (s):

| layer | total | phases |
|---|---|---|
| ateapi resume | 0.610 / 2.282 / 4.946 | atelet_restore 0.600 |
| atelet restore (local) | 0.450 / 2.390 / 4.411 | ateom_restore 0.426 / 1.784 / 3.495; sandbox_record 0.006 / 0.398 / 1.547; dirs_reset 0.001 / 0.163 / 0.413 |
| ateom-microvm restore | 0.619 / 1.105 / 1.478 | vm_restore (CH) 0.287; upper_join (wait for rootfs upper untar) 0.083; lowers (virtiofsd) 0.038; wakeup_probe 0.032; prep 0.021; vmm_launch 0.013 |
| atelet checkpoint | 0.562 / 8.935 / 14.615 | ateom_checkpoint 0.509 / 8.333 / 13.766; local_prune 0.038 / 0.342 / 1.780 |
| ateom-microvm checkpoint | 1.251 / 3.669 / 4.985 | rootfs_upper (tar) 0.825 / 2.988 / 3.889; snapshot (CH) 0.549; teardown 0.312; pause 0.005 |

Findings:

1. The same disk ceiling, hit harder. A micro-VM pause writes
   `memory-ranges` (896 MiB apparent, 428 MB allocated) plus a 46 MB
   `rootfs-upper.tar`, about 2.7x the gVisor checkpoint, so 1.9 pauses/s
   saturates the 890 MiB/s boot disk at the intended duty cycle; I/O
   pressure `full` sat at 35 to 70% through the run and every tail above
   (atelet `sandbox_record` p90 0.4 s is a single small file write) is that
   queue. Uncontended, micro-VM pause/resume is close to gVisor: 220 ms wake
   p50 in the first 90 s.
2. Substrate work on the micro-VM restore path is about 0.3 s of the 0.62 s
   p50 (prep, bundles, upper_join, lowers, vmm_launch, tap) against 0.29 s
   in cloud-hypervisor; on the checkpoint path the rootfs upper tar (0.83 s
   p50) is the critical path, longer than the CH snapshot (0.55 s) it runs
   concurrently with, and teardown adds 0.31 s before the pause returns.
   Under gVisor the equivalent Substrate share was 25 ms. This is where the
   micro-VM path differs and where Substrate-side work would pay.
3. The 46 MB rootfs upper for a glutton actor is worth a look on its own:
   it is tarred on every pause and untarred on every resume.


### B8b. Micro-VM, one metal node, suspend lifecycle, C1 to C4 (06:33-06:42 UTC)

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 800 | 470 | 3000 | 3700 | 5500 | 7600 | 0 |
| SuspendActor (ms) | 897 | 2400 | 11000 | 13000 | 16000 | 20000 | 0 |

`ate.actor.restore.source`: 752 retained, 0 download. atelet restore p50
0.468 s (ateom_restore 0.450); checkpoint p50 2.486 s = ateom_checkpoint
0.659 + persist (zstd + PUT of about 470 MB) 1.548.

The retained copy and the node preference carry over to micro-VM unchanged:
the suspend/resume wake equals the pause/resume wake (B8a: 460 ms p50) and
no restore touched GCS. Both are held at 0.46 s p50 and 3 s p90 by the
same saturated boot disk rather than by anything in the resume path.

The rootfs upper tar is the glutton script's own files: `/tmp/glutton/`
holds deps_cache 32 MB, build_artifacts 8 MB, repo_tarball 4 MB, patch.
Under gVisor those live in the sandbox's memory and ride in pages.img;
under micro-VM the rootfs upper is host-backed, so every pause tars them
and every resume untars them. For a pause (node-local by definition) the
tar and untar are avoidable: the upper dir could stay in place. That needs
ateom-microvm to know a checkpoint is local and to defer the tar to the
later upload if the actor is suspended from PAUSED; not done here.


### B8c. Control: micro-VM suspend with `--retain-uploaded-snapshots=false` (06:45-06:52 UTC)

Same as B8b with retention switched off on atelet (the flag was added to
the DaemonSet args for this run and removed afterwards).

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 777 | 1400 | 10000 | 10000 | 10000 | 10000 | 110 |
| SuspendActor (ms) | 933 | 2300 | 11000 | 23000 | 37000 | 38000 | 110 |

`ate.actor.restore.source`: 891 download, 0 retained. atelet restore p50
1.394 s, p90 11.9 s; download p50 1.03 s, p90 11.3 s (about 470 MB per
restore competing with the uploads on one NIC and one disk). The 10 s
ceiling on the wake is the router's park budget running out; those are the
110 failures. Against B8b (retention on): wake p50 1.4 s to 0.47 s, p99
over 10 s to 5.5 s, failures 110 to 0. C3 matters more on micro-VM than on
gVisor because the snapshot is 2.7x larger.


## Memory-backed per-actor state (E1, run on your go-ahead)

Setup: `mount -t tmpfs -o size=400G,mode=0700 tmpfs /var/lib/ate/actors` in
the worker node's root namespace, then the atelet pod on that node and the
WorkerPool restarted so both see it (verified in their mount tables).
Everything per actor (bundles, checkpoint-state, local-checkpoint,
retained-snapshot, restore-state) is now in RAM; the sandbox assets, image
cache and runsc logs stay on disk. gVisor pool back to one node.

### B9a. tmpfs, gVisor, pause, think-scale 5 (14:01-14:08 UTC), compare B1

| series | n | B1 p50 / p99 | B9a p50 / p90 / p99 / max | fail |
|---|---|---|---|---|
| WakeFirstTouch (ms) | 2274 | 220 / 10000 (34 fail) | 200 / 250 / 270 / 330 / 840 | 0 |
| PauseActor (ms) | 2372 | 3900 / 44000 (34 fail) | 190 / 240 / 260 / 310 / 970 | 5 |

Server side p50 / p90 / p99 (s): atelet checkpoint 0.194 / 0.234 / 0.284
(ateom_checkpoint 0.170, dirs_reset 0.023); ateom-gvisor checkpoint 0.186
(runsc checkpoint 0.133, teardown 0.051); atelet restore 0.191 / 0.243 /
0.323; ateom restore 0.190 (app_restore 0.102, pause_create 0.037). I/O
pressure on the node: zero for the whole run; tmpfs use 35 GB for 100
actors. The 5 pause failures are 2 runsc checkpoint errors plus their
follow-on FailedPreconditions, same family as before, unrelated to storage.

The runsc checkpoint itself is no faster (133 ms vs 127 ms on disk; it is
memcpy-bound). What disappeared is the queue: at 5.6 pauses/s the disk was
asked for 1 GB/s and gave 0.9; RAM does not care. The pause p99 improved
140x and the wake p99 30x at the same load, and the run that previously
collapsed is now flat.


### B9b. tmpfs, gVisor, pause, think-scale 15 (14:10-14:19 UTC), compare B4a

| series | n | B4a p50 / p90 / p99 | B9b p50 / p90 / p99 / max | fail |
|---|---|---|---|---|
| WakeFirstTouch (ms) | 946 | 190 / 240 / 310 | 180 / 210 / 610 / 920 | 0 |
| PauseActor (ms) | 1061 | 250 / 890 / 3600 | 150 / 210 / 650 / 1800 | 18 |

atelet checkpoint p50 0.186 (B4a on disk: 0.23 at the client), restore
p50 0.176. The wake p99 of 610 ms is the replacement actors' first
activations after the failures below (a golden restore with a download).

The 18 pause failures are 9 `runsc checkpoint` exit 128 plus their
follow-on FailedPreconditions, and this time they are the storage change:
the node's dmesg shows memcg OOM kills of `gvisor_sentry` with
`shmem-rss` around 310 MB inside the actor's 1 GiB cgroup. Pages written to
a tmpfs are charged as shmem to the writer's cgroup and are not
reclaimable, so an actor near its limit dies when its checkpoint is
written; on disk the same pages were reclaimable page cache (which is also
why disk checkpoints showed writeback stalls inside the limit). Upstream
#1917 (chw120, open) writes pages.img outside the actor cgroup and would
remove this; the alternative is headroom in the actor limit. The memory
accounting question is the real cost of E1, not the RAM itself (59 GB of
tmpfs for 100 actors with retained copies).


### B9c. tmpfs, gVisor, suspend, think-scale 15 (14:22-14:30 UTC), compare B4b

| series | n | B4b p50 / p90 / p99 | B9c p50 / p90 / p99 / max | fail |
|---|---|---|---|---|
| WakeFirstTouch (ms) | 813 | 190 / 250 / 340 | 180 / 210 / 240 / 840 | 0 |
| SuspendActor (ms) | 924 | 1800 / 4700 / 60000 | 1600 / 2600 / 3500 / 4100 | 12 |

atelet checkpoint p50 1.72 s = persist (zstd + GCS PUT) 1.54 + ateom
0.16 + dirs_reset 0.02; restore p50 0.177 s, all retained. The suspend tail
is now the upload alone (p99 3.5 s against 60 s on disk, though B4b also
had the GCS stall minute); the wake tail tightens further because nothing
on the node queues. The 12 suspend failures are again memcg OOM kills of
sentries writing their checkpoint into tmpfs inside the 1 GiB actor limit
(node total 30 kills across B9b and B9c), plus their follow-ons.


Leak that E1 turns from disk into RAM: after all actors of B9a to B9c were
suspended and deleted, 318 actor directories (90 GB) remained on the tmpfs,
each holding about 630 MB of `restore-state` and `checkpoint-state`. A
delete of a SUSPENDED actor has no worker and never reaches atelet, so
nothing removes the directory (upstream #1688; draft #1706 is the sweep).
On disk this is a slow leak; on a tmpfs it is memory the node never gets
back, so E1 needs that sweep (or a terminate-on-delete signal) before it
can be a default.

Layering upstream #1917 (checkpoint I/O in a sibling cgroup) onto this
branch to close that was blocked by the sandbox as untrusted code
integration, so the clean-room check is B9d below: the same run with a
2 GiB actor limit, i.e. headroom instead of isolation.


### B9d. tmpfs, gVisor, pause, think-scale 15, 2 GiB actor limit (14:32-14:40 UTC)

Same as B9b with the glutton template at 2Gi so the checkpoint's shmem fits
beside the guest inside the actor cgroup.

| series | n | p50 | p90 | p95 | p99 | max | fail |
|---|---|---|---|---|---|---|---|
| WakeFirstTouch (ms) | 875 | 180 | 200 | 210 | 230 | 250 | 0 |
| PauseActor (ms) | 973 | 150 | 210 | 220 | 240 | 260 | 0 |

ateapi resume total p50 0.179 / p99 0.225 s; ateom checkpoint p50 0.166 /
p99 0.210 (runsc 0.119). No OOM kills (node counter unchanged at 30), no
failures of any kind. This is the flattest run of the study: pause and wake
p99 within 50 ms of their p50, and the pause itself 40% faster than on disk
at the same duty cycle (B4a p50 250 / p90 890 / p99 3600).

Reading of E1 so far: memory-backed per-actor state removes the disk as a
factor entirely, for pause and for the node-side part of suspend. Its two
costs are accounting, not speed: the checkpoint's pages must not be charged
to the actor's limit (#1917, or headroom), and deleted actors' directories
must be reclaimed (#1688).


### B10a. tmpfs, micro-VM on metal, pause, think-scale 15 (14:43-14:51 UTC), compare B8a

800G tmpfs on the metal node's `/var/lib/ate/actors`, atelet and the
WorkerPool restarted onto it; glutton at 1Gi (no headroom change).

| series | n | B8a p50 / p90 / p99 | B10a p50 / p90 / p99 / max | fail |
|---|---|---|---|---|
| WakeFirstTouch (ms) | 848 | 460 / 2400 / 4500 | 310 / 450 / 580 / 700 | 0 |
| PauseActor (ms) | 946 | 560 / 9000 / 15000 | 320 / 480 / 650 / 700 | 0 |

Server side p50 / p90 / p99 (s): atelet restore 0.297 / 0.440 / 0.564;
atelet checkpoint 0.312 / 0.468 / 0.637 (local_prune 0.020);
ateom-microvm restore 0.429 (vm_restore 0.298, lowers 0.033, upper_join
0.033, wakeup_probe 0.033, vmm_launch 0.013, prep 0.011); ateom-microvm
checkpoint 0.346 (CH snapshot 0.202, teardown 0.135, rootfs_upper 0.033).
I/O pressure under 1%, no OOM kills, 44 GB of tmpfs in use.

The rootfs upper tar fell from 0.825 s to 0.033 s and the CH snapshot from
0.549 to 0.202 s once neither waited on the disk; the whole micro-VM pause
tail collapsed (p99 15 s to 0.65 s). What remains on the micro-VM restore
path is 0.30 s in cloud-hypervisor and about 0.13 s of Substrate (overlay
lowers and virtiofsd start, the upper untar wait, VMM launch, prep), and on
the checkpoint path the 0.135 s teardown that runs before the pause RPC
returns.


### B10b. tmpfs, micro-VM on metal, suspend, think-scale 15 (14:53-15:01 UTC), compare B8b

| series | n | B8b p50 / p90 / p99 | B10b p50 / p90 / p99 / max | fail |
|---|---|---|---|---|
| WakeFirstTouch (ms) | 804 | 470 / 3000 / 5500 | 300 / 430 / 620 / 2000 | 0 |
| SuspendActor (ms) | 903 | 2400 / 11000 / 16000 | 1900 / 3000 / 5000 / 9800 | 0 |

All 712 restores from the retained copy; atelet restore p50 0.296 /
p99 0.601 s; checkpoint p50 1.945 = persist 1.604 (about 470 MB zstd + PUT)
+ ateom 0.317 + dirs_reset 0.004. No OOM kills, 90 GB of tmpfs. Suspend is
now upload-bound only; the micro-VM suspend/resume wake equals its pause
wake, as on gVisor.

### E1 conclusion

Memory-backed per-actor state is the single largest improvement found for
pause latency and for every tail in this study, on both sandbox classes,
and it costs no change to the resume path. Wake p50 / p99 (ms), 100 actors,
think-scale 15, with C1 to C4 in both columns:

| path | disk | tmpfs |
|---|---|---|
| gVisor pause | 190 / 310 | 180 / 230 (2Gi limit) |
| gVisor pause, think-scale 5 stress | 220 / 10000, 34 failures | 200 / 330, 0 failures |
| gVisor suspend | 190 / 340 | 180 / 240 |
| micro-VM pause | 460 / 4500 | 310 / 580 |
| micro-VM suspend | 470 / 5500 | 300 / 620 |

Pause p50 / p99: gVisor 250 / 3600 to 150 / 240; micro-VM 560 / 15000 to
320 / 650.

To make it a Substrate feature rather than a node mount: an atelet flag
that mounts a size-capped tmpfs under its actors directory (or asks for a
memory-backed emptyDir in the atelet DaemonSet), plus the two accounting
fixes it exposes, checkpoint pages charged outside the actor cgroup (#1917)
and reclamation of deleted actors' directories (#1688, draft #1706). The
RAM budget is modest (about 0.5 to 0.9 GB per resident or recently
suspended actor here) and is the resident rung of lifecycle v2 in its
simplest form. Node sizing should then count RAM for sandboxes plus
checkpoints rather than disk bandwidth.

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

### E1 (not run): memory-backed per-actor state on the worker node

B1 showed pause is bound by the boot disk's write throughput (175 MB per
checkpoint, 890 MiB/s provisioned) and B3/B4b showed the uploaded files'
deletion costing seconds under contention. The obvious experiment is to put
`/var/lib/ate/actors` on a tmpfs (the node has 704 GiB; 100 actors hold
about 50 GB of checkpoints and retained copies) and rerun B1 and B4b. This
needs a mount in the node's root namespace; I did not do that on my own
authority. If you want it, the one-off is
`mount -t tmpfs -o size=400G tmpfs /var/lib/ate/actors` on the worker node
followed by restarting the atelet and worker pods; the Substrate-side
version would be an atelet flag that mounts a size-capped tmpfs under its
base path, which is lifecycle v2's resident rung for pause checkpoints in
its cheapest form. Alternatives that stay on disk: provision the hyperdisk
for 2400 MiB/s, or use a local-SSD machine shape for worker nodes.

### C4. ateapi: prefer the node that produced the snapshot (2f4d9d46)

The suspend workflow records the uploading node on the committed
`ExternalSnapshot` (`produced_on_node`, a hint). A SUSPENDED actor's resume
narrows the scheduler's candidates to workers on that node when any applies
and has room, keeps power-of-two-choices within that set, and falls back to
the whole fleet otherwise; paused actors keep their hard pin. Logged as
`ate.scheduler.node_preference=hit|miss|none`. This is the "affinity with
failover" of #1761/#589 in its smallest form. Expected: retained hit rate on
3 nodes from 29% to near 100%, wake p50 from 0.73 s to about 0.19 s.
Measured (B7a vs B6a): hit rate 29% to 91.5% (the rest are first
activations), wake p50 0.73 s to 0.18 s, p99 2.1 s to 0.24 s, 0 misses on
the preference itself. Kept.

## Actor sizing and snapshot sizes

What the actors were (every run above unless stated): glutton ActorTemplate
with a single resource limit, memory 1Gi (2Gi only in B9d). No CPU limit, so
the sandbox is sized to the host: under gVisor `runsc` boots with
`--cpu-num 88` (or 192 on metal) and no cgroup CPU quota; under micro-VM the
guest gets kata's default 1 vCPU and 1Gi minus the VMM reserve of guest RAM
(the memory image is 896 MiB). The coding-session script declares 96 MiB of
RAM arrays (agent_context 32Mi, compiler_ws 64Mi) and about 90 MiB of files
(ingest and write_disk), with min_actor_memory 1Gi for guest and allocator
overhead.

Snapshot sizes, measured on 2026-10-05 with a one-off run (10 actors,
suspend after every step, think-scale 1 so every step is reached within the
run) by polling the bucket for the compressed objects and reading atelet's
upload records for the uncompressed sizes:

| | gVisor | micro-VM |
|---|---|---|
| memory image, uncompressed (populated) p50 / p90 / max | pages.img 281 / 325 / 356 MB | memory-ranges 487 / 507 / 521 MB (940 MB apparent, sparse) |
| memory image, zstd object in GCS p50 / p90 / max | 259 / 305 / 325 MB | 379 / 402 / 412 MB |
| other files per snapshot | checkpoint.img 1 MB, pages_meta 12 KB | rootfs-upper.tar 84 MB raw, 78 MB zstd (p50), state.json 65 KB |
| whole snapshot in GCS p50 / p90 | 251 / 305 MB | 456 / 498 MB |
| one actor across successive suspends (zstd MB) | 147, 305, 277 | 122, 301, 300, 303, 314, 382, 382 ... |

zstd gains little on these images (gVisor 0.92, micro-VM 0.78 of populated
bytes): glutton's arrays and files are random bytes. Under gVisor the
script's files live in the sandbox's memory and ride in pages.img; under
micro-VM they are the rootfs upper and travel as a separate tar. The size
grows through the first few steps (clone, deps, first build) and plateaus
at the script's working set; the Friday runs at think-scale 15 sampled
earlier steps more often and showed a lower median (gVisor pages.img p50
215 MB, micro-VM memory-ranges populated p50 365 MB).

## Candidates to submit

In order of value. None has been opened as a PR; each is a self-contained
commit on `perf/resume-latency`.

1. **C3 + C4 together: node-local retained snapshot and same-node
   preference** (0ccab235, 2f4d9d46). Turns a suspend/resume wake into a
   pause/resume wake on the same node (6x p50, 10x p99 on gVisor; same on
   micro-VM) and removes the GCS GET from the common path. Before
   upstreaming: a size or age budget for retained copies (today one per
   suspended actor on its last node, freed only on terminate), a decision on
   the default (on in this branch), and alignment with #1551's cache
   directory and `SnapshotSharing` field so PRIVATE snapshots are explicitly
   this mechanism's job. C4 is the smallest honest answer to #1761/#589 and
   should be filed against them; it changes `ExternalSnapshot` (new optional
   field), so it needs the API owners' eyes.
2. **C0 instrumentation** (20d10da3): ateom-gvisor restore/checkpoint phase
   records, the atelet phases that close the #1646 gap (`sandbox_record`
   name shared with #1975), the ateapi `Resume timing breakdown`, and the
   router flight statistics. Independent of everything else; splits
   naturally into four PRs. The registry gained six phase values and one
   log attribute; weaver passes.
3. **C2 egress mint overlap** (590efbb5, ateom-gvisor part): small, pure
   overlap, no cost. Could ride with C0's gVisor instrumentation.
4. **C1 manifest/dial reordering** (590efbb5, atelet part): correct and
   harmless but not a measured win on these runs; submit only if the cold
   asset/OCI case is shown to benefit, or fold into #1551's restructuring.
5. **E1 as an atelet option**: memory-backed per-actor state directory
   with a size cap; depends on #1917 (checkpoint pages outside the actor
   cgroup) and an actor-directory sweep (#1688/#1706) before it can default
   on. Largest pause and tail win measured; no resume-path code change.
6. **Benchmark tooling**: `--worker-node-selector` / `--worker-toleration`
   on `benchmarking/workloads/deploy.sh`; a `boomer:stop` hook so an
   interactive stop runs the agent-session shutdown fan-out.

Not changed, reported for the backlog:

- Pause and suspend throughput per node are bound by the worker boot disk's
  write bandwidth (175 MB per gVisor checkpoint, about 470 MB per micro-VM
  checkpoint). At the intended duty cycle 100 actors on one node already
  saturate a hyperdisk at 890 MiB/s. E1 (memory-backed per-actor state,
  run as a node mount) removes it; the productized form is an atelet flag
  plus #1917 and #1688. Candidate 6 below.
- A suspend canceled by its caller's deadline leaves the actor CRASHED
  (seen under a GCS stall; #1665 family).
- A PAUSED actor cannot be deleted; the harness's shutdown has to suspend
  first.
- Micro-VM: the rootfs upper tar on the pause path and the 0.3 s teardown
  before the pause RPC returns are Substrate-side costs worth a design
  pass (keep the upper in place for local checkpoints; tear down after
  responding).
- ateapi's store work per resume is 12 ms in 13 statements; collapsing
  `finalizeRunning`'s read and merging bind + update would save a few
  milliseconds. Not worth it for latency; relevant for ateapi throughput.
- runsc offers `-direct` for checkpoint/restore and `-background` restore;
  Substrate passes neither. Out of scope by your rule, noted for later.

## Summary

Wake latency (client first byte after an idle gap), 100 agent-session
actors, think-scale 15, p50 / p99 in ms:

| path | before | after | change |
|---|---|---|---|
| gVisor pause, 1 node | 190 / 290 | 190 / 310 | none (runsc-bound) |
| gVisor suspend, 1 node | 1200 / 3400 | 190 / 340 | C3 |
| gVisor suspend, 3 nodes | 730 / 2100 | 180 / 240 | C3 + C4 |
| gVisor pause, 3 nodes | n/a | 180 / 240 | |
| micro-VM pause, 1 metal node | 460 / 4500 | same build | disk-bound |
| micro-VM suspend, 1 metal node | 1400 / 10000+ (110 failures) | 470 / 5500 | C3 + C4 |
| gVisor pause, 1 node, tmpfs (E1) | 190 / 310 | 180 / 230 | E1 |
| micro-VM pause, 1 metal node, tmpfs (E1) | 460 / 4500 | 310 / 580 | E1 |
| micro-VM suspend, 1 metal node, tmpfs (E1) | 470 / 5500 | 300 / 620 | E1 + C3 + C4 |

What a gVisor wake is made of now: about 165 ms of `runsc` (create and
restore of the pause and app containers) and about 25 ms of Substrate
(ateapi 12 ms of PostgreSQL, atelet 3 ms, ateom outside runsc 10 ms, router
under 5 ms). The Substrate share cannot get much lower without changing how
the sandbox is driven. The large remaining lever on both sandbox classes is
checkpoint write bandwidth per node, which bounds pause latency and
therefore how many actors a node can cycle.

## Tracing study (2026-10-06)

Question: can OpenTelemetry show, for one request from a user, where the
time of the wake it triggers goes, including the out-of-band parts, and
can that view exist in production without tracing every request?

### What existed before this study

Verified in Cloud Trace against the 2026-10-03 runs (the cluster exports
through the GKE managed collector). A request that Envoy's 1% root sampling
picked produced one trace from Envoy's `ingress` span through the router's
`ExtProc.RequestHeaders` and `ResumeActor`, ateapi's `step.*`, atelet's
`Restore` with every GCS range GET of the download, down to a single leaf
`ateom.Ateom/RestoreWorkload`. Gaps: ateom was opaque (90% of a pause
wake); atelet's phases were log fields, not spans; no database spans in
ateapi; no Envoy upstream span, so the actor's own time after the wake was
invisible; the egress certificate mint chain was an orphan trace; a request
joining another's flight had no link to it; parking left no events; no span
links anywhere in the tree. The suspend side is caller-rooted and was never
sampled under the benchmark, because boomer sends an explicitly unsampled
traceparent when its probability is 0 (parent-based samplers honor it).

Out-of-band work falls in three classes. Work a request waits for that
another trace drives (WorkerPool scale-up while parked): the wait is a gap
between retry attempts, and atecontroller has no trace context, so the
only stitch is actor uid plus time in logs. Work done earlier whose result
the wake consumes (the suspend that produced the snapshot, the golden
build): stitchable by recording the producing trace on the artifact and
linking. Work forked inside the request (untar goroutine, singleflight
joins): spans and links in place.

### T0a. Baseline traces, micro-VM, pause, think-scale 15 (05:54-06:04 UTC)

Build b98e7189 (C0 to C4), no code change. `trace_probability=1.0` on the
load generator produced no client traces: the boomer worker container had
no `OTEL_EXPORTER_OTLP_ENDPOINT` (every export failed with "missing
address"), and the agent-session HTTP steps open no span anyway, so the
wake requests carried no traceparent and Envoy rooted at 1%. 11 wake
traces for about 1,000 wakes.

Client: WakeFirstTouch n=997 p50 590 / p95 4400 / p99 5000 ms, 76 fail;
PauseActor n=1111 p50 550 / p95 10000 / p99 14000, 52 fail. The
micro-VM pause path on a disk-backed node at 100 actors is checkpoint-bound
as in B8a.

Span breakdown of the 11 traces (ms, p50 / p90 / p99):

| segment | p50 | p90 | p99 |
|---|---|---|---|
| ingress (Envoy) | 1056 | 2223 | 2260 |
| ingress minus ResumeActor (router) | 4.8 | 5.2 | 5.2 |
| ateapi.Control/ResumeActor minus atelet Restore | 7.5 | 8.4 | 8.7 |
| atelet Restore minus ateom RestoreWorkload | 58 | 914 | 1354 |
| ateom RestoreWorkload (opaque) | 623 | 1213 | 1326 |
| fetchFileFromGCSWithZstd (first activations) | 169 | 562 | 797 |

The 58 to 1354 ms between atelet and ateom is the copy of the local
checkpoint into the restore directory plus the download on first
activation, which the trace could not separate: exactly the gap the phase
spans below close.

### T1. Instrumentation (d6c7d832)

One commit, deployed to every component on the cluster (ateapi, atenet
router, atelet DaemonSet, ateom-microvm worker image; the gVisor worker
image follows when the WorkerPool switches class):

- `internal/phasespan`: a Sequence opens one child span per sequential
  phase, Start one per concurrent phase; names are `restore.<phase>` and
  `checkpoint.<phase>`, the phase names the log records already use.
- ateom-gvisor: every restore and checkpoint phase is a span; the egress
  mint is `restore.egress_prepare` beside the sequence. ateom-microvm: the
  same, plus `restore.untar_rootfs_upper` for the concurrent untar,
  `checkpoint.capture` with the three concurrent captures under it,
  `checkpoint.vmm_snapshot` and `checkpoint.merge_delta` inside the
  snapshot, and `wakeup_probe` split into `agent_dial`, `crng_reseed` and
  the probe proper (the log record gained the two new keys).
- atelet: restore and checkpoint phases as spans, the fetch and prep legs
  of a restore as two sequences, `checkpoint.retain` for the node-local
  copy, `checkpoint.persist` on the paused-checkpoint upload.
- `internal/ateletdial`: otelgrpc client handler, so the certificate mint
  joins the restore's trace.
- Router: Envoy `spawn_upstream_span`, so the actor's answer after the
  wake is its own span. The resume flight is a `ResumeFlight` span under
  the leader's request; attempts are its children, each parked wait an
  event with the gRPC code, the verdict an attribute; a joiner's
  `ResumeActor` span links to it (`ate.resume.joined`).
- ateapi: `ActorWake` span for every resume past the fast path. With
  `--trace-every-wake` (default on) it is a new root, always sampled by
  `serverboot.AlwaysSampleSpanNames`, linked to the request span and
  linked back from it; the request span and the `Resume timing breakdown`
  record carry `ate.wake.trace_id`. `ExternalSnapshot` gained
  `produced_by_trace_id` and `produced_by_span_id`, written by the suspend
  workflow and linked from the wake that restores the snapshot.
- `benchmarking/workloads/deploy.sh`: `ACTOR_TRACES_SAMPLER` reaches the
  glutton template, so the actor's own handler span can appear under the
  routed request.

Not done: the golden build is still untraced (the template reconciler
runs under the process context; recording its ids on the golden snapshot
is the same two fields); atecontroller has no trace context at all;
ateapi's store calls have no spans (upstream 1455); the GCS SDK's own
spans stay dev-gated. The metric registry check could not run on the
laptop (weaver's image is pulled from docker.io, which the local Docker
cannot reach); the one new attribute follows the shape of its neighbors.

Deploy notes: the atenet deploy rebuilds the envoy-dataplane image with
the in-cluster buildx builder, which must be scaled up first; the
benchmark worker container had no OTLP endpoint (set by hand on the
locust Deployment) and the system pool had no CPU headroom left for a
rolling locust restart (requests lowered to 100m).

### T1a. Instrumented build, micro-VM, pause, 100 actors, think-scale 15 (06:36-06:45 UTC)

Router root sampling at 100% (`OTEL_TRACES_SAMPLER_ARG=1.0`, mirrored into
Envoy), `--trace-every-wake` on, glutton actor sampler `parentbased_always_on`.

Client: WakeFirstTouch n=758 p50 6200 / p95 10000 ms, 175 fail (504, park
budget); PauseActor n=1002 p50 1800 / p95 12000, 175 fail (Aborted,
another operation in progress). Not a tracing effect: the first three and a
half minutes ran at the T0a rate (atelet restore p50 0.39 to 0.50 s, by
minute), then at 06:40 every disk-bound phase ballooned at once and stayed
there: restore 5 to 10 s, checkpoint 4.5 to 9 s p50 per minute. The pause
path writes about 1 GiB of sparse memory image per checkpoint plus the
rootfs upper tar; at the 2.5 pauses/s this run reached, that exceeds the
node's 890 MiB/s hyperdisk, as in B8a. T0a stayed just under the cliff for
its ten minutes. Later micro-VM runs use 50 actors.

What the traces now show, 873 complete wakes (ms, p50 / p90 / p99):

| span | p50 | p90 | p99 |
|---|---|---|---|
| ActorWake (root) | 4497 | 12540 | 17754 |
| restore.ateom_restore | 4486 | 12486 | 17702 |
| restore.vm_restore | 1405 | 3107 | 6436 |
| restore.upper_join (untar of the rootfs upper tar) | 100 | 4500 | 9244 |
| restore.lowers (overlay mounts + virtiofsd) | 400 | 1028 | 1616 |
| restore.prep | 314 | 1893 | 3507 |
| MintActorCertificate (inside prep, serial on micro-VM) | 118 | 782 | 1186 |
| restore.vmm_launch | 114 | 411 | 695 |
| restore.wakeup_probe | 106 | 1206 | 3389 |
| restore.agent_dial | 96 | 302 | 697 |
| restore.crng_reseed | 86 | 202 | 426 |
| restore.resume | 81 | 293 | 502 |
| restore.download (first activation only) | 0.4 | 594 | 759 |

Two things the breakdown exposes that the log record did not: the
certificate mint is serial on the micro-VM restore path (gVisor got C2's
overlap; micro-VM did not), 118 ms at p50 and 782 ms at p90 under load; and
the upper tar untar is the second-largest phase under disk pressure,
because the glutton writes about 90 MB of files per lap into the rootfs
upper, which the micro-VM ships as a tar in both directions.

Wake-rooted sampling, first lesson: 551 of 1424 ActorWake roots were
three-span traces for actors in CRASHED, because the router retries a
parked resume several times a second and each attempt passed the fast
path. The root is now opened only for PAUSED and SUSPENDED actors (commit
after d6c7d832); other states keep the wake nested in the request's trace.

Request side: the Envoy ingress trace now carries the upstream span
(`router actor_original_dst egress`, the actor's answer after the wake,
294 ms on the sampled 8.8 s request), the `ResumeFlight` span, and a
`ateapi.Control/ResumeActor` server span that holds only
`step.LoadActorForResume` plus the link to the wake trace. The glutton
actor's own `otelhttp` span did not arrive: the actor's exporter cannot
reach the collector from inside the sandbox on this cluster (connection
reset on the egress path), so the actor-side time stays measured from
Envoy's upstream span, which is enough for the question asked.

### T1b. Instrumented build, micro-VM, pause, 50 actors, think-scale 15 (06:51-07:01 UTC)

Same build and settings as T1a with half the actors, and
`trace_probability=1.0` on the load generator, whose worker container now
has an OTLP endpoint: its gRPC spans root the pause traces, so the
checkpoint side is traced for the first time.

Client: WakeFirstTouch n=503 p50 710 / p95 7400 / p99 10000 ms, 12 fail;
PauseActor n=562 p50 690 / p95 5200 / p99 9300, 12 fail. The tail is the
same disk queue as T1a at a lower duty.

400 complete wakes (ms, p50 / p90 / p99): ActorWake 702 / 4311 / 11144;
vm_restore 398 / 2437 / 4904; lowers 67 / 484 / 796; upper_join 51 / 517 /
4003; agent_dial 24 / 125 / 322; vmm_launch 13 / 190 / 316; prep 12 / 305 /
1196 (mint 7 / 191 / 485); crng_reseed 6 / 102 / 1008; wakeup_probe 6 /
128 / 504. At p50 the micro-VM wake is 57% reading guest memory back
(vm_restore), the rest spread over ten phases under 70 ms each; at p99
the untar of the rootfs upper and vm_restore share the blame.

A pause trace, as it now reads (one of 169 boomer-rooted ones): PauseActor
594 ms = ateapi 7 ms + atelet Checkpoint 586 ms = ateom 474 (prep 1, pause
6, capture 373 of which vmm_snapshot 373 and rootfs_upper 161 concurrent,
teardown 93) + local_prune 99 + persist 1 + dirs_reset 11. Every phase
that the log record names is a span, and the concurrent captures are
visibly concurrent.

### Side finding: a micro-VM golden does not survive a worker image change

After the worker image rollout, every actor created from the existing
`openclaw-microvm` template (golden e54f5d51, built by the previous worker
pod) hung at the wakeup probe: the guest restored, kata-agent answered,
and the gateway never served `/health` in 300 s (two attempts, oc-m3 and
oc-m4). A new template with the same spec, whose golden the new worker
built (`openclaw-microvm-t`), woke in 18.9 s on the first try. The glutton
template had been recreated by `deploy.sh` and so never showed it. Not
diagnosed; the wake trace of oc-m3 (10b2e0a0aad4d1260372efacdab91c44)
shows the restore phases complete in normal time and 300 s in
`restore.wakeup_probe`, which says the guest is up but the workload inside
it is not answering. Worth an upstream issue once reproduced on main.

### OpenClaw on micro-VM, traced (oc-m6, 07:11-07:16 UTC)

The personal-agent image (4 GiB, state dir on guest tmpfs, see the
OpenClaw snapshot notes) driven through the router with the same action
sequence as before, router at 100%, every trace below pulled from Cloud
Trace by `ate.actor.name:oc-m6`. Fifteen traces tell the whole run:

| time | trace | duration | what it says |
|---|---|---|---|
| 11:20.819 | ingress (504) | 10.0 s | the first request: ResumeFlight 37.2 s, parked past the 10 s budget, request span tagged with the wake's trace id |
| 11:20.822 | ActorWake (97 spans) | 37.2 s | download 2.6 s, vm_restore 0.84 s, wakeup_probe 33.3 s, everything else under 60 ms |
| 11:50.911 | ingress (200) | 30.5 s | the second request joined the first flight (`ate.resume.joined=true`, link to the flight span), then the upstream span is 23.4 s: the gateway's first chat turn |
| 12:31 to 12:54 | 3 ingress | 0.6 to 1.1 s | resident turns, flight 1 ms, upstream is all of it |
| 13:11 | PauseActor (24 spans) | 1.9 s | checkpoint.capture with vmm_snapshot and rootfs_upper side by side |
| 13:16 | ingress + ActorWake | 3.3 s | wake 1.5 s (vm_restore most of it) plus 1.8 s of agent time |
| 14:57 | ingress + ActorWake | 5.6 s | wake 2.3 s plus 3.3 s of agent time after 65 s paused |
| 15:40 | ingress + ActorWake | 2.2 s | wake from the retained suspend copy 1.8 s plus 0.4 s of agent time |

The 33 s in wakeup_probe on the golden wake is the gateway finishing its
own startup after the restore: the golden was taken as soon as `/health`
first answered, so the memory index build and lazy module loads land on
the first wake of every actor made from it. Earlier in the day, from a
golden the previous worker built, the same wake took 0.7 s in the probe;
the difference is where in the gateway's startup the golden caught it.
Either way the trace makes the split between Substrate (3.9 s) and the
workload (33 s) plain, which no Substrate log could.

The two SuspendActor calls were issued by kubectl-ate without `--trace`
and fell under ateapi's 10% root ratio; neither was sampled. The gVisor
run below forces them.

### Correction: the worker pod ran under a 2-CPU limit from 06:07 UTC

Found while reading T1d (gVisor, pause, 100 actors, 07:21-07:31 UTC: wake
p50 7.7 s, 295 failures, against 190 ms in B2 on Friday). The traces said
every ateom phase was 20 to 50x slower at once, including CPU-only ones
(pause_create 2.1 s p50 against 34 ms, the certificate mint 1.2 s against
5 ms), which no disk explains. The worker pod's cgroup had `cpu.max
200000 100000` and 8,073 s of throttled time, and the 278 gVisor sandbox
cgroups sit under the pod's cgroup, so 100 actors shared two cores.
`kubectl get workerpool --show-managed-fields` attributes
`spec.template.resources.limits.cpu: "2"` (and `ate.dev/kvm: 1`) to a
`kubectl-patch` at 06:06:43 UTC, not to `deploy.sh` and not to this
session. Every run from T1a on, and the OpenClaw micro-VM run, used a
worker pod created after that patch; T0a did not. The micro-VM sandboxes
run outside the pod cgroup, so there the limit hit ateom, virtiofsd and
the tar and untar of the rootfs upper, which is the T1a collapse at 06:40
(the backlog of pod-side work, then disk queueing on top), not a disk cliff
on its own. The T1a and T1b phase tables above remain correct as traces of
what happened; they are not representative of the build.

Removing the limit from the WorkerPool is a cluster change this session
could not make (denied by the auto-mode permission classifier). The
gVisor and micro-VM latency runs need repeating once it is gone:

```
kubectl --context gke_gke-ai-eco-dev_us-central1-a_resume-lat -n benchmark-workloads \
  patch workerpool benchmark-ateom --type=json \
  -p='[{"op":"remove","path":"/spec/template/resources/limits/cpu"},{"op":"remove","path":"/spec/template/resources/requests/cpu"}]'
```

### T1f. Production sampling check: router at 1%, gVisor, pause, 100 actors (07:39-07:44 UTC)

Router `OTEL_TRACES_SAMPLER_ARG=0.01` (Envoy RandomSampling 1%), load
generator `trace_probability=0` (explicitly unsampled traceparent, the
worst case for the control plane), `--trace-every-wake` on. Five minutes,
still under the 2-CPU limit, so the latencies are not the point.

| | count |
|---|---|
| routed requests (router `ResumeActor result`) | 1,260 |
| request traces in Cloud Trace (`root:ingress`) | 27 (2.1%) |
| resumes past the fast path (ateapi `Resume timing breakdown`) | 526, every one carrying `ate.wake.trace_id` |
| wake traces in Cloud Trace (`root:ActorWake`) | 532, all complete (36 spans for a pause wake, 52 with the download) |

Every wake has a complete trace while the request stream is sampled at
one in fifty, and each wake trace carries the link to the request span
that caused it (unsampled, so the link points at a span that was never
exported, which is what makes the `ate.wake.trace_id` on the log record
the other half of the join). Cost at this rate: about 60 spans per second
from wakes against 20 from requests.

### OpenClaw on gVisor, traced (oc-g6, 07:47-07:52 UTC)

Same sequence as oc-m6 on the gVisor pool (template `openclaw-gvisor-t`,
fresh golden), router at 1%, pause and suspend commands issued with
`kubectl ate --trace`, worker pod still under the 2-CPU limit. Ten traces
cover the run; the wakes are all `ActorWake` roots even though the
requests were sampled at 1%:

| time | trace | duration | phases |
|---|---|---|---|
| 47:23 | ActorWake (72 spans) | 4.9 s | download 1.66, app_restore 2.27, wakeup_probe 0.63 (request 15.6 s, the rest agent time) |
| 47:55 | PauseActor | 2.9 s | runsc checkpoint 2.67 |
| 48:01 | ActorWake | 3.3 s | app_restore 2.79, probe 0.05 |
| 48:58 | PauseActor | 0.95 s | checkpoint 0.70 |
| 49:02 | ActorWake | 4.9 s | app_restore 4.6 |
| 49:40 | PauseActor | 0.91 s | checkpoint 0.63 |
| 50:44 | ActorWake | 4.0 s | app_restore 3.5, probe 0.24 |
| 51:01 | SuspendActor (106 spans) | 2.9 s | checkpoint 0.63, teardown 0.13, persist 1.97 (pages.img upload 1.86, two small files 0.13 each, manifest 0.10), retain 0.3 ms, dirs_reset 0.12 |
| 51:24 | ActorWake | 3.7 s | from the retained copy, no download; app_restore 3.2, probe 0.31 |
| 51:41 | SuspendActor (116 spans) | 3.7 s | checkpoint 1.36, persist 1.99 |

The suspend trace is the first complete one of the night: the three
`sendFileToGCSWithZstd` uploads run concurrently under
`checkpoint.persist` and the 285 MB zstd of pages.img is 95% of it.
app_restore at 2 to 4.6 s is `runsc restore` of a 1.3 GB image on a
throttled pod; Friday's untouched number for the same image was 0.17 to
0.29 s.

Cloud Trace's read API (v1) does not return span links, so the wake to
request and wake to snapshot-producer links cannot be checked from the
API; they are written (the OTel SDK test in `wake_span_test.go` covers
them) and `ate.wake.trace_id` on the request span and the timing record
is the join that is visible everywhere.
