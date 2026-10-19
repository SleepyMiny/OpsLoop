#!/usr/bin/env bash
# Demo A: one node-failure run on the owner host (spec 8.2). Runbook and inputs: README.md in this directory.
# Needs bash and GNU coreutils (date +%N, sha256sum), curl, ssh and timedatectl. Never stores credentials.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NS=opsloop-demo
POLICY=demo-api
DEPLOY=demo-api
PRIMARY=sarmubuntuserver
LOAD_PODS=(opsloop-load-cpu opsloop-reserve-requests)
# shortcut: fixed 2 x maxSampleAge(90s) + scrape interval(30s); derive from the Policy when maxSampleAge changes (O14).
LOAD_SETTLE_SECONDS=210
TERMINAL_PHASES=" RECOVERED FAILED ABORTED SUPERSEDED UNSUPPORTED_WORKLOAD MANUAL_REQUIRED OBSERVED "

OPSLOOP_MODE="${OPSLOOP_MODE:-Enforce}"
OPSLOOP_ARTIFACT_DIR="${OPSLOOP_ARTIFACT_DIR:-./artifacts}"
read -r -a KUBECTL <<<"${OPSLOOP_KUBECTL:-}"

# Hard outer bound also covers API discovery/connection hangs (longest wait is 300s).
k() { timeout --kill-after=5s 330s "${KUBECTL[@]}" "$@"; }
now() { date -u +%FT%T.%NZ; }
log() { printf '[%s] %s\n' "$(now)" "$*" >&2; }
# Bound the whole SSH command, including a stuck systemctl; ConnectTimeout alone
# would not bound a remote command after the connection succeeds.
psh() { timeout --kill-after=5s 45s ssh -o BatchMode=yes -o ConnectTimeout=10 "$OPSLOOP_PRIMARY_SSH" "$@"; }

