# Resident Pool Demo

This demo shows what a [resident pool](../../docs/concepts/resident-pool.md)
does to the time a fresh actor takes from `CreateActor` to its first HTTP
response. Two ActorTemplates run the `counter` workload from the counter demo:
`counter-pooled` keeps six sandboxes restored and running ahead of claims,
`counter-plain` restores the golden snapshot on every claim. `claimbench`
claims actors from both, inside the cluster, and serves a page that draws each
claim as it happens.

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
kubectl ate get actors -a ate-demo-resident   # the six rsd-* residents, RUNNING
```

Open the page:

```bash
kubectl port-forward -n ate-demo-resident svc/claimbench 8080:8080
open http://localhost:8080
```

Each card claims from one template. A bar is one actor: `CreateActor`,
`ResumeActor`, and the first HTTP response through the router. A claim that
adopted a resident is marked with a check. The dots above the bars are the pool
refilling after each claim. "One every 300 ms" is an arrival rate a pool of
six keeps up with; "back to back" drains it faster than it refills, so some
claims fall back to a restore and show as plain bars.

For numbers without the page, run `claimbench` headless:

```bash
kubectl exec -n ate-demo-resident deploy/claimbench -- \
  /ko-app/claimbench --interval 300ms --run counter-plain=30,counter-pooled=30
```

## Clean up

```bash
go run ./cmd/ate-setup delete demo resident
```
