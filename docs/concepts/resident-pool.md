# Resident pools

A resident pool keeps sandboxes of an ActorTemplate restored before anyone
asks for them. A claim of a fresh actor then skips the snapshot restore: the
control plane hands the caller a sandbox that is already running and only
rebinds its identity.

## The problem

Every first activation of an actor restores the template's golden snapshot.
On a warm node that is the sandbox runtime's own restore: about 65 ms of
`runsc` for a small guest, 150 to 300 ms for a guest holding a few hundred MB,
more under Cloud Hypervisor. The node caches
([sandbox asset cache](sandbox-asset-cache.md), the shared snapshot cache) take
the downloads off that path, but not the restore itself. Workloads that create
many short-lived actors from one template, such as RL rollouts and evaluation
harnesses, pay it on every claim.

## What a resident is

A resident is an ordinary Actor in the template's atespace, created by the
control plane under a placeholder name (`rsd-<template uid prefix>-<random>`),
restored from the golden snapshot and left RUNNING on a worker. It books worker
capacity like any running actor. `ActorStatus.resident.state` is `POOLED` while
it waits.

`ActorTemplate.resident_pool.replicas` sizes the pool. The resident pool
reconciler in ate-api-server keeps that many residents per template: it
creates and resumes missing ones, deletes crashed or surplus ones, and reports
`ready` and `pending` counts in `status.resident_pool_status`. Templates with
external volumes cannot have a pool, because volumes are created per actor at
its first resume.

## The claim

`CreateActor` for a template with a pool takes one `POOLED` resident, in one
store transaction: it inserts the new actor with the resident's uid, worker
assignment, assigned node and golden snapshot, deletes the resident row, and
points the worker's assignment row at the new name. The new actor reads
`SUSPENDED`, as every new actor does, with `resident.state = RESERVED` and the
placeholder name in `resident.pooled_name`. Nothing happens on the node.

The first `ResumeActor`, explicit or the router's implicit wake, sees
`RESERVED` and skips scheduling and restore. It marks the actor RESUMING and
calls atelet `Adopt`, which calls the worker's `AdoptWorkload`. The sandbox,
its network namespace, cgroup and directories stay. The worker mints the
actor's egress certificate under the new name, retargets the DNS policy
source, registers ingress under the new name and retires the placeholder's,
and swaps the attribution its stats and crash reports carry. atelet rewrites
the system-info volume files and drops the cached worker-for-actor JWT. The
control plane then commits RUNNING with `resident.state = ADOPTED`.

If the adoption fails, the resume terminates the resident's sandbox, releases
the actor's assignment, clears the marker, and restores the actor from the
golden snapshot as if no pool existed. The pool is an optimization: a claim
never fails because of it.

## What does not change

- Actors created from an explicit `source_tag` never take a resident.
- A later suspend and resume of an adopted actor restores its own snapshot;
  residents only serve first activations.
- Residents are single-use. A sandbox that ran under one actor is never handed
  to another; threat T-27 in the threat model forbids it. Anything the guest
  obtained under the placeholder name before adoption, such as an actor JWT or
  the system-info files it read at boot, stays valid for its lifetime; the
  placeholder had no egress policy and no caller, so nothing of another
  tenant's is in it.
- A pool waits for the template's golden snapshot and is refused for
  templates that disable it, so a resident is always a restore, never a boot.
- `DeleteActor` of a reserved actor terminates the sandbox through the
  assigned node, as for any assigned actor. `DeleteActorTemplate` deletes the
  template's pooled residents first.
- A worker restart releases the reservation instead of crashing the actor; the
  first resume then restores the snapshot.

## Known gaps

- `SuspendActor` of a reserved actor that was never resumed returns at once,
  as for any SUSPENDED actor, and leaves the resident running until the
  actor is resumed or deleted.
- A resident that crashes between the claim and the first resume reports the
  crash under its placeholder name, which no longer exists. The actor stays
  SUSPENDED until its first resume finds the sandbox gone and restores the
  snapshot.
- Residents are placed by the scheduler like any other actor. There is no
  per-node target mix; a claim only needs one resident on some worker.
- Only the gVisor runtime adopts. ateworker-microvm returns Unimplemented and
  the resume falls back to a restore.
- The resident's container stdio logs keep the placeholder name; the log
  pipes are opened at start and carry their attribution by value.