# JSON string literal for free-form command output (no jq on the owner host).
json_str() {
  # Quoted replacements stay literal across bash versions (5.2 patsub_replacement changed unquoted ones).
  local s=$1 b='\'
  s=${s//"$b"/"$b$b"}
  s=${s//'"'/"$b\""}
  s=${s//$'\n'/"${b}n"}
  s=${s//$'\r'/"${b}r"}
  s=${s//$'\t'/"${b}t"}
  printf '"%s"' "$s"
}

# Go duration (e.g. 10m0s, 1h0m0s, 90s) to whole seconds; anything else is rejected.
dur_seconds() {
  local d=$1 total=0
  [[ $d =~ ^([0-9]+h)?([0-9]+m)?([0-9]+s)?$ && -n $d ]] || return 1
  [[ $d =~ ([0-9]+)h ]] && total=$((total + BASH_REMATCH[1] * 3600))
  [[ $d =~ ([0-9]+)m ]] && total=$((total + BASH_REMATCH[1] * 60))
  [[ $d =~ ([0-9]+)s ]] && total=$((total + BASH_REMATCH[1]))
  echo "$total"
}

node_ready() { [[ "$(k get node "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]]; }

# True when exactly one demo-api Pod exists (none terminating) and it is Ready on the Primary.
single_primary_pod_ready() {
  local lines
  lines="$(k -n "$NS" get pods -l app.kubernetes.io/name=demo-api \
    -o jsonpath='{range .items[*]}{.spec.nodeName}|{.metadata.deletionTimestamp}|{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')" || return 1
  [[ "$lines" == "$PRIMARY||True" ]]
}

deploy_state() {
  local gen sha
  gen="$(k -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.metadata.generation}')"
  sha="$(k -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.spec}' | sha256sum | cut -d' ' -f1)"
  printf '{"generation":%s,"specSha256":"%s"}' "$gen" "$sha"
}

FAILS=()
fail() { FAILS+=("$*"); }
preflight_exit() {
  [[ ${#FAILS[@]} -eq 0 ]] && return
  printf 'preflight FAILED (cluster untouched):\n' >&2
  printf '  - %s\n' "${FAILS[@]}" | tee "$RUN_DIR/preflight-failed.txt" >&2
  exit 1
}

# ---- scenario table (spec 8.2) ----
EXPECT_NODE=""
REJECTS=()
LOADS=()
NODES=("$PRIMARY" opsloop-control)
case "${OPSLOOP_SCENARIO:-}" in
  azure-loaded)
    EXPECT_NODE=opsloop-control
    REJECTS=(opsloop-azure-worker=CPU_PRESSURE)
    LOADS=(load-azure-cpu.yaml)
    NODES+=(opsloop-azure-worker) ;;
  laptop-loaded)
    EXPECT_NODE=opsloop-azure-worker
    REJECTS=(opsloop-control=INSUFFICIENT_REQUESTED_CPU)
    LOADS=(reserve-control-requests.yaml)
    NODES+=(opsloop-azure-worker) ;;
  primary-only)
    EXPECT_NODE=opsloop-control ;;
  all-rejected)
    REJECTS=(opsloop-control=INSUFFICIENT_REQUESTED_CPU)
    LOADS=(reserve-control-requests.yaml) ;;
  *) fail "OPSLOOP_SCENARIO is not a known scenario" ;;
esac

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="$OPSLOOP_ARTIFACT_DIR/demo-a/${OPSLOOP_SCENARIO:-unknown}/$RUN_ID"
mkdir -p "$RUN_DIR"
STARTED_AT="$(now)"
INJECT_AT=""
INJECT_CMD_SECONDS=null
DEPLOY_AFTER=null
PROBE_PID=""

# ---- 1. preflight: read-only; any failure exits non-zero without touching the cluster ----
for v in OPSLOOP_KUBECTL OPSLOOP_URL OPSLOOP_PRIMARY_SSH OPSLOOP_TIME_SSH OPSLOOP_SCENARIO OPSLOOP_PLATFORM_DIGESTS \
  OPSLOOP_SOURCE_SHA OPSLOOP_IMAGE_CACHE OPSLOOP_NETPATH_CMD OPSLOOP_DRIVER; do
  [[ -n "${!v:-}" ]] || fail "$v is not set"
done
case "$OPSLOOP_MODE" in Enforce | ObserveOnly) ;; *) fail "OPSLOOP_MODE must be Enforce or ObserveOnly" ;; esac
case "${OPSLOOP_IMAGE_CACHE:-}" in warm | cold) ;; *) fail "OPSLOOP_IMAGE_CACHE must be warm or cold" ;; esac
[[ -x "${OPSLOOP_DRIVER:-}" ]] || fail "OPSLOOP_DRIVER is not an executable file"
for c in curl ssh sha256sum timedatectl timeout; do command -v "$c" >/dev/null || fail "command not found: $c"; done
[[ -f "$REPO_ROOT/deploy/owner/kustomization.yaml" ]] || fail "deploy/owner/kustomization.yaml is missing (reset needs it)"
if [[ " ${LOADS[*]} " == *" reserve-control-requests.yaml "* ]]; then
  # Trust boundary: only a plain milli-CPU value may reach the sed substitution.
  [[ "${OPSLOOP_RESERVE_CPU:-}" =~ ^[1-9][0-9]*m$ ]] || fail "OPSLOOP_RESERVE_CPU must match ^[1-9][0-9]*m$"
fi
preflight_exit

AZURE_NODE="$(k get node opsloop-azure-worker --ignore-not-found -o name)" || fail "cannot query Node opsloop-azure-worker"
if [[ "$OPSLOOP_SCENARIO" == all-rejected && -n "$AZURE_NODE" ]]; then
  REJECTS+=(opsloop-azure-worker=CPU_PRESSURE)
  LOADS+=(load-azure-cpu.yaml)
  NODES+=(opsloop-azure-worker)
fi
# primary-only is the reduced 2-node run; a third candidate would make it a FAIL round instead.
if [[ "$OPSLOOP_SCENARIO" == primary-only && -n "$AZURE_NODE" ]]; then
  fail "primary-only needs no opsloop-azure-worker Node, but it exists"
