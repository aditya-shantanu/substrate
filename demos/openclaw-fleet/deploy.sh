#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Deploys the openclaw-fleet demo onto a cluster that already runs Substrate
# (hack/install-ate.sh --deploy-ate-system --setup-csi=nfs) and has kubectl-ate
# on PATH (make build-atectl).
#
# Standalone rather than an install-ate.sh demo hook because the actor image
# is a Cloud Build product (public OpenClaw base + config), not a ko build,
# and the template needs secret substitutions that must never land in a
# rendered file on disk.
#
# Required env:
#   PROJECT_ID       GCP project (Cloud Build + GCR)
#   BUCKET_NAME      GCS bucket for snapshots (no gs:// prefix)
# Optional env:
#   GEMINI_API_KEY        model key baked into the template (default: dummy;
#                         lifecycle tests pass without a real key)
#   OPENCLAW_GATEWAY_TOKEN bearer for OpenClaw's HTTP API (default: generated)
#   STORAGE_CLASS         per-employee volume class (default csi-nfs-sc)
#   WORKSPACE_CAPACITY    per-employee volume size (default 20Gi)
#   WORKER_REPLICAS       concurrency ceiling (default 5)
#   ATESPACE              default openclaw-fleet
#   TEMPLATE_NAME         default openclaw-fleet-v1
#   TEMPLATE_REV          default v1
#   ACTOR_IMAGE           skip Cloud Build, use this digest-pinned image
#   PROBE_IMAGE           skip ko build, use this digest-pinned image
#   KO_DOCKER_REPO        for the probe build (default gcr.io/$PROJECT_ID/ate-images)

set -o errexit -o nounset -o pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

die() { echo "ERROR: $*" >&2; exit 1; }

[ -n "${PROJECT_ID:-}" ] || die "Set PROJECT_ID"
[ -n "${BUCKET_NAME:-}" ] || die "Set BUCKET_NAME (snapshot bucket, no gs:// prefix)"
command -v kubectl >/dev/null || die "kubectl not found"
command -v gcloud >/dev/null || die "gcloud not found"
kubectl ate --help >/dev/null 2>&1 || die "kubectl-ate not on PATH (run: make build-atectl)"

ATESPACE="${ATESPACE:-openclaw-fleet}"
TEMPLATE_NAME="${TEMPLATE_NAME:-openclaw-fleet-v1}"
TEMPLATE_REV="${TEMPLATE_REV:-v1}"
STORAGE_CLASS="${STORAGE_CLASS:-csi-nfs-sc}"
WORKSPACE_CAPACITY="${WORKSPACE_CAPACITY:-20Gi}"
WORKER_REPLICAS="${WORKER_REPLICAS:-5}"
GEMINI_API_KEY="${GEMINI_API_KEY:-dummy-key-lifecycle-tests-need-no-model}"
OPENCLAW_GATEWAY_TOKEN="${OPENCLAW_GATEWAY_TOKEN:-$(openssl rand -hex 16)}"
export KO_DOCKER_REPO="${KO_DOCKER_REPO:-gcr.io/${PROJECT_ID}/ate-images}"

SUBSTRATE_VERSION="$(kubectl get nodes -l ate.dev/substrate-version \
  -o jsonpath='{.items[0].metadata.labels.ate\.dev/substrate-version}' 2>/dev/null || true)"
[ -n "${SUBSTRATE_VERSION}" ] || die "No node carries the ate.dev/substrate-version label; is Substrate installed?"

echo "[1/5] Actor image"
if [ -z "${ACTOR_IMAGE:-}" ]; then
  TAG="gcr.io/${PROJECT_ID}/openclaw-fleet-actor:demo"
  gcloud builds submit --project "${PROJECT_ID}" \
    --config "${DEMO_DIR}/build/cloudbuild-actor.yaml" \
    --substitutions "_IMAGE=${TAG}" "${DEMO_DIR}"
  DIGEST="$(gcloud container images describe "${TAG}" --format='value(image_summary.digest)')"
  ACTOR_IMAGE="gcr.io/${PROJECT_ID}/openclaw-fleet-actor@${DIGEST}"
