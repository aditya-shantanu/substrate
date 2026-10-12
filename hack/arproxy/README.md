# arproxy: per-node registry proxy for Nydus lazy loading

`arproxy` runs as a sidecar in the atelet DaemonSet (hostPort 5000) and is the registry backend every
per-actor `nydusd` on that node talks to (`atelet --nydus-backend-url=http://$(HOST_IP):5000`). It exists for
three reasons, in the order they were discovered:

1. nydusd's registry client cannot follow Artifact Registry's relative-`Location` blob redirects; the proxy
   follows them and injects the node's service-account token (metadata server; `gcloud` on a VM).
2. One proxy must serve images from several registries, so a repository may be prefixed with its registry host
   in the path: `/v2/us-east4-docker.pkg.dev/<project>/<repo>/...`.
3. Registries meter requests, not bytes. nydusd reads ~1 MiB per request (Nydus zran contexts cap read
   amplification) and a 4-6 GB image costs ~2,300 requests; 1,600 actors starting at once exceeded Artifact
   Registry's 60,000 requests/min regional quota. The proxy therefore serves blob range reads from node-local,
   aligned, single-flight **windows** (`window.go`), retries 429/5xx with backoff, caps in-flight upstream
   requests and can rate-limit per node. With 32 MiB windows the fleet made 30x fewer registry requests.

## Configuration (environment)

| variable | default | meaning |
|---|---|---|
| `ARPROXY_LISTEN` | `:5000` | listen address |
| `ARPROXY_UPSTREAM` | `https://us-east4-docker.pkg.dev` | default registry when the path carries no host |
| `ARPROXY_WINDOW_MB` | off | window size for blob range reads; 32 is what the 10K run used |
| `ARPROXY_WINDOW_CACHE_MB` | 3072 | in-memory LRU budget for windows (2048 used; give the container that much memory) |
| `ARPROXY_RATE_PER_MIN` | off | per-node token bucket for upstream attempts (1,500 used under a 60k/min quota) |
| `ARPROXY_MAX_INFLIGHT` | 48 | concurrent upstream requests |
| `ARPROXY_RETRY_BUDGET_S` | 25 | retry budget per logical request; must stay under nydusd's backend `timeout` |

Counters are logged every 30 s: requests, bytes, upstream 429/5xx, recovered-by-retry, gave-up, a Range-size
histogram, window fetches/hits/evictions, and (v9) window body retries/errors.

## Build and deploy

```
KO_DOCKER_REPO=<registry>/<repo>/arproxy KO_DEFAULTPLATFORMS=linux/amd64 ko build --bare -t v9 .
# atelet DaemonSet: add the sidecar and point nydusd at it
kubectl -n ate-system patch ds <atelet-ds> --type=json -p '[
 {"op":"add","path":"/spec/template/spec/containers/<atelet-idx>/env/-","value":{"name":"HOST_IP","valueFrom":{"fieldRef":{"fieldPath":"status.hostIP"}}}},
 {"op":"add","path":"/spec/template/spec/containers/<atelet-idx>/args/-","value":"--nydus-backend-url=http://$(HOST_IP):5000"},
 {"op":"add","path":"/spec/template/spec/containers/-","value":{"name":"arproxy","image":"<registry>/<repo>/arproxy:v9",
   "env":[{"name":"ARPROXY_LISTEN","value":":5000"},{"name":"ARPROXY_WINDOW_MB","value":"32"},{"name":"ARPROXY_WINDOW_CACHE_MB","value":"2048"},{"name":"ARPROXY_RATE_PER_MIN","value":"1500"}],
   "ports":[{"containerPort":5000,"hostPort":5000,"name":"arproxy"}],
   "resources":{"requests":{"cpu":"200m","memory":"2560Mi"}}}}]'
```

Roll the proxy only between waves of work: restarting it fails every nydusd fetch in flight on that node.

## Chunk-cache pruning

atelet's image GC covers unpacked layers only. The Nydus chunk cache (`<image-cache>/nydus-cache`) grows by
~15-30 GB per 200 distinct images per node. `nydus-cache-prune.yaml` is a DaemonSet that deletes idle blob
families (`<id>.blob.data`, `<id>.blob.data.chunk_map`, `<id>.blob.meta`) as a unit, chunk map first. Never prune
file by file: a chunk map that outlives its data file makes the next nydusd serve zeros (`exec format error`).
The right fix is to fold this into atelet's GC.

## Converting images

Zran conversion reuses the original compressed layers and only adds a bootstrap, so it is fast and does not
duplicate the data:

```
nydusify convert --source <ref> --target <ref>-zran --oci-ref --work-dir <unique-dir>
```

The target must be in the same repository as the source. Run parallel conversions with distinct work dirs.
