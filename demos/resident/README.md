# Resident Pool Demo

This demo shows what a [resident pool](../../docs/concepts/resident-pool.md)
does to the time a fresh actor takes from `CreateActor` to its first HTTP
response, across guest images of different sizes. For each guest it creates
two ActorTemplates: `<guest>-pooled` keeps sandboxes restored and running
ahead of claims, `<guest>-plain` restores the golden snapshot on every claim.
The guest image is any OCI image; the `counter` binary from the counter demo
is mounted into it as an image volume so the actor answers HTTP whatever the
image's own entrypoint is, and `--touch-memory-mb` makes the golden carry a
working set. `claimbench` claims actors from both templates of a guest, inside
the cluster, and serves a page that draws each claim as it happens.

The default guests run from a 10 MB distroless image to a 2.4 GB SWE-rebench
environment, holding more memory as they grow. Pass your own with
`--guests 'label|touchMiB|memoryMiB|image;...'` (images digest-pinned) and
size the pools with `--replicas`.

## Prerequisites

- A cluster with Agent Substrate installed as in the
  [Quickstart](../../README.md#quickstart-development) or
  [GKE Quickstart](../../README.md#gke-quickstart).
- `claimbench` runs as the `claimbench` ServiceAccount in `ate-demo-resident`.
  It needs the editor role on the atespace, and the router has to admit its
  SPIFFE ID, `spiffe://cluster.local/ns/ate-demo-resident/sa/claimbench`, in
  `--ingress-allowed-spiffe-ids`.

## How to run

```bash
go run ./cmd/ate-setup deploy demo resident
```

This builds the `counter` and `claimbench` images with `ko`, creates the
`ate-demo-resident` namespace with a two-worker `WorkerPool`, the two
ActorTemplates, and the `claimbench` Deployment, and waits for both golden
snapshots. The pool reconciler then fills `counter-pooled`'s pool:

```bash
kubectl ate get actor-templates -a ate-demo-resident
kubectl ate get actors -a ate-demo-resident   # the rsd-* residents, RUNNING
```

Open the page:

```bash
kubectl port-forward -n ate-demo-resident svc/claimbench 8080:8080
open http://localhost:8080
```

Pick a guest at the top; each card claims from one of its templates. A bar is one actor: `CreateActor`,
`ResumeActor`, and the first HTTP response through the router. A claim that
adopted a resident is marked with a check. The dots above the bars are the pool
refilling after each claim. "One every 300 ms" is an arrival rate a pool of
six keeps up with; "back to back" drains it faster than it refills, so some
claims fall back to a restore and show as plain bars.

For numbers without the page, run `claimbench` headless:

```bash
kubectl exec -n ate-demo-resident deploy/claimbench -- \
  /ko-app/claimbench --interval 300ms --run slim-plain=30,slim-pooled=30
```

## Clean up

```bash
go run ./cmd/ate-setup delete demo resident
```
