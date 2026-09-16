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

# Asserting end-to-end walkthrough of the six fleet requirements against a
# deployed openclaw-fleet demo (see deploy.sh). Section numbering mirrors the
# agent-sandbox openclaw-fleet-gke example so results are comparable:
#   §1 fast creation/activation (+ batch, + oversubscription)
#   §2 deletion releases every resource
#   §3 sleep releases compute; wake-on-request; memory + volume state survive
#   §4 fleet update: template v2 repoint (memory reset by design, volume kept)
#   §5 per-employee external volume workspace
#   §6 stable per-employee address (implicit in every section: one DNS name)
#
# All traffic goes through atenet-router in-cluster: plain HTTP with an
# explicit Host header for OpenClaw on port 80, HTTP CONNECT for the probe on
# 8080. Requires: kubectl context on the cluster, kubectl-ate + jq + gcloud.
#
# Env: ATESPACE, TEMPLATE_NAME, TEMPLATE_NAME_V2, BUCKET_NAME (for §2's
# snapshot-residue check; skipped if unset), BATCH_N (default 10).

set -o errexit -o nounset -o pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

ATESPACE="${ATESPACE:-openclaw-fleet}"
TEMPLATE_NAME="${TEMPLATE_NAME:-openclaw-fleet-v1}"
TEMPLATE_NAME_V2="${TEMPLATE_NAME_V2:-openclaw-fleet-v2}"
BATCH_N="${BATCH_N:-10}"
ACTOR_DOMAIN="actors.resources.substrate.ate.dev"
EMP="emp-e2e-$(( $(date +%s) % 10000 ))"