fi
echo "    ${ACTOR_IMAGE}"

echo "[2/5] Probe image (ko)"
if [ -z "${PROBE_IMAGE:-}" ]; then
  command -v ko >/dev/null || die "ko not found (or set PROBE_IMAGE)"
  PROBE_IMAGE="$(cd "${ROOT}" && ko build --base-import-paths ./demos/openclaw-fleet/probe)"
fi
echo "    ${PROBE_IMAGE}"

echo "[3/5] Worker pool"
# ko resolve builds ateom-gvisor from THIS checkout, so the worker matches the
# installed control plane's version (a skewed ateom fails golden resume with
# an opaque error). Requires KO_DOCKER_REPO to be pushable.
sed -e "s|\${SUBSTRATE_VERSION}|${SUBSTRATE_VERSION}|g" \
    -e "s|\${WORKER_REPLICAS}|${WORKER_REPLICAS}|g" \
    "${DEMO_DIR}/openclaw-fleet.yaml.tmpl" \
  | (cd "${ROOT}" && ko resolve -f -) | kubectl apply -f -
kubectl -n ate-demo-openclaw-fleet wait workerpool/openclaw-fleet \
  --for=jsonpath='{.status.readyReplicas}'="${WORKER_REPLICAS}" --timeout=10m

echo "[4/5] Atespace + ActorTemplate ${TEMPLATE_NAME} (rev ${TEMPLATE_REV})"
kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
# Templates are immutable: delete-and-recreate is the only update path.
kubectl ate delete actor-template "${TEMPLATE_NAME}" -a "${ATESPACE}" 2>/dev/null || true
sed -e "s|\${ATESPACE}|${ATESPACE}|g" \
    -e "s|\${TEMPLATE_NAME}|${TEMPLATE_NAME}|g" \
    -e "s|\${TEMPLATE_REV}|${TEMPLATE_REV}|g" \
    -e "s|\${ACTOR_IMAGE}|${ACTOR_IMAGE}|g" \
    -e "s|\${PROBE_IMAGE}|${PROBE_IMAGE}|g" \
    -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
    -e "s|\${OPENCLAW_GATEWAY_TOKEN}|${OPENCLAW_GATEWAY_TOKEN}|g" \
    -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
    -e "s|\${WORKSPACE_CAPACITY}|${WORKSPACE_CAPACITY}|g" \
    -e "s|\${STORAGE_CLASS}|${STORAGE_CLASS}|g" \
    "${DEMO_DIR}/openclaw-fleet-template.yaml.tmpl" \
  | kubectl ate create actor-template -a "${ATESPACE}" -f -

echo "[5/5] Waiting for the golden snapshot (one per template, shared by every employee)"
deadline=$((SECONDS + 600))
while (( SECONDS < deadline )); do
  json="$(kubectl ate get actor-template "${TEMPLATE_NAME}" -a "${ATESPACE}" -o json 2>/dev/null || true)"
  snapshot="$(jq -r '.. | .goldenSnapshot? // empty | .snapshotUri? // empty' <<<"${json}" 2>/dev/null | head -1)"
  if [ -n "${snapshot}" ]; then echo "    golden ready: ${snapshot}"; break; fi
  err="$(jq -r '.. | .errorMessage? // empty' <<<"${json}" 2>/dev/null | head -1)"
  [ -n "${err}" ] && die "golden snapshot failed: ${err}"
  sleep 5
done
(( SECONDS < deadline )) || die "timed out waiting for the golden snapshot"

cat <<EOF

Deployed. Next steps:
  Create an employee:   kubectl ate create actor emp-alice -a ${ATESPACE} --template-ref ${TEMPLATE_NAME}
  Run the full test:    OPENCLAW_GATEWAY_TOKEN=${OPENCLAW_GATEWAY_TOKEN} ./run-test-gke.sh
EOF