fi

for n in "${NODES[@]}"; do node_ready "$n" || fail "node $n is not Ready"; done

# O8: expected and rejected candidates must carry the Policy candidateSelector label.
for n in $EXPECT_NODE "${REJECTS[@]%%=*}"; do
  if lbl="$(k get node "$n" -o jsonpath='{.metadata.labels.opsloop\.io/recovery-candidate}')"; then
    [[ "$lbl" == true ]] || fail "node $n lacks label opsloop.io/recovery-candidate=true (O8)"
  else
    fail "cannot read labels of node $n"
  fi
done

IFS='|' read -r P_UID P_ENABLED P_MODE P_ACTIVE P_CIRCUITS P_FAILURES P_SEQ P_DEADLINE < <(k -n "$NS" get recoverypolicy "$POLICY" \
  -o jsonpath='{.metadata.uid}|{.spec.enabled}|{.spec.mode}|{.status.activeIncident.name}|{.status.circuits}|{.status.recentFailures}|{.status.incidentSequence}|{.spec.budget.incidentDeadline}{"\n"}') ||
  fail "cannot read RecoveryPolicy $NS/$POLICY"
[[ -n "${P_UID:-}" ]] || fail "policy metadata.uid is empty"
[[ "${P_ENABLED:-}" == true ]] || fail "policy spec.enabled is not true"
[[ "${P_MODE:-}" == "$OPSLOOP_MODE" ]] || fail "policy spec.mode is '${P_MODE:-}', want $OPSLOOP_MODE"
[[ -z "${P_ACTIVE:-}" ]] || fail "policy has activeIncident ${P_ACTIVE}"
[[ -z "${P_CIRCUITS:-}" || "${P_CIRCUITS}" == "[]" ]] || fail "policy has circuits"
[[ -z "${P_FAILURES:-}" || "${P_FAILURES}" == "[]" ]] || fail "policy has recentFailures"
PREV_SEQ="${P_SEQ:-0}"
POLL_SECONDS=""
if DEADLINE_S="$(dur_seconds "${P_DEADLINE:-}")"; then POLL_SECONDS=$((DEADLINE_S + 180)); else fail "cannot parse budget.incidentDeadline '${P_DEADLINE:-}'"; fi

single_primary_pod_ready || fail "want exactly one Ready demo-api Pod on $PRIMARY"
for p in "${LOAD_PODS[@]}"; do
  if out="$(k -n "$NS" get pod "$p" --ignore-not-found -o name)"; then
    [[ -z "$out" ]] || fail "load Pod $p already exists"
  else
    fail "cannot query load Pod $p"
  fi
done

if code="$(curl -sS --max-time 5 -o "$RUN_DIR/preflight-response.json" -w '%{http_code}' "$OPSLOOP_URL")"; then
  [[ "$code" == 200 ]] || fail "$OPSLOOP_URL returned HTTP $code"
  grep -q "\"node\":\"$PRIMARY\"" "$RUN_DIR/preflight-response.json" || fail "$OPSLOOP_URL is not served by $PRIMARY"
else
  fail "$OPSLOOP_URL is unreachable"
fi

