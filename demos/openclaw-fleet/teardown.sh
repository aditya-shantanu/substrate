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

# Tears down everything deploy.sh created, in dependency order: actors first
# (releases workers, deletes CSI volumes and snapshots), then templates, the
# atespace, and finally the worker pool. Deleting the pool while actors are
# awake wedges them, so order matters.
#
# Env: ATESPACE (default openclaw-fleet), BUCKET_NAME (optional: also purge
# the demo's snapshot prefix residue).

set -o errexit -o nounset -o pipefail

ATESPACE="${ATESPACE:-openclaw-fleet}"

echo "[1/4] Deleting all actors in ${ATESPACE}"
for name in $(kubectl ate get actors -a "${ATESPACE}" -o json 2>/dev/null \
  | jq -r '.actors[]?.metadata.name'); do
  kubectl ate delete actor "${name}" -a "${ATESPACE}" --any-state || true
done

echo "[2/4] Deleting actor templates"
for tmpl in $(kubectl ate get actor-templates -a "${ATESPACE}" -o json 2>/dev/null \
  | jq -r '.actorTemplates[]?.metadata.name'); do
  kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" || true
done

echo "[3/4] Deleting atespace"
kubectl ate delete atespace "${ATESPACE}" 2>/dev/null || true

echo "[4/4] Deleting worker pool + namespace + test client"
kubectl delete namespace ate-demo-openclaw-fleet --ignore-not-found
kubectl delete pod openclaw-fleet-testclient --ignore-not-found

if [ -n "${BUCKET_NAME:-}" ]; then
  echo "Purging snapshot residue under gs://${BUCKET_NAME}/openclaw-fleet/"
  gcloud storage rm -r "gs://${BUCKET_NAME}/openclaw-fleet/**" 2>/dev/null || true
fi
echo "done."
