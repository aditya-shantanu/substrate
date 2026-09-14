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

# Fleet-wide template migration: repoints every actor in the atespace at a new
# ActorTemplate, rate-limited. This is the closest thing Substrate has to a
# rolling update today, and it is lossy BY DESIGN: a repointed actor's next
# restore is data-only, so live memory state (in-flight conversation context)
# is discarded while durable and external volumes survive. Actors must be
# SUSPENDED to be repointed; running actors are suspended first unless
# SKIP_RUNNING=true.
#
# Usage: ATESPACE=openclaw-fleet ./rolling-update.sh <new-template-name>
# Env: RATE (actors/second, default 0.5), SKIP_RUNNING (default false).

set -o errexit -o nounset -o pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

NEW_TEMPLATE="${1:?usage: rolling-update.sh <new-template-name>}"
ATESPACE="${ATESPACE:-openclaw-fleet}"
RATE="${RATE:-0.5}"
SKIP_RUNNING="${SKIP_RUNNING:-false}"
SLEEP="$(python3 -c "print(1.0/${RATE})")"

kubectl ate get actor-template "${NEW_TEMPLATE}" -a "${ATESPACE}" >/dev/null

migrated=0 skipped=0
while read -r name state; do
  if [[ "${state}" == *RUNNING* ]]; then
    if [ "${SKIP_RUNNING}" = "true" ]; then
      echo "skip ${name} (running)"; skipped=$((skipped+1)); continue
    fi
    kubectl ate suspend actor "${name}" -a "${ATESPACE}"
  fi
  (cd "${ROOT}" && go run ./demos/openclaw-fleet/tools/update-actor \
    --atespace "${ATESPACE}" --template-ref "${NEW_TEMPLATE}" "${name}")
  migrated=$((migrated+1))
  sleep "${SLEEP}"
done < <(kubectl ate get actors -a "${ATESPACE}" -o json \
  | jq -r '.actors[] | "\(.metadata.name) \(.status.state // .state // "?")"')

echo "migrated ${migrated} actors to ${NEW_TEMPLATE}, skipped ${skipped}."
echo "NOTE: each actor cold-restores (data-only) on its next request; memory state was discarded."