PASS=0; FAIL=0
ok()   { echo "  PASS: $*"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# ---- test client: a plain curl pod; all routing decisions live in atenet ----
CLIENT=openclaw-fleet-testclient
ensure_client() {
  kubectl get pod "${CLIENT}" >/dev/null 2>&1 || kubectl run "${CLIENT}" \
    --image=curlimages/curl:8.10.1 --restart=Never --command -- sleep infinity
  kubectl wait pod/"${CLIENT}" --for=condition=Ready --timeout=2m >/dev/null
}
ROUTER="atenet-router.ate-system.svc.cluster.local"
# HTTP to the primary actor port 80: Host-header routing via the router.
actor_curl() { # actor_curl <actor> <path> [curl args...]
  local actor="$1" path="$2"; shift 2
  kubectl exec "${CLIENT}" -- curl -s --max-time 30 \
    -H "Host: ${actor}.${ATESPACE}.${ACTOR_DOMAIN}" "$@" "http://${ROUTER}:80${path}"
}
# HTTP to the probe on 8080: CONNECT-tunneled via the router.
probe_curl() { # probe_curl <actor> <path> [curl args...]
  local actor="$1" path="$2"; shift 2
  kubectl exec "${CLIENT}" -- curl -s --max-time 60 --proxytunnel \
    -x "http://${ROUTER}:8081" "$@" \
    "http://${actor}.${ATESPACE}.${ACTOR_DOMAIN}:8080${path}"
}
actor_state() { # actor_state <actor> -> e.g. ACTOR_STATE_RUNNING
  kubectl ate get actors -a "${ATESPACE}" -o json \
    | jq -r "(.actors // [])[] | select(.metadata.name==\"$1\") | .status.state // .state // empty"
}

echo "== §0 preflight"
command -v jq >/dev/null || { echo "jq required"; exit 1; }
kubectl ate get actor-template "${TEMPLATE_NAME}" -a "${ATESPACE}" >/dev/null
ensure_client
ok "template ${TEMPLATE_NAME} exists, test client ready"

echo "== §1 creation + first activation (${EMP})"
t0=$(now_ms)
kubectl ate create actor "${EMP}" -a "${ATESPACE}" --template-ref "${TEMPLATE_NAME}"
create_ms=$(( $(now_ms) - t0 ))
# Wall-clock includes ~1-2s of kubectl-ate startup + port-forward to the API
# server; the CreateActor handler itself is a DB insert. Record the handler
# time from the api-server log as the honest control-plane number.
create_rpc_ms="$(kubectl logs -n ate-system deploy/ate-api-server --since=2m 2>/dev/null \
  | jq -r "select(.msg==\"Handle RPC\" and .method==\"/ateapi.Control/CreateActor\") | .\"elapsed-time\"" 2>/dev/null | tail -1)"
[ "${create_ms}" -lt 5000 ] && ok "CreateActor wall ${create_ms}ms (CLI-dominated; handler ${create_rpc_ms:-n/a})" \
                            || fail "CreateActor took ${create_ms}ms"

# First request wakes the actor from the golden snapshot: wake-on-request IS
# the activation path; there is no separate "start" call.
t0=$(now_ms)
state_json=""
for _ in $(seq 1 60); do
  state_json="$(probe_curl "${EMP}" /state || true)"
  [ -n "${state_json}" ] && jq -e .boot_id <<<"${state_json}" >/dev/null 2>&1 && break
  sleep 1
done
activate_ms=$(( $(now_ms) - t0 ))
boot_id_1="$(jq -r .boot_id <<<"${state_json}" 2>/dev/null || true)"
[ -n "${boot_id_1}" ] && ok "first activation served in ${activate_ms}ms (boot_id ${boot_id_1})" \
                      || fail "actor never served on probe port"
# OpenClaw itself (port 80, Host-routed) must answer: any HTTP status proves
# the agent runtime is up; 401 additionally proves token auth is enforced.
# Skipped in PROBE_ONLY diagnostic mode (probe binary as PID 1, no OpenClaw
# process to answer on port 80).
if [ "${PROBE_ONLY:-false}" = "true" ]; then
  echo "  SKIP: OpenClaw port-80 check (PROBE_ONLY mode)"
else
  oc_code="$(actor_curl "${EMP}" /v1/chat/completions -o /dev/null -w '%{http_code}' -X POST || true)"
  [ "${oc_code}" != "000" ] && [ "${oc_code}" != "502" ] && [ "${oc_code}" != "504" ] \
    && ok "OpenClaw answered on port 80 (HTTP ${oc_code})" || fail "OpenClaw unreachable (HTTP ${oc_code})"
fi

echo "== §1b batch creation: ${BATCH_N} employees (workers are fewer: oversubscription)"
t0=$(now_ms)
for i in $(seq 1 "${BATCH_N}"); do
  kubectl ate create actor "${EMP}-b${i}" -a "${ATESPACE}" --template-ref "${TEMPLATE_NAME}" &
done
wait
batch_ms=$(( $(now_ms) - t0 ))
n_registered="$(kubectl ate get actors -a "${ATESPACE}" -o json | jq "[(.actors // [])[] | select(.metadata.name | startswith(\"${EMP}-b\"))] | length")"
[ "${n_registered}" -eq "${BATCH_N}" ] && ok "${BATCH_N} actors registered in ${batch_ms}ms (no bulk API: client-side fan-out)" \
                                        || fail "only ${n_registered}/${BATCH_N} actors registered"
# Touch each actor once, then suspend it — the cycle a channel gateway with
# an idle-suspender drives. The suspend is NOT optional: Substrate never
# preempts an idle-but-awake actor, so without caller-driven suspends the
# (BATCH_N+1)th activation parks for its 5s budget and 503s once the pool's
# workers are all occupied. Verified live; see the README gap list.
touch_fail=0
for i in $(seq 1 "${BATCH_N}"); do
  t1=$(now_ms)
  if probe_curl "${EMP}-b${i}" /state | jq -e .boot_id >/dev/null 2>&1; then
    echo "    ${EMP}-b${i} first-touch $(( $(now_ms) - t1 ))ms"
  else
    echo "    ${EMP}-b${i} FIRST-TOUCH FAILED"; touch_fail=$((touch_fail+1))
  fi
  kubectl ate suspend actor "${EMP}-b${i}" -a "${ATESPACE}" >/dev/null 2>&1 || true
done
[ "${touch_fail}" -eq 0 ] && ok "all ${BATCH_N} employees cycled through the smaller worker pool (touch+suspend)" \
                          || fail "${touch_fail}/${BATCH_N} actors failed first touch"

echo "== §5 per-employee workspace (external CSI volume)"
probe_curl "${EMP}" "/workspace/write?name=poc-marker.txt" -X POST -d "marker-${EMP}-${boot_id_1}" >/dev/null
marker="$(probe_curl "${EMP}" "/workspace/read?name=poc-marker.txt")"
[ "${marker}" = "marker-${EMP}-${boot_id_1}" ] && ok "workspace write/read round-trip" || fail "workspace marker mismatch: '${marker}'"
probe_curl "${EMP}" /state | jq . || true
# Isolation: another employee must not see this employee's files.
other_files="$(probe_curl "${EMP}-b1" /workspace/list | jq -r '.files | join(",")')"
[[ "${other_files}" != *poc-marker* ]] && ok "workspace is per-employee (b1 sees: '${other_files:-empty}')" \
                                       || fail "workspace leaked across employees"

echo "== §3 sleep releases compute; wake-on-request restores memory + volume"
probe_curl "${EMP}" /bump -X POST >/dev/null   # counter=1 in live memory
t0=$(now_ms)
kubectl ate suspend actor "${EMP}" -a "${ATESPACE}"
suspend_ms=$(( $(now_ms) - t0 ))
state="$(actor_state "${EMP}")"
[[ "${state}" == *SUSPEND* ]] && ok "suspend returned in ${suspend_ms}ms (state ${state}); worker freed" \
                              || fail "unexpected state after suspend: ${state}"
t0=$(now_ms)
woken="$(probe_curl "${EMP}" /state || true)"
wake_ms=$(( $(now_ms) - t0 ))
boot_id_2="$(jq -r .boot_id <<<"${woken}" 2>/dev/null || true)"
counter_2="$(jq -r .counter <<<"${woken}" 2>/dev/null || true)"
[ "${boot_id_2}" = "${boot_id_1}" ] && ok "wake-on-request in ${wake_ms}ms, SAME boot_id: live memory restored" \
                                    || fail "boot_id changed on resume (${boot_id_1} -> ${boot_id_2}): memory lost"
[ "${counter_2}" = "1" ] && ok "in-memory counter survived suspend/resume" || fail "counter after resume: ${counter_2}"
marker="$(probe_curl "${EMP}" "/workspace/read?name=poc-marker.txt")"
[ "${marker}" = "marker-${EMP}-${boot_id_1}" ] && ok "workspace volume reattached on resume" || fail "workspace lost on resume"

echo "== §4 fleet update: repoint at template v2 (${TEMPLATE_NAME_V2})"
kubectl ate get actor-template "${TEMPLATE_NAME_V2}" -a "${ATESPACE}" >/dev/null 2>&1 \
  || { echo "  SKIP: template ${TEMPLATE_NAME_V2} not deployed (deploy.sh with TEMPLATE_NAME=${TEMPLATE_NAME_V2} TEMPLATE_REV=v2)"; TEMPLATE_V2_SKIPPED=1; }
if [ -z "${TEMPLATE_V2_SKIPPED:-}" ]; then
  kubectl ate suspend actor "${EMP}" -a "${ATESPACE}" 2>/dev/null || true
  t0=$(now_ms)
  (cd "${ROOT}" && go run ./demos/openclaw-fleet/tools/update-actor \
    --atespace "${ATESPACE}" --template-ref "${TEMPLATE_NAME_V2}" "${EMP}")
  # A repointed actor restores data-only, i.e. a real cold boot of OpenClaw
  # (seconds, not a memory restore), so poll like §1 does rather than
  # trusting a single request.
  rebuilt=""
  for _ in $(seq 1 60); do
    rebuilt="$(probe_curl "${EMP}" /state || true)"
    [ -n "${rebuilt}" ] && jq -e .boot_id <<<"${rebuilt}" >/dev/null 2>&1 && break
    sleep 1
  done
  rebuild_ms=$(( $(now_ms) - t0 ))
  rev="$(jq -r .rev <<<"${rebuilt}" 2>/dev/null || true)"
  boot_id_3="$(jq -r .boot_id <<<"${rebuilt}" 2>/dev/null || true)"
  [ "${rev}" = "v2" ] && ok "actor now runs template v2 (downtime ${rebuild_ms}ms)" || fail "rev after repoint: '${rev}'"
  [ "${boot_id_3}" != "${boot_id_2}" ] && ok "boot_id changed: repoint discards memory state (DOCUMENTED Substrate behavior)" \
                                       || fail "boot_id unchanged after repoint?"
  marker="$(probe_curl "${EMP}" "/workspace/read?name=poc-marker.txt")"
  [ "${marker}" = "marker-${EMP}-${boot_id_1}" ] && ok "workspace volume survived the template update" \
                                                 || fail "workspace lost across template update"
fi

echo "== §6 stable address"
# The same DNS name (${EMP}.${ATESPACE}.${ACTOR_DOMAIN}) served every request
# above across create, suspend, wake, and template repoint — no aliasing
# layer, no per-employee Service objects. Assert it one more time end-to-end.
if probe_curl "${EMP}" /state | grep -q boot_id; then
  ok "same address still serves after full lifecycle"
else
  fail "address stopped serving after lifecycle churn"
fi

echo "== §2 deletion releases every resource"
actor_uid="$(kubectl ate get actors -a "${ATESPACE}" -o json | jq -r ".actors[] | select(.metadata.name==\"${EMP}\") | .metadata.uid")"
kubectl ate delete actor "${EMP}" -a "${ATESPACE}" --any-state
for i in $(seq 1 "${BATCH_N}"); do kubectl ate delete actor "${EMP}-b${i}" -a "${ATESPACE}" --any-state & done; wait
sleep 10
n_left="$(kubectl ate get actors -a "${ATESPACE}" -o json | jq "[(.actors // [])[] | select(.metadata.name | startswith(\"${EMP}\"))] | length")"
[ "${n_left}" -eq 0 ] && ok "all $((BATCH_N+1)) actors deleted from the control plane" || fail "${n_left} actors still listed"
if [ -n "${BUCKET_NAME:-}" ] && [ -n "${actor_uid}" ]; then
  residue="$(gcloud storage ls "gs://${BUCKET_NAME}/openclaw-fleet/**" 2>/dev/null | grep -c "${actor_uid}" || true)"
  [ "${residue}" -eq 0 ] && ok "no snapshot residue for actor uid ${actor_uid} in gs://${BUCKET_NAME}" \
                         || fail "${residue} snapshot objects left for ${actor_uid}"
fi
# CSI volume release: with csi-driver-nfs each actor volume is a subdir on the
# NFS export; count subdirs before/after externally if you need a hard check.

echo
echo "== summary: ${PASS} passed, ${FAIL} failed"
echo "   create_ms=${create_ms} activate_ms=${activate_ms} batch_ms=${batch_ms}(${BATCH_N}) suspend_ms=${suspend_ms} wake_ms=${wake_ms} rebuild_ms=${rebuild_ms:-skipped}"
[ "${FAIL}" -eq 0 ]