TIME_SYNC="\"host\":$(json_str "$(timedatectl show -p NTPSynchronized --value 2>&1 || true)")"
[[ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" == yes ]] || fail "host clock is not NTP-synchronized"
for h in $OPSLOOP_TIME_SSH; do
  r="$(ssh -o BatchMode=yes "$h" timedatectl show -p NTPSynchronized --value 2>&1 || true)"
  TIME_SYNC+=",$(json_str "$h"):$(json_str "$r")"
  [[ "$r" == yes ]] || fail "clock of $h is not NTP-synchronized ($r)"
done

# The reset trap must never wait for a sudo password.
psh sudo -n true </dev/null || fail "ssh $OPSLOOP_PRIMARY_SSH 'sudo -n true' failed (NOPASSWD sudo needed)"
# Injection must stop k3s-agent on the Primary itself, never on another Node.
if ssh_host="$(psh hostname </dev/null)"; then
  [[ "$ssh_host" == "$PRIMARY" ]] ||
    fail "ssh $OPSLOOP_PRIMARY_SSH reaches host '$ssh_host', not Node $PRIMARY"
else
  fail "ssh $OPSLOOP_PRIMARY_SSH hostname failed"
fi

preflight_exit

# Non-DaemonSet Pods on the Primary may be evicted by the 300s NotReady taint; the owner reviews this list.
k get pods -A --field-selector "spec.nodeName=$PRIMARY" \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.metadata.ownerReferences[0].kind}{"\n"}{end}' |
  awk '$2 != "DaemonSet"' >"$RUN_DIR/primary-pods.txt"
log "non-DaemonSet Pods on $PRIMARY:"
cat "$RUN_DIR/primary-pods.txt" >&2

k version -o json >"$RUN_DIR/kubectl-version.json"
k get nodes -o json >"$RUN_DIR/nodes.json"
k -n "$NS" get recoverypolicy "$POLICY" -o json >"$RUN_DIR/policy-before.json"
IMAGE_REF="$(k -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.spec.template.spec.containers[0].image}')"
DEPLOY_BEFORE="$(deploy_state)"
NETPATH="$(bash -c "$OPSLOOP_NETPATH_CMD" 2>&1 && echo "(rc=0)" || echo "(rc=$?)")"

write_env() {
  local rejects="" r
  for r in "${REJECTS[@]}"; do rejects+="${rejects:+,}$(json_str "$r")"; done
  cat >"$RUN_DIR/env.json" <<EOF
{"schemaVersion":"demo-a-env/v1","runId":"$RUN_ID","scenario":"$OPSLOOP_SCENARIO","mode":"$OPSLOOP_MODE",
 "startedAt":"$STARTED_AT","injectAt":"$INJECT_AT","injectCmdSeconds":$INJECT_CMD_SECONDS,
 "imageCache":"$OPSLOOP_IMAGE_CACHE","netPath":$(json_str "$NETPATH"),"imageRef":$(json_str "$IMAGE_REF"),
 "expectNode":"$EXPECT_NODE","expectRejects":[$rejects],"reserveCPU":$(json_str "${OPSLOOP_RESERVE_CPU:-}"),
 "platformDigests":$(json_str "$OPSLOOP_PLATFORM_DIGESTS"),"sourceSha":$(json_str "$OPSLOOP_SOURCE_SHA"),
 "previousIncidentSequence":$PREV_SEQ,"nodeTimeSync":{$TIME_SYNC},
 "deploymentBefore":$DEPLOY_BEFORE,"deploymentAfter":$DEPLOY_AFTER,
 "notes":["An opsloop.io/attempt-id template annotation left by an earlier run is expected; reset SSA does not own it."],
 "kubectlVersion":$(cat "$RUN_DIR/kubectl-version.json"),"nodes":$(cat "$RUN_DIR/nodes.json"),"policy":$(cat "$RUN_DIR/policy-before.json")}
EOF
}
write_env

# ---- 6. recovery and reset, armed only after preflight passed ----
stop_probe() {
  local i signal pid=$PROBE_PID
  [[ -n "$pid" ]] || return 0
  # Never let a stuck probe prevent recovery. Reap only after it has exited;
  # wait itself has no timeout and could otherwise hold the reset indefinitely.
  for signal in INT TERM; do
    kill -"$signal" "$pid" 2>/dev/null || true
    for ((i = 0; i < 5; i++)); do
      if ! kill -0 "$pid" 2>/dev/null; then
        wait "$pid" 2>/dev/null || true
        PROBE_PID=""
        return 0
      fi
      sleep 1
    done
  done
  kill -KILL "$pid" 2>/dev/null || true
  PROBE_PID=""
  log "probe did not exit after INT/TERM; sent KILL; probe output may be incomplete"
  return 1
}

# Every step has its own error boundary. Evidence contains fixed step names and
# exit codes only; failure to write evidence is itself a cleanup failure.
reset_record() {
  if ! printf '%s\t%s\n' "$1" "$2" >>"$RUN_DIR/reset.tsv"; then
    RESET_FAILED=1
    log "reset: cannot write reset.tsv"
  fi
}
reset_step() {
  local step=$1 code=0
  shift
  "$@" || code=$?
  reset_record "$step" "$code"
  if ((code != 0)); then
    RESET_FAILED=1
    log "reset: $step failed (exit $code); continuing safety cleanup"
  fi
  return "$code"
}
# Every Policy mutation is fenced to the object captured by preflight and the
# exact snapshot just read. JSON Patch tests are atomic with the field change.
# Never fall back to a name-only merge patch after replacement or a conflict.
reset_patch_policy() {
  local action=$1 state uid rv enabled active deleting patch subresource=()
  [[ "${POLICY_IDENTITY_LOST:-0}" == 0 ]] || return 1
  state="$(k -n "$NS" get recoverypolicy "$POLICY" -o jsonpath='{.metadata.uid}|{.metadata.resourceVersion}|{.spec.enabled}|{.status.activeIncident.name}|{.metadata.deletionTimestamp}')" || return 1
  IFS='|' read -r uid rv enabled active deleting <<<"$state"
  if [[ -z "$P_UID" || "$uid" != "$P_UID" || -n "$deleting" ]]; then
    POLICY_IDENTITY_LOST=1
    log "reset: Policy identity changed or is deleting; refusing Policy mutations"
    return 1
  fi
  [[ "$uid" =~ ^[a-zA-Z0-9_-]+$ && "$rv" =~ ^[0-9]+$ ]] || return 1
  [[ "$enabled" == true || "$enabled" == false ]] || return 1
  patch="[{\"op\":\"test\",\"path\":\"/metadata/uid\",\"value\":\"$P_UID\"},{\"op\":\"test\",\"path\":\"/metadata/resourceVersion\",\"value\":\"$rv\"}"
  case "$action" in
    disable) patch+=',{ "op":"replace","path":"/spec/enabled","value":false}]' ;;
    enable|ledger)
      [[ "$enabled" == false && -z "$active" ]] || return 1
      patch+=',{"op":"test","path":"/spec/enabled","value":false}'
      if [[ "$action" == enable ]]; then
        patch+=',{"op":"replace","path":"/spec/enabled","value":true}]'
      else
        subresource=(--subresource=status)
        patch+=',{"op":"add","path":"/status/circuits","value":[]},{"op":"add","path":"/status/recentFailures","value":[]}]'
      fi ;;
    *) return 1 ;;
  esac
  k -n "$NS" patch recoverypolicy "$POLICY" "${subresource[@]}" --type=json -p "$patch"
}

