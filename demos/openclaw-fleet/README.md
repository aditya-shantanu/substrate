# OpenClaw Fleet on Agent Substrate: per-employee agent workspaces on GKE

This demo is a measured blueprint for operating a **fleet of per-employee
[OpenClaw](https://openclaw.ai) agent workspaces on Substrate** — the shape a
company takes when it gives every employee a personal, persistent AI-agent
workspace that must feel always-on while actually running almost never. It is
the Substrate counterpart of the
[agent-sandbox `openclaw-fleet-gke` example](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/openclaw-fleet-gke),
proving the same six fleet requirements with an asserting test script
([`run-test-gke.sh`](run-test-gke.sh)), and it builds on the single-agent
[always-on-agent](https://github.com/agent-substrate/always-on-agent)
integration (same actor image recipe; no OpenClaw source changes).

| # | Fleet requirement | Substrate mechanism | Verdict (live-tested) |
|---|---|---|---|
| 1 | Fast creation & activation; batch onboarding | `CreateActor` = a DB registration (~2 ms); first request restores the template's **golden snapshot** into a pre-warmed worker | ✅ create; ✅ activation 1.4–2.9 s for the probe workload — but ❌ **a real OpenClaw actor cannot restore at all today** (gap #1); ❌ no bulk API |
| 2 | Deletion releases every resource | `DeleteActor --any-state` frees the worker, deletes the per-actor CSI volume and every snapshot under the actor's UID prefix | ✅ verified (zero residue) — but wedges after failed restores (gap #3) |
| 3 | Sleep releases compute; wake-up measured; state preserved | `SuspendActor` checkpoints **live memory + rootfs** to GCS and frees the worker; the next request auto-resumes (atenet ext_proc) | ✅ mechanism verified — same `boot_id`, in-memory counter intact, wake p50 1.22 s; ⚠️ idle *policy* is yours to run (gap #4); ❌ blocked for OpenClaw itself (gap #1) |
| 4 | Fleet-wide image/config updates, CPU/mem resizes | none natively — ActorTemplates are **immutable**; the only path is create-template-v2 + repoint each suspended actor (`UpdateActor`), which **discards memory state** | ❌ needs Substrate work; repoint workaround verified (4–5 s downtime, volume survives, memory lost) |
| 5 | Per-employee ~20 GB NAS workspace, thousands of users | `externalVolumeTemplate` → one CSI volume per actor; with `csi-driver-nfs` each volume is a **subdirectory of one NFS/Filestore share**; excluded from snapshots, reattached on resume | ✅ verified incl. suspend/resume reattach and cross-employee isolation; ⚠️ no quota enforcement |
| 6 | Distinct, stable address per employee | `<actor>.<atespace>.actors.resources.substrate.ate.dev`, Host-routed by atenet, **wake-on-request** built in — no per-employee Service/alias objects at all | ✅ verified across create/suspend/wake/repoint — Substrate's strongest answer |

The central design difference from the Kubernetes-native (agent-sandbox)
blueprint: there, a *pod* is the unit of sleep, so waking means rescheduling
and rebooting the app (~20 s), and sub-second onboarding needs a warm pool of
already-running pods. Here, the *process image* is the unit of sleep: every
employee's agent is a suspended memory snapshot, waking resumes it
mid-thought in seconds without any pod churn, and "warm pool" is just the
worker pool every actor multiplexes onto. The trade arrives at update time:
the snapshot **is** the state, so anything that invalidates it (image bump,
resize, key rotation) is a fleet-wide state-loss event to manage.

## Architecture

```mermaid
flowchart LR
    subgraph employee [Employee]
        B[Client / channel gateway]
    end

    subgraph gke [GKE cluster - Substrate installed]
        RT[atenet-router<br/>Host-routing + CONNECT<br/>auto-resume on request]
        API[ate-api-server<br/>actors in Postgres, not CRDs]

        subgraph pool [WorkerPool openclaw-fleet - N gVisor workers]
            W1[worker: emp-alice awake<br/>OpenClaw :80 + probe :8080]
            W2[worker: idle]
        end
        NFS[csi-driver-nfs<br/>subdir per employee]
    end

    GCS[(GCS bucket<br/>golden + per-actor<br/>memory snapshots)]
    FS[(NFS / Filestore share<br/>emp-alice/ emp-bob/ ...)]

    B -- "Host: emp-alice.openclaw-fleet..." --> RT --> W1
    RT -. "ResumeActor if suspended" .-> API
    API -- suspend/resume checkpoints --> GCS
    W1 --- NFS --- FS
```

Employee lifecycle, entirely through the ate API (no portal layer needed —
compare the ~550-line portal the agent-sandbox blueprint carries):

```mermaid
stateDiagram-v2
    [*] --> Registered: CreateActor (ms, DB row only)
    Registered --> Serving: first request<br/>golden restore, seconds
    Serving --> Suspended: SuspendActor<br/>(your idle policy)
    Suspended --> Serving: any request<br/>auto-resume, same memory
    Suspended --> Suspended: UpdateActor -> template v2<br/>(fleet update; memory discarded,<br/>workspace volume kept)
    Serving --> [*]: DeleteActor --any-state<br/>volume + snapshots deleted
```

## Files

| File | Role |
|---|---|
| [`openclaw-fleet.yaml.tmpl`](openclaw-fleet.yaml.tmpl) | Namespace + `WorkerPool`: N gVisor workers = the fleet's *concurrency* ceiling (fleet size is unbounded by it). |
| [`openclaw-fleet-template.yaml.tmpl`](openclaw-fleet-template.yaml.tmpl) | The `ActorTemplate` (protojson, not a CRD): OpenClaw on port 80, test probe on 8080, FULL snapshots, 20 Gi external volume per employee. |
| [`build/`](build/) | Actor image: stock public OpenClaw + fleet config (adapted from always-on-agent). Digest-pinned base — snapshots are image-exact. |
| [`probe/`](probe/) | Test-instrumentation sidecar (Substrate has no `exec` into actors): boot ID + in-memory counter distinguish memory-restore from cold boot; workspace endpoints prove the volume. |
| [`tools/update-actor/`](tools/update-actor/) | Repoints an actor at a new template via `UpdateActor` — the RPC exists but `kubectl-ate` has no verb for it yet. |
| [`deploy.sh`](deploy.sh) | Build images (Cloud Build + ko), worker pool, atespace, template, golden snapshot wait. |
| [`run-test-gke.sh`](run-test-gke.sh) | Asserting walkthrough of requirements 1–6 (section numbering mirrors the agent-sandbox example). |
| [`rolling-update.sh`](rolling-update.sh) | Rate-limited fleet migration to a new template (the lossy update path, made explicit). |
| [`tools/measure-activation-latency.sh`](tools/measure-activation-latency.sh) | suspend/wake percentiles over N cycles. |
| [`teardown.sh`](teardown.sh) | Actors → templates → atespace → pool, in the order that avoids wedging. |

## Walkthrough

### 0. Prerequisites

A GKE cluster that Substrate can run on — the PodCertificate beta APIs must
be enabled **at cluster creation** (GKE 1.36; default-served on 1.37+):

```sh
gcloud container clusters create openclaw-fleet-sub \
  --zone us-east4-a --cluster-version 1.36.4-gke.1082000 \
  --num-nodes 3 --machine-type c3-standard-4 \
  --workload-pool <PROJECT>.svc.id.goog --enable-dataplane-v2 \
  --enable-kubernetes-unstable-apis=certificates.k8s.io/v1beta1/podcertificaterequests,certificates.k8s.io/v1beta1/clustertrustbundles
```

A snapshot bucket with Workload Identity grants for `ate-system/atelet`
**and** `ate-system/ate-api-server` (`roles/storage.objectAdmin` +
`roles/storage.bucketViewer`; missing the api-server half wedges actors on
their second suspend). Then, from this checkout:

```sh
cp hack/ate-dev-env.sh.example .ate-dev-env.sh   # edit project/cluster/bucket
hack/install-ate.sh --deploy-ate-system --setup-csi=nfs
make build-atectl        # kubectl-ate on PATH
```

`--setup-csi=nfs` deploys an in-cluster NFS server + `csi-driver-nfs` +
`CSIDriverConfig` — fine for the demo. For production, point the
`csi-nfs-sc` StorageClass at a Filestore instance instead
(`parameters: {server: <filestore-ip>, share: /<share>}`); the CSI driver
then provisions **one subdirectory per employee on one share**, the same
thousands-of-users shape as the agent-sandbox blueprint, with no per-user
PV/PVC objects.

### 1. Deploy

```sh
export PROJECT_ID=<project> BUCKET_NAME=<snapshot-bucket>
./deploy.sh                                          # template v1
TEMPLATE_NAME=openclaw-fleet-v2 TEMPLATE_REV=v2 ./deploy.sh   # v2, for the update test
```

`deploy.sh` Cloud-Builds the actor image (stock OpenClaw + config), ko-builds
the probe and the ateom worker from this checkout, applies the pool, creates
the atespace + template, and waits for the **golden snapshot** — the one-time
warm-boot of OpenClaw whose checkpoint every employee's first activation
restores from. Onboarding N employees costs N database rows, not N boots.

### 2. Run the asserting test

```sh
BUCKET_NAME=<snapshot-bucket> PROBE_ONLY=true ./run-test-gke.sh
```

`PROBE_ONLY=true` (both here and on `deploy.sh`) runs the baked-in probe
binary as PID 1 instead of OpenClaw — required until gap #1 below is fixed,
since a Node.js OpenClaw process never survives its first restore. Drop the
flag once the gVisor memory-file bug is resolved; every assertion except the
port-80 OpenClaw check is identical in both modes.

### 3. Fleet update (the honest version)

```sh
ATESPACE=openclaw-fleet ./rolling-update.sh openclaw-fleet-v2
```

Every actor is suspended, repointed, and cold-restores (data-only) on its
next request: config/image now v2, **conversation memory gone, workspace
volume intact**. There is no way to update a fleet without this loss today —
see the gaps section.

## Measured results

Live validation 14 Sep 2026: GKE `1.36.4-gke.1082000` (us-east4-a), Substrate
`release-0.1` at `v0.1.0-2-gf151db26` installed by `hack/install-ate.sh`,
worker pool of 5 on 2× `c2d-standard-8`, gVisor nightly `2026-09-02` asset,
in-cluster NFS via `--setup-csi=nfs`, 20 Gi external volume per employee.
`run-test-gke.sh`: **17/17 assertions passed** — in `PROBE_ONLY=true` mode,
because a real OpenClaw process cannot survive restore today (gap #1 below);
the lifecycle numbers are for the probe workload on the same actor image.

| Metric | Measured | Notes |
|---|---|---|
| `CreateActor` (register employee) | **~2 ms** handler; ~2.0 s wall | wall time is kubectl-ate startup + port-forward, not the control plane |
| Batch-register 10 employees | 3.2 s | no bulk API; client-side fan-out |
| First activation (golden restore → HTTP served) | 1.4–2.9 s | end-to-end through atenet |
| Wake-on-request after suspend | **p50 1.22 s, max 1.23 s** (n=5) | pure HTTP; no CLI in the path |
| Suspend (checkpoint + free worker) | p50 2.44 s wall (n=5) | includes ~1.5 s CLI overhead |
| 10 employees over 5 workers | 1.2–1.7 s per first-touch | requires caller-driven suspends (gap #4) |
| Template-v2 repoint downtime | 4.1–5.0 s | memory state discarded by design |
| Deletion | zero residue | actor list, GCS snapshot prefix verified |

For a real OpenClaw actor, add its Node.js restore cost — always-on-agent
measured ~2.9 s P50 handler / 3.3–4.7 s end-to-end for a 55–61 MiB live-memory
snapshot — once gap #1 is fixed.

## What works

- **Wake-on-request with a stable per-employee address** is built into the
  data plane: the DNS name is deterministic, survives suspend/resume/
  rescheduling, and a request to a sleeping employee just… works. The
  agent-sandbox blueprint needs a portal, alias Services, and a router
  deployment to approximate this; here it is zero per-employee objects.
- **True memory-state sleep**: the employee's agent resumes with its process
  memory (session context, JIT warm-up) intact — verified by boot-ID and
  in-memory-counter continuity across suspend/resume. Disk-tier sleep in the
  agent-sandbox blueprint reboots the app instead (~20 s wake, state only as
  good as what OpenClaw flushed to disk).
- **Fleet-size decoupling**: registered employees are DB rows; only
  *concurrently active* employees consume compute (1 awake actor : 1 worker).
- **Per-employee volumes with actor-scoped lifecycle**: provisioned at
  create, detached at suspend, deleted at delete — no PV/PVC sprawl.
- **Clean deletion**, including snapshots under the actor's UID prefix and
  the CSI volume.

## What does NOT work yet (Substrate gaps)

Ordered by how hard they bite this use case. Items 1–4 were **discovered or
confirmed during this demo's live validation**; each is reproducible with
the files in this directory.

1. **A real OpenClaw actor cannot be restored — FATAL for this use case
   today.** The golden checkpoint succeeds, but every first resume fails
   with gVisor `FATAL ERROR: ... inconsistent private memory files on
   restore: savedMFOwners = [pause:/], mfmap = map[openclaw:/ ...]` — the
   checkpoint attributes the container's private memory file to the pause
   container, restore expects it on the app container. Reproduced across:
   Intel `c3-standard-4` and AMD `c2d-standard-8` nodes; gVisor nightly
   assets `2026-09-02` and `2026-09-13`; `release-0.1` head and `c48b3a3c`;
   one and two app containers; with and without external volumes; `tini`,
   shell wrapper, or bare `node` as PID 1. Small Go binaries on the *same
   1.2 GB actor image* restore perfectly (that is what `PROBE_ONLY` mode
   exploits), so the trigger is the workload/image scale, most likely the
   overlay/memory-file "waste small" accounting in the pinned gVisor
   nightlies. Consequence: **every Node.js-class agent is unusable with
   suspend/resume on today's release-0.1 + pinned assets.** Also note the
   asset coupling: the `gvisor.tar.zstd` bundles contain Substrate-specific
   binaries (`checkpointgofer`, `gvisor_sentry`, prewarmer), exist only
   from nightly 2026-09-02 onward, so there is no older/stock runsc to pin
   as a workaround.
2. **Snapshots are CPU-feature-pinned and Substrate schedules blind to it.**
   Two of three freshly-created GKE `c3-standard-4` nodes (same zone, same
   machine type, same Xeon 8481C model!) lacked `tsc_deadline_timer`; a
   golden taken on the third node failed FeatureSet validation on the other
   two. Substrate has no CPU-feature-aware placement, no
   checkpoint-compatibility check at scheduling time, and no remediation —
   requests just 500. Worker pools must be kept feature-homogeneous by hand
   (this demo does it with node labels), and one heterogeneous node can
   poison a fleet's snapshots.
3. **Failed restores wedge, poison the pool, and end in data loss.** An
   actor whose restore fails is left `RESUMING` **while still holding its
   worker** (saturating the pool → `no free workers available` for everyone
   else), `DeleteActor --any-state` then fails with
   `TERMINAL_FILE_SYSTEM_ERROR ... sandbox-assets.json: no such file`, and
   the only recovery is deleting the worker *pod*, after which the actor is
   terminal `CRASHED` (memory state gone). There is no automatic cleanup,
   retry-with-different-worker, or fencing.
4. **No idle preemption: multiplexing is entirely caller-driven.** With 5
   workers and 11 registered employees, the 6th activation parks for its
   5 s budget and 503s — an idle-but-awake actor is never suspended to make
   room. Verified live; `run-test-gke.sh` §1b only passes because it
   suspends after every touch, exactly the loop the always-on-agent
   idle-suspender runs. Density claims assume this loop exists; ship it or
   size pools for peak-concurrent-awake.
5. **No fleet update story.** ActorTemplates are immutable, there is no
   rollout primitive, and repointing an actor (`UpdateActor`) forces a
   data-only restore — memory state is discarded fleet-wide on every image
   or config change. Resizes (`resources.limits`) are also template-bound
   and take effect only on cold boot. Roadmap lists "ActorDeployment"; until
   then [`rolling-update.sh`](rolling-update.sh) is the state of the art.
6. **Secrets are baked into the golden snapshot.** Template env is
   literal-only (no `secretKeyRef` since upstream #835); a provider-key
   rotation means new template + re-golden + lossy fleet migration. Egress
   credential injection is the missing tier.
7. **No bulk APIs.** Onboarding thousands = client-side `CreateActor`
   fan-out; offboarding likewise. Also ~2 s of CLI/port-forward overhead per
   `kubectl ate` invocation dwarfs the ~2 ms RPC — fleet tooling should hold
   one connection.
8. **No `kubectl-ate update actor`** — the RPC exists, the CLI verb doesn't
   (this demo carries [`tools/update-actor`](tools/update-actor/)).
9. **No exec/cp/debug path into actors** — hence the probe pattern this
   demo uses for testing; operators will want something first-class.
10. **Volume capacity is bookkeeping, not enforcement**, with the NFS
    driver: nothing stops an employee filling the shared share past their
    20 Gi. (Filestore multishares or an enforcing CSI driver would fix
    this.)
11. **Static worker pools.** No autoscaling; concurrent-active demand
    beyond the pool parks for ≤5 s, then 503s. Sizing is on you (see the
    always-on-agent density model: P99-peak, not average).
12. **In-cluster-only ingress.** atenet-router is ClusterIP; the external
    per-employee URL (LB, TLS, IAP/SSO) is entirely left to the operator —
    the agent-sandbox blueprint's Gateway-API layer has no counterpart
    here.
13. Minor: `hack/install-ate.sh` labels only currently-present nodes
    (nodes added later run no dataplane until labeled by hand), and a dirty
    working tree yields a `-dirty` version label that no node carries, so
    demo pools silently never schedule.

## How these gaps compare to agent-sandbox

The same six-requirement exercise was run against the
[Kubernetes SIG agent-sandbox `openclaw-fleet-gke` example](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/examples/openclaw-fleet-gke)
(Sandbox / SandboxClaim / SandboxTemplate / SandboxWarmPool CRDs), also
live-validated on GKE. Mapping the 13 gaps above onto that stack shows most
of them are the cost of Substrate's core bet — an agent as a checkpointed
process image under a bespoke control plane — while agent-sandbox's bet (an
agent as a pod under ordinary Kubernetes) inherits everything Kubernetes
already solves, and pays with a different gap list.

### The 13 gaps above, on agent-sandbox

| # | Gap here | On agent-sandbox | Why |
|---|---|---|---|
| 1 | OpenClaw can't survive gVisor restore (fatal) | Not a gap in its default tier | Native sleep is disk-tier: the pod is deleted and the app *reboots* — no checkpoint fidelity involved; OpenClaw provably survives it (22.3 s wake). Its memory tier (GKE Pod Snapshots) **fails open to a cold start**, never a crash. |
| 2 | CPU-feature-pinned snapshots, feature-blind scheduling | Same trap, memory tier only, fail-open | Same "same machine series, homogeneous pool, pod-spec-is-the-cache-key" warnings — but a mismatch silently cold-starts instead of wedging; the disk tier is immune. |
| 3 | Failed restore → wedged worker, undeletable actor, terminal CRASHED | Not a gap | No terminal states: level-triggered CRs with conditions; the claim controller falls back to cold start when a warm candidate misbehaves; pools replace bad spares. Failure degrades, nothing wedges. |
| 4 | No idle preemption; saturation parks 5 s then 503s | Half shared | Saturation degrades to a cold start (5.7 s) instead of an error. Idle *detection* is caller-driven there too (portal sweeper + absolute `shutdownTime`) — both roadmaps list auto-suspend as planned. |
| 5 | Immutable templates, lossy repoint, no rollout | Mostly solved | Templates are mutable CRs; `updateStrategy: Recreate` auto-rolls unclaimed warm spares; claimed sandboxes use a rate-limited rebuild (5.2 s downtime). Memory is lost either way, but disk/profile state survives by design. |
| 6 | Secrets baked into template + golden snapshot | Not a gap | The blueprint is a full `corev1.PodSpec`: `secretKeyRef`/`envFrom`/projected volumes; rotation = rotate the Secret. (Only fleet-shared secrets fit the warm path.) |
| 7 | No bulk APIs; ~2 s CLI overhead per call | Structurally absent | Everything is a CRD: `kubectl apply` N claims, list/watch, clientsets, Go/Python SDKs. Measured 174 ms warm claims, ~85 creates/s. |
| 8 | No `kubectl-ate update actor` verb | Structurally absent | No bespoke CLI exists to be incomplete; kubectl over CRDs is complete by construction. |
| 9 | No exec/cp/debug into actors | Not a gap | Sandbox pod = normal pod: `kubectl exec/cp/logs/debug` all work (which is why this demo needs a probe and that one doesn't). |
| 10 | Volume capacity unenforced | Shared, with an escape hatch | Its sub-second bind-mount storage shape has the identical gap, documented; but per-sandbox `volumeClaimTemplates` + Filestore multishares give enforced isolation at the cost of ~15 s cold claims. |
| 11 | Static pools, no autoscaling | Not a gap | `SandboxWarmPool` exposes `/scale` and plugs into HPA/KEDA (shipped examples); over-demand cold-starts rather than 503s. |
| 12 | ClusterIP-only ingress | Not a gap | Ships a sandbox-router (path routing, WebSockets, TokenReview authz) + Gateway API + optional IAP; only identity→URL authorization is operator glue. |
| 13 | Node-label/dataplane install footguns | Structurally absent | No node dataplane at all — one controller Deployment from release manifests; new nodes need nothing. |

Scorecard: 10 of 13 are not gaps there (the Kubernetes-native dividend), two
exist only in its optional GKE memory tier and fail open (#1, #2), one is
shared as a documented trade-off (#10).

### The gaps agent-sandbox has instead

What this demo does natively that agent-sandbox does not — plus what its own
live validation exposed:

1. **Density.** One awake employee = one full gVisor pod, 24/7; no
   multiplexing of idle-but-awake workloads (≥2,250 requested cores for
   9,000 awake seats before a keystroke, vs. actors-over-workers here).
   Multi-sandbox-per-pod is roadmap-only.
2. **Memory-true sleep isn't the platform's.** Native suspend reboots the
   app (22.3 s wake, session memory gone, vs. p50 1.22 s resume-mid-thought
   measured here); the memory tier is GKE Pod Snapshots — manual triggers,
   machine-series-pinned, template-edit-silently-cold-starts, GKE-only.
3. **Late-bind storage lifecycle is unsupported glue with a data-loss
   invariant.** Unbind-before-teardown lives only in example code; an
   out-of-band pod delete wedges pods in Terminating or wipes the
   employee's NFS workspace. No finalizer hook or binding CRD exists.
4. **No in-platform idle detection / auto-suspend / scale-to-zero** (same
   as here; sweeper lives in the example portal).
5. **Warm claims forbid all per-user pod customization**: per-claim
   env/volumes force cold starts, and there is no per-claim resource
   sizing at all — per-employee CPU/memory tiers mean a separate template
   and warm pool per size class.
6. **Sub-second is conditional**: only while the warm pool has spares
   (cold 5.7 s, first pull 27 s); refill throughput bounds batch
   onboarding; the 10k-claim fleet shape is undocumented.
7. **Stable per-user addressing is portal glue**: ExternalName alias +
   two Services per employee vs. this system's zero-object wake-on-request
   DNS — the one place Substrate is decisively ahead.
8. **Example-glue to productionize**: single-replica portal with in-memory
   state, a privileged hostPath-`/` storage daemon whose one shared token
   authorizes deleting any workspace (and whose NetworkPolicy is silently
   inert without Dataplane V2), and edge identity→sandbox authorization.

### Synthesis

The two gap lists are near-duals. Substrate's are *reliability and
operability* gaps in a young bespoke plane (fatal restore bug, terminal
states, missing verbs, no rollouts) under genuinely superior primitives
(wake-on-request addressing, memory-true sleep, heavy density).
agent-sandbox's are *economics and glue* gaps (no multiplexing, memory tier
outsourced to GKE, portal/daemon/alias layer to harden) on a platform where
failure degrades instead of wedging. For an OpenClaw-shaped fleet today,
agent-sandbox can run it in production with known glue costs; Substrate
cannot run OpenClaw at all until gap #1 is fixed — but once fixed,
Substrate's density and wake semantics attack exactly the two gaps at the
top of agent-sandbox's list.

## Teardown

```sh
BUCKET_NAME=<snapshot-bucket> ./teardown.sh
```

Order matters: actors before templates before atespace before pool —
deleting the pool under awake actors wedges them (`CRASHED` is terminal).
