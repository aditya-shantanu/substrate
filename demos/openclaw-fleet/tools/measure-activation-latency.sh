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

# Suspend/wake latency percentiles over N cycles for one throwaway actor:
# the steady-state number an employee actually feels when their idle
# workspace wakes on the next message. Prints min/p50/p90/max for suspend_ms
# and wake_ms (percentile method matches the agent-sandbox example:
# floor-index over the sorted sample).
#
# Usage: ATESPACE=openclaw-fleet TEMPLATE_NAME=openclaw-fleet-v1 ./measure-activation-latency.sh [N]

set -o errexit -o nounset -o pipefail

N="${1:-5}"
ATESPACE="${ATESPACE:-openclaw-fleet}"
TEMPLATE_NAME="${TEMPLATE_NAME:-openclaw-fleet-v1}"
ACTOR_DOMAIN="actors.resources.substrate.ate.dev"
ROUTER="atenet-router.ate-system.svc.cluster.local"
CLIENT=openclaw-fleet-testclient
ACTOR="emp-bench-$(( $(date +%s) % 10000 ))"

now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }
probe() {
  kubectl exec "${CLIENT}" -- curl -s --max-time 60 --proxytunnel -x "http://${ROUTER}:8081" \
    "http://${ACTOR}.${ATESPACE}.${ACTOR_DOMAIN}:8080/state"
}

kubectl get pod "${CLIENT}" >/dev/null 2>&1 || kubectl run "${CLIENT}" \
  --image=curlimages/curl:8.10.1 --restart=Never --command -- sleep infinity
kubectl wait pod/"${CLIENT}" --for=condition=Ready --timeout=2m >/dev/null

kubectl ate create actor "${ACTOR}" -a "${ATESPACE}" --template-ref "${TEMPLATE_NAME}"
probe >/dev/null  # first activation (golden restore) is excluded: measured separately in run-test-gke.sh §1

suspends=() wakes=()
for i in $(seq 1 "${N}"); do
  t0=$(now_ms); kubectl ate suspend actor "${ACTOR}" -a "${ATESPACE}"; s=$(( $(now_ms) - t0 ))
  t0=$(now_ms); probe >/dev/null; w=$(( $(now_ms) - t0 ))
  suspends+=("${s}"); wakes+=("${w}")
  echo "cycle ${i}: suspend ${s}ms wake ${w}ms"
done
kubectl ate delete actor "${ACTOR}" -a "${ATESPACE}" --any-state

pct() { # pct <name> <values...>
  local name="$1"; shift
  printf '%s\n' "$@" | sort -n | awk -v name="${name}" '
    { v[NR]=$1 } END {
      p50=v[int((NR+1)/2)]; i=int(NR*0.9); if(i<1)i=1; p90=v[i];
      printf "%s: min %d  p50 %d  p90 %d  max %d  (n=%d)\n", name, v[1], p50, p90, v[NR], NR }'
}
pct suspend_ms "${suspends[@]}"
pct wake_ms "${wakes[@]}"
