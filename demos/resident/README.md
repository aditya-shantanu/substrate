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
  [GKE Quickstart](../../README.md#gke-quickstart), from a tree that has
  resident pools (`ActorTemplate.resident_pool`).
- `claimbench` runs as the `claimbench` ServiceAccount in `ate-demo-resident`
  and reaches the actors through the router, so the install has to admit its
  SPIFFE ID. Install, or reinstall, with it on the allow-list:

  ```bash
  ATE_INGRESS_ALLOWED_SPIFFE_IDS=spiffe://cluster.local/ns/ate-demo-resident/sa/claimbench,<existing IDs> \
    go run ./cmd/ate-setup deploy ate-system
  ```

- Worker nodes with room for every guest's residents: the default guests keep
  twenty residents of 500m CPU and 256Mi to 1.5Gi each, and the demo's
  WorkerPool asks for two workers of 12 CPU and 32Gi.
- The default `swe-rebench` guest is a 2.4 GB image in a private Artifact
  Registry of the `gke-ai-eco-dev` project. Elsewhere, pass `--guests` with
  images your nodes can pull; any digest-pinned OCI image works.

## How to run

```bash
go run ./cmd/ate-setup deploy demo resident
kubectl ate create access-policy -a ate-demo-resident -f demos/resident/claimbench-access-policy.yaml
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

"Run the experiment" at the top runs the whole measurement from the page: pick
one guest or all, set the claims per template and the pause between claims,
and watch each claim land as a cell per template, plain templates first and
then pooled. When the last template finishes the page draws the results: a
dumbbell per guest (plain against pooled claim time, p90) and the resume step
against the memory each guest holds, with a table view and the raw JSON at
`/api/experiment/last`. The last run stays on the page across reloads until
claimbench restarts.

Below that, pick a guest; each card claims from one of its templates. A bar is one actor: `CreateActor`,
`ResumeActor`, and the first HTTP response through the router. A claim that
adopted a resident is marked with a check. The dots above the bars are the pool
refilling after each claim. "One every 300 ms" is an arrival rate a pool of
six keeps up with; "back to back" drains it faster than it refills, so some
claims fall back to a restore and show as plain bars.

For numbers without the page, run `claimbench` headless and build the same
charts offline with `chart.py` (standard library only):

```bash
kubectl exec -n ate-demo-resident deploy/claimbench -- \
  /ko-app/claimbench --interval 300ms --run slim-plain=30,python-plain=30,slim-pooled=30,python-pooled=30 \
  | tee results.txt
curl -s localhost:8080/api/guests | python3 -c 'import json,sys; print(json.dumps({g["label"]: g for g in json.load(sys.stdin)}))' > guests.json
python3 demos/resident/chart.py guests.json results.txt -o report.html
```

The numbers this demo produced on a two-worker gVisor cluster of
c3-standard-22 nodes, 30 paced claims per template, p90: a pooled claim took
24 to 27 ms for the four smaller guests and 175 ms for `swe-rebench`, where
the first HTTP response on the 2.4 GB rootfs is most of it; the plain claims
took 77, 198, 238, 431 and 304 ms. The adopted resume step was 12 ms for
every guest against 68 to 349 ms for a restore.

## Clean up

```bash
go run ./cmd/ate-setup delete demo resident
```