reset_policy_disabled() {
  local state
  state="$(k -n "$NS" get recoverypolicy "$POLICY" -o jsonpath='{.metadata.uid}|{.spec.enabled}')" || return 1
  [[ "$state" == "$P_UID|false" ]]
}
reset_load_absent() {
  local pod
  pod="$(k -n "$NS" get pod "$1" --ignore-not-found -o name)" || return 1
  [[ -z "$pod" ]]
}
reset_incident_released() {
  local i state
  for ((i = 0; i < 30; i++)); do
    # Empty output on an API error is never proof of release or disablement.
    if state="$(k -n "$NS" get recoverypolicy "$POLICY" -o jsonpath='{.metadata.uid}|{.spec.enabled}|{.status.activeIncident.name}')" &&
      [[ "$state" == "$P_UID|false|" ]]; then return 0; fi
    sleep 2
  done
  return 1
}
reset_primary_pod_ready() {
  local i
  for ((i = 0; i < 60; i++)); do
    single_primary_pod_ready && return 0
    sleep 5
  done
  return 1
}
reset_enable_verified() {
  local state
  reset_patch_policy enable || return 1
  state="$(k -n "$NS" get recoverypolicy "$POLICY" -o jsonpath='{.metadata.uid}|{.spec.enabled}')" || return 1
  [[ "$state" == "$P_UID|true" ]]
}

reset_all() {
  local rc=$? pod
  trap - EXIT INT TERM
  RESET_FAILED=0
  reset_record original_exit "$rc"
  # First try to restore the Primary, even when injection failed halfway.
  # Command success alone does not prove that its service or Node is healthy.
  reset_step start_primary psh sudo -n systemctl start k3s-agent </dev/null || true

  # These safety measures are independent, including each individual load Pod.
  # Disable before waiting for Ready so a failed Primary cannot prolong Enforce.
  reset_step disable_policy reset_patch_policy disable || true
  reset_step policy_disabled reset_policy_disabled || true
  for pod in "${LOAD_PODS[@]}"; do
    reset_step "delete_$pod" k -n "$NS" delete pod "$pod" --ignore-not-found --grace-period=1 --timeout=60s || true
    reset_step "absent_$pod" reset_load_absent "$pod" || true
  done
  reset_step stop_probe stop_probe || true
  reset_step primary_service psh sudo -n systemctl is-active --quiet k3s-agent </dev/null || true
  reset_step primary_ready k wait --for=condition=Ready "node/$PRIMARY" --timeout=300s || true
  reset_step incident_released reset_incident_released || true

  # Only a verified safe baseline permits resetting workload placement/ledgers.
  # deploy/owner must exclude RecoveryPolicy, as required by its example.
  if ((RESET_FAILED == 0)); then
    reset_step apply_owner k apply --server-side --field-manager=opsloop-deploy --force-conflicts -k "$REPO_ROOT/deploy/owner" || true
    reset_step policy_still_disabled reset_policy_disabled || true
    reset_step primary_pod_ready reset_primary_pod_ready || true
  fi
  if ((RESET_FAILED == 0)); then
    reset_step clear_ledger reset_patch_policy ledger || true
  fi
  if ((RESET_FAILED == 0)); then
    # Recheck conditions after potentially slow apply/rollout/ledger operations.
    reset_step final_primary_service psh sudo -n systemctl is-active --quiet k3s-agent </dev/null || true
    reset_step final_primary_ready node_ready "$PRIMARY" || true
    reset_step final_primary_pod single_primary_pod_ready || true
    reset_step final_policy reset_incident_released || true
    for pod in "${LOAD_PODS[@]}"; do reset_step "final_absent_$pod" reset_load_absent "$pod" || true; done
  fi
  if ((RESET_FAILED == 0)); then
    reset_step enable_policy reset_enable_verified || true
  fi
  if ((RESET_FAILED != 0)); then
    # An enable request can succeed remotely but return an error locally.
    # Always try disabling again; never claim it succeeded without a readback.
    reset_step fallback_disable reset_patch_policy disable || true
    reset_step fallback_disabled reset_policy_disabled || true
    log "RESET FAILED: inspect reset.tsv; Policy disablement and Primary recovery must be confirmed before the next run."
    reset_record cleanup_exit 1
    # Preserve a prior failure/signal code; otherwise expose cleanup failure.
    ((rc != 0)) || rc=1
  else
    reset_record cleanup_exit 0
    # The final evidence write may fail too; never return success without it.
    if ((RESET_FAILED != 0)); then
      reset_step fallback_disable reset_patch_policy disable || true
      reset_step fallback_disabled reset_policy_disabled || true
      ((rc != 0)) || rc=1
    else
      log "reset: done (service, Node, workload and Policy verified)"
    fi
  fi
  exit "$rc"
}
trap reset_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- 2. load ----
if [[ ${#LOADS[@]} -gt 0 ]]; then
  for f in "${LOADS[@]}"; do
    sed "s/\${OPSLOOP_RESERVE_CPU}/${OPSLOOP_RESERVE_CPU:-}/" "$SCRIPT_DIR/$f" | k apply -f -
  done
  end=$(($(date +%s) + 60))
  for f in "${LOADS[@]}"; do
    p=opsloop-load-cpu
    [[ "$f" == reserve-control-requests.yaml ]] && p=opsloop-reserve-requests
    left=$((end - $(date +%s)))
    ((left > 0)) || { log "load Pods not Running within 60s"; exit 1; }
    k -n "$NS" wait --for=jsonpath='{.status.phase}'=Running "pod/$p" --timeout="${left}s" ||
      { log "load Pod $p not Running within 60s"; exit 1; }
  done
  log "load Running; waiting ${LOAD_SETTLE_SECONDS}s so the whole rate window is post-load"
  sleep "$LOAD_SETTLE_SECONDS"
fi

# ---- 3. probe, baseline, inject ----
"$OPSLOOP_DRIVER" probe -url "$OPSLOOP_URL" -interval 1s -timeout 2s -out "$RUN_DIR/probe.jsonl" &
PROBE_PID=$!
sleep 10
INJECT_AT="$(now)"
t0="$(date +%s.%N)"
log "inject: systemctl stop k3s-agent on $OPSLOOP_PRIMARY_SSH"
psh sudo -n systemctl stop k3s-agent </dev/null
t1="$(date +%s.%N)"
INJECT_CMD_SECONDS="$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.3f", b - a }')"

# ---- 4. wait for the new Incident to become terminal ----
INCIDENT=""
PHASE=""
TIMED_OUT=1
end=$(($(date +%s) + POLL_SECONDS))
while (($(date +%s) < end)); do
  # The uid match drops Incidents left by an earlier Policy object with the same name.
  while IFS='|' read -r name puid seq phase; do
    if [[ -n "$name" && "$puid" == "$P_UID" ]] && ((seq > PREV_SEQ)); then
      INCIDENT=$name
      PHASE=$phase
      break
    fi
  done < <(k -n "$NS" get recoveryincidents.opsloop.io -l "opsloop.io/policy=$POLICY" --sort-by=.spec.sequence \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.spec.policyRef.uid}|{.spec.sequence}|{.status.runtime.phase}{"\n"}{end}')
  [[ -n "$PHASE" && "$TERMINAL_PHASES" == *" $PHASE "* ]] && { TIMED_OUT=0; break; }
  sleep 5
done
log "incident '${INCIDENT:-none}' phase '${PHASE:-}' (timed out: $TIMED_OUT)"

# ---- 5. collect and report ----
stop_probe
k -n "$NS" get recoverypolicy "$POLICY" -o json >"$RUN_DIR/policy.json"
k -n "$NS" get pods -o json >"$RUN_DIR/pods.json"
k -n "$NS" get events --sort-by=.lastTimestamp >"$RUN_DIR/events.txt"
DEPLOY_AFTER="$(deploy_state)"
write_env
[[ -n "$INCIDENT" ]] || { log "no new Incident after sequence $PREV_SEQ"; exit 1; }
k -n "$NS" get recoveryincidents.opsloop.io "$INCIDENT" -o json >"$RUN_DIR/incident.json"

REPORT_ARGS=(-probe "$RUN_DIR/probe.jsonl" -incident "$RUN_DIR/incident.json" -pods "$RUN_DIR/pods.json"
  -inject-at "$INJECT_AT" -mode "$OPSLOOP_MODE" -platform-digests "$OPSLOOP_PLATFORM_DIGESTS"
  -expect-version "$OPSLOOP_SOURCE_SHA" -scenario "$OPSLOOP_SCENARIO" -env "$RUN_DIR/env.json" -out "$RUN_DIR/summary.json")
[[ -n "$EXPECT_NODE" ]] && REPORT_ARGS+=(-expect-node "$EXPECT_NODE")
for r in "${REJECTS[@]}"; do REPORT_ARGS+=(-expect-reject "$r"); done
"$OPSLOOP_DRIVER" report "${REPORT_ARGS[@]}"
((TIMED_OUT == 0)) || { log "Incident did not become terminal within ${POLL_SECONDS}s"; exit 1; }
grep -q '"pass": true' "$RUN_DIR/summary.json" || { log "FAIL: see $RUN_DIR/summary.json notes"; exit 1; }
log "PASS: $RUN_DIR"
