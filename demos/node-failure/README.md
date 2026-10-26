# Demo A — Node 장애 복구 runbook

Primary(`sarmubuntuserver`)의 kubelet을 멈춰 Node를 NotReady로 만든 뒤, OpsLoop가 resource state를 보고 다른 후보 Node로 demo-api를 옮기는지 측정한다. 이 디렉터리의 `run.sh`는 **1회**만 실행한다. 5회 반복은 아래 절차로 사람이 진행한다.

모든 단계는 소유자 호스트(Linux)에서 소유자가 실행한다. 장애 주입·부하·클러스터 쓰기는 매번 소유자의 승인 아래에서만 한다. 자격증명·token 값은 이 저장소, 명령 인자, 결과 파일 어디에도 넣지 않는다.

## 파일

| 파일 | 용도 |
| --- | --- |
| `run.sh` | preflight → 부하 → probe·주입 → Incident 대기 → 수집·report → 복구·reset |
| `load-azure-cpu.yaml` | `azure-loaded`(와 3노드 `all-rejected`): Azure worker CPU 약 90% 사용 → `CPU_PRESSURE` |
| `reserve-control-requests.yaml` | `laptop-loaded`·`all-rejected`: `opsloop-control`의 requested CPU만 채움 → `INSUFFICIENT_REQUESTED_CPU`. `${OPSLOOP_RESERVE_CPU}`를 run.sh가 치환하므로 그대로는 적용할 수 없다 |
| `../../cmd/demo-driver` | `probe`·`report`·`summarize` |

## 전제

- 소유자 호스트: Linux, bash 4.4 이상, GNU coreutils(`date +%N`, `sha256sum`), `curl`, `ssh`, `timedatectl`, `awk`, `sed`. jq는 쓰지 않는다.
- 저장소 루트에서 실행한다(`OPSLOOP_ARTIFACT_DIR` 기본값 `./artifacts`는 gitignored).
- demo-driver 빌드: `go build -o bin/demo-driver ./cmd/demo-driver` (`bin/`도 gitignored).
- `deploy/owner/kustomization.yaml`(gitignored, O9에서 작성)이 있어야 한다. reset이 이것을 다시 적용한다.
- Primary ssh 계정은 NOPASSWD sudo가 **아래 세 명령으로만** 제한돼 있어야 한다. 경로는 `command -v systemctl`, `command -v true`로 확인해 맞춘다.

  ```
  # /etc/sudoers.d/opsloop-demo (visudo -f 로 편집)
  <ssh 사용자> ALL=(root) NOPASSWD: /usr/bin/systemctl stop k3s-agent, /usr/bin/systemctl start k3s-agent, /usr/bin/true
  ```

- 처음 1회: `systemctl stop k3s-agent` 뒤 실행 중 컨테이너가 살아남는지(k3s unit `KillMode`) 확인하고 기록한다(U5). 살아남으면 구 Pod가 남는 partition 유사 상황이 되고 summary `oldPodsOutstanding`에 기록된다.

## 입력 환경변수

| 변수 | 예 / 의미 |
| --- | --- |
| `OPSLOOP_KUBECTL` | `kubectl`(클러스터 ±1 minor) 또는 `k3s kubectl` |
| `OPSLOOP_URL` | `http://100.73.4.76:30080/` |
| `OPSLOOP_PRIMARY_SSH` | Primary ssh 대상(예 `sarmubuntuserver`). 주입 `sudo -n systemctl stop k3s-agent`, 복구 `... start k3s-agent` |
| `OPSLOOP_TIME_SSH` | 시계 동기화 확인 대상(공백 구분). controller가 도는 `opsloop-control`과 후보 Node 전부. 3노드면 `opsloop-control opsloop-azure-worker` |
| `OPSLOOP_SCENARIO` | `azure-loaded` / `laptop-loaded` / `primary-only` / `all-rejected` |
| `OPSLOOP_MODE` | 기본 `Enforce`. O10에서만 `ObserveOnly` |
| `OPSLOOP_RESERVE_CPU` | `laptop-loaded`·`all-rejected`에서 필수. 형식 `^[1-9][0-9]*m$`(예 `3300m`). 계산은 아래 |
| `OPSLOOP_PLATFORM_DIGESTS` | CI step summary의 플랫폼별 digest(쉼표 구분) |
| `OPSLOOP_SOURCE_SHA` | CI step summary의 source SHA(배포 index digest를 만든 commit) |
| `OPSLOOP_IMAGE_CACHE` | `warm` / `cold`(운영자가 사전 상태를 선언) |
| `OPSLOOP_NETPATH_CMD` | 예 `tailscale ping -c 1 opsloop-azure-worker`(출력과 rc를 netPath로 기록) |
| `OPSLOOP_ARTIFACT_DIR` | 기본 `./artifacts` |
| `OPSLOOP_DRIVER` | demo-driver 실행 파일 경로(예 `bin/demo-driver`) |

`OPSLOOP_RESERVE_CPU = opsloop-control의 allocatable cpu - 기존 requests - 100m`. allocatable은 하드코딩하지 말고 `kubectl get node opsloop-control -o jsonpath='{.status.allocatable.cpu}'`로 읽는다. 기존 requests는 `kubectl describe node opsloop-control`의 Allocated resources 또는 직전 decision의 `requestedCPUMilli`. 이 값이면 예약 Pod는 스케줄되고 `requested + 50m(demo) + reserve 10% > allocatable`이 된다. 실제로 그 이유로 reject됐는지는 report의 expect-reject가 판정한다.

## 시나리오별 기대값 (run.sh 안에 고정)

| 시나리오 | 노드 | 부하 | expect-node | expect-reject |
| --- | --- | --- | --- | --- |
| `azure-loaded` | 3 | load-azure-cpu | `opsloop-control` | `opsloop-azure-worker=CPU_PRESSURE` |
| `laptop-loaded` | 3 | reserve-control-requests | `opsloop-azure-worker`(servedArch arm64) | `opsloop-control=INSUFFICIENT_REQUESTED_CPU` |
| `primary-only`(축소) | 2 | 없음 | `opsloop-control` | 없음 |
| `all-rejected`(부정) | 2 또는 3 | reserve(+3노드면 load-azure-cpu) | 없음 | `opsloop-control=INSUFFICIENT_REQUESTED_CPU`(+3노드면 `opsloop-azure-worker=CPU_PRESSURE`) |

`all-rejected`는 기본 deadline 10m을 그대로 써서 FAILED `DEADLINE_EXCEEDED`, attempts 0, `NO_CANDIDATE`를 기대한다. 300s 뒤 eviction이 일어나도 WAITING이 유지돼야 한다.

## run.sh 1회 실행 순서와 규칙

1. **preflight(읽기 전용)**: 입력 형식, 기대 Node Ready, expect-node·expect-reject Node의 `opsloop.io/recovery-candidate=true` label(O8), `primary-only`면 `opsloop-azure-worker` Node 부재, Policy `enabled=true`·`mode`·activeIncident/circuits/recentFailures 없음, demo-api Pod 정확히 1개가 Primary에서 Ready, `OPSLOOP_URL` 200이고 `node == sarmubuntuserver`, Host와 `OPSLOOP_TIME_SSH` 대상의 `NTPSynchronized == yes`, `sudo -n true` 성공, `ssh $OPSLOOP_PRIMARY_SSH hostname`이 Primary Node 이름과 같음, 부하 Pod 잔존 없음. kubectl 조회 자체가 실패해도 preflight 실패다. 하나라도 실패하면 실패 항목만 출력하고(`preflight-failed.txt`) **클러스터를 바꾸지 않고 reset도 하지 않은 채** 비0 종료한다. 진행 중인 실제 Incident나 circuit을 reset이 덮어 이상 상태를 지우지 않게 하기 위해서다. **preflight 실패는 FAIL 회차로 세지 않는다.** 원인을 해결하고 다시 실행한다.
2. preflight 통과 후 Primary의 비-DaemonSet Pod 목록(`primary-pods.txt`, 300s 뒤 taint eviction 대상일 수 있음)과 `env.json`을 남기고, **이 시점에만** reset trap(EXIT·INT·TERM)을 건다.
3. 부하 적용 → 부하·예약 Pod 모두 Running까지 최대 60s(아니면 중단, trap이 reset) → 210s 대기(`2 × maxSampleAge + scrape interval`).
4. probe 시작 → 10s baseline → `injectAt` 기록 → `systemctl stop k3s-agent`(소요 시간을 `injectCmdSeconds`로 기록).
5. preflight에서 읽은 Policy `metadata.uid`와 `spec.policyRef.uid`가 같고 직전 `incidentSequence`보다 큰 Incident가 terminal이 될 때까지 5s poll(상한 `incidentDeadline + 3m`).
6. probe 중지 → `incident.json`·`policy.json`·`pods.json`·`events.txt`·주입 후 Deployment 상태 수집 → `demo-driver report` → `summary.json`.
7. **reset**(정상·실패 모두): 먼저 Primary의 `start k3s-agent`를 시도한다. 성공 여부와 무관하게 Policy `enabled=false` 및 UID·상태 조회, 부하 Pod별 삭제·부재 확인을 독립적으로 수행한다. 이어 service `is-active`, Primary Ready(최대 300s), disabled Policy의 activeIncident 해제(최대 60s)를 확인한다. 모두 성공한 경우에만 `deploy/owner`를 SSA로 재적용하고, Policy disabled 유지 및 Primary의 단일 Ready demo-api Pod(최대 300s)를 확인한 뒤 status의 `circuits`·`recentFailures`를 지운다. 서비스·Node·Pod·Policy·부하 Pod 부재를 다시 확인한 후에만 `enabled=true`를 쓰고 조회로 확인한다. 어느 단계든 실패하면 가능한 나머지 안전 정리를 계속하고 Policy 비활성화를 다시 시도한다. **명령 성공을 복구 성공으로 추정하지 않으며, 비활성화 조회 실패 시 안전 상태를 보장하지 않는다.** SSH는 45s(강제 종료 유예 5s), kubectl은 330s(최장 Ready 대기 300s 포함, 강제 종료 유예 5s)로 제한한다. Probe는 INT·TERM을 각각 최대 5s 기다린 뒤 KILL을 보내며 강제 종료는 정리 실패로 기록한다. `reset.tsv`에 단계별 종료 코드, 원래 실행 종료 코드(`original_exit`), 정리 결과(`cleanup_exit`)를 남긴다. 정리 실패 시 비0 종료하며 기존 실패·시그널 종료 코드는 보존한다. 다음 실행 전 소유자가 증거와 실제 상태를 확인하고 수동 복구해야 한다.

종료 코드: 0 = PASS, 그 외 = FAIL 또는 오류(로그와 `summary.json`의 `notes`에 `FAIL: ...`로 이유가 남는다).

결과: `$OPSLOOP_ARTIFACT_DIR/demo-a/<scenario>/<UTC runId>/`.

reset의 SSA 재적용은 `deploy/owner`가 소유하지 않은 template annotation `opsloop.io/attempt-id`(manager `opsloop-controller`)를 지우지 않는다. **이전 회차의 attempt-id annotation이 baseline template에 남는 것은 정상**이다. 다음 회차는 새 actionID로 판정한다(`env.json` `notes`에도 기록).

측정값 해석: T_detect·T_decide·T_place는 `metav1.Time`(초 단위)을 쓰므로 ±1s 해상도다. T_detect는 Host `injectAt`과 controller VM `detectedAt`의 교차 clock이다. T_decide는 controller VM 단일 clock이다. T_place는 대상이 `opsloop-control`이면 VM 단일, 아니면 controller VM과 대상 kubelet의 교차 clock이다. report는 `detectedAt`이 `injectAt - 1s`보다 이르면 이전 Incident로 보고 FAIL 처리한다. T_recover·hostMaxGap은 Host 단일 clock이다. 180s 목표는 표시만 하고 PASS 판정에 쓰지 않는다. RECOVERED여도 attempts가 2개 이상이면 `retry`로 기록하고 PASS로 세지 않는다.

## 이미지 cache: warm / cold

- 첫 회 전에 같은 digest를 두 후보 Node에 미리 pull하고 `OPSLOOP_IMAGE_CACHE=warm`으로 선언한다. 후보 Node마다 아래 임시 Pod를 띄워 Running 확인 후 삭제한다(자격증명을 CLI에 넘기지 않는다). label에 `app.kubernetes.io/name: demo-api`를 붙이지 않는다(Service·preflight 대상이 되지 않게).

  ```yaml
  apiVersion: v1
  kind: Pod
  metadata: {name: opsloop-warm-<node>, namespace: opsloop-demo}
  spec:
    nodeSelector: {kubernetes.io/hostname: <node>}
    imagePullSecrets: [{name: acr-pull}]
    automountServiceAccountToken: false
    securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
    containers:
      - name: warm
        image: <ACR login server>/opsloop/demo-api@sha256:<index digest>
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
  ```

- cold는 별도 표기 회차로만 측정한다: 소유자가 대상 Node에서 `sudo k3s crictl rmi <image@digest>` 후 `OPSLOOP_IMAGE_CACHE=cold`.

## 소유자 절차 (spec 11장 O8–O13)

**O8 후보 label**(승인 필요, 1회성): `kubectl label node opsloop-control sarmubuntuserver opsloop-azure-worker opsloop.io/recovery-candidate=true`(2노드면 `opsloop-azure-worker` 제외). Policy `candidateSelector`가 이 label을 쓴다. action마다 label을 바꾸지 않는다. preflight가 expect-node·expect-reject Node의 label을 확인한다.

**O9 배포**(승인 필요): `deploy/owner/kustomization.yaml.example`을 `kustomization.yaml`로 복사해 CI summary 값으로 채운다 → `kubectl apply --server-side --field-manager=opsloop-deploy -k deploy/owner`(enforce 주석 상태) → `kubectl wait --for=condition=Established crd/recoverypolicies.opsloop.io crd/recoveryincidents.opsloop.io` → Policy 1회 적용 `kubectl apply --server-side --field-manager=opsloop-policy -f deploy/demo-policy/recoverypolicy.yaml`. 확인: demo-api가 Primary에서 Ready, `curl http://100.73.4.76:30080/`, ServiceMonitor target UP. 부하 manifest 2개를 `--dry-run=server`로 확인한다(reserve는 치환본):

```bash
kubectl apply --dry-run=server -f demos/node-failure/load-azure-cpu.yaml
sed 's/${OPSLOOP_RESERVE_CPU}/3300m/' demos/node-failure/reserve-control-requests.yaml | kubectl apply --dry-run=server -f -
```

`opsloop-demo` namespace는 Pod Security `restricted`로 강제된다. 부하 Pod와 warm Pod는 이를 만족하도록 securityContext를 둔다. busybox image는 `busybox:1.37.0`의 OCI index digest(`sha256:bdf57e52…`)로 고정한다. 2026-10-13 레지스트리 조회로 1.37.0 태그와 일치함을 확인했다.

**O10 P3 라이브**(승인 필요, 장애 주입): `OPSLOOP_MODE=ObserveOnly OPSLOOP_SCENARIO=primary-only demos/node-failure/run.sh` 1회. 3번째(ARM) Node가 이미 합류했으면 `primary-only` preflight가 실패하므로 같은 기대 Node를 쓰는 `OPSLOOP_SCENARIO=azure-loaded`로 실행한다. PASS = OBSERVED, `opsloop-control` 선택, Deployment `generation`과 `.spec` sha256 불변. 결과를 `docs/experiments/`에 기록한다.

**O11 Enforce 전환**(승인 필요): Policy `enabled=false` → activeIncident 해제 대기 → overlay에서 `../../config/rbac/enforce` 주석 해제 후 `deploy/owner` 재적용 → `enabled=true`. 그 다음 `kubectl auth can-i update deployments/demo-api --as=system:serviceaccount:opsloop-system:opsloop-controller -n opsloop-demo`가 `yes`인지 확인하고 `kubectl -n opsloop-demo patch recoverypolicy demo-api --type=merge -p '{"spec":{"mode":"Enforce"}}'`. Policy 파일은 고치지 않는다(ObserveOnly 복귀도 같은 patch).

**O12 Demo A**(승인 필요: 장애 주입·부하·Azure 비용 창 48h 안):

1. warm 준비(위 절) 후 시작한다.
2. `azure-loaded`를 실행한다. 회차마다 run.sh가 reset으로 끝난 것을 확인한 뒤 다음 회차를 시작한다.
3. **연속 카운트 규칙**: 회차 판정은 그 회차 `summary.json`의 `pass`만 기준으로 한다. `pass: true`면 그 시나리오의 연속 카운트 +1. `pass: false`이거나, preflight 통과 뒤 `summary.json` 없이 중단된 회차(부하 Pod Running 실패, Incident 없음 등)는 FAIL이다. `summarize`는 `summary.json`만 읽으므로 이런 회차는 표에 나오지 않는다. 실험 기록에 FAIL 회차로 직접 적는다. FAIL이면 원자료 디렉터리를 지우지 않고 남기고, 원인을 실험 기록에 적은 뒤 그 시나리오의 연속 카운트를 0부터 다시 센다. 회차에서 빼는 것은 preflight 실패(`preflight-failed.txt`만 남고 클러스터 무변경)뿐이다. reset 실패는 그 회차의 PASS/FAIL 판정을 바꾸지 않는다. 원인을 확인하고 수동으로 복구한 뒤 다음 회차를 시작한다.
4. 연속 5회 PASS가 될 때까지 반복한다. `laptop-loaded`도 같은 방식으로 연속 5회 PASS(servedArch arm64, 같은 index digest의 ARM native 실행).
5. cold 측정은 별도 표기 회차로만 한다(연속 카운트와 분리).
6. 부정 시나리오(spec 9.4, 수행하지 않으면 "P4/arch 수용 미충족"으로 기록): `all-rejected` 1회; Prometheus 중단(Prometheus CR `spec.replicas` 0 → primary 주입 → WAITING `METRICS_UNAVAILABLE` → `replicas: 1` 복원, Helm values와 같은지 확인하고 patch·복원 시각 기록); Tailscale partition(tailnet ACL 변경 별도 승인, 미승인이면 미충족 기록); ACR 인증 만료(cold cache + `acr-pull` 임시 제거 → `IMAGE_PULL_FAILED` → 복원); ARM manifest 누락(amd64 전용 digest로 `laptop-loaded` → `IMAGE_PLATFORM_MISSING`); patch 직후 Controller 종료(attempt `Patched` 직후 controller Pod 삭제 → attempts 1개·RECOVERED); 외부 image 변경(VERIFYING 중 `kubectl set image` → SUPERSEDED); Pending(attempt annotation 변경 즉시 선택 Node `cordon` → `PLACEMENT_TIMEOUT` → 다른 후보 attempt → `uncordon`, 최대 3회, 관측 못 하면 미충족 기록).

**3번째(ARM) Node가 없을 때(축소)**: `primary-only` 5회 + `all-rejected` 1회만 실행한다(`OPSLOOP_TIME_SSH=opsloop-control`). 이것은 양방향 resource-aware 선택과 ARM native 실행을 입증하지 못하므로 실험 기록에 **"P4 DoD 미충족(3노드 미구성)"**으로 명시한다. `scripts/verify-platform.py`는 2노드 baseline 전용이며 이 판정에 쓰지 않는다.

**O13 결과 정리**(문서 commit 승인 필요): `bin/demo-driver summarize "$OPSLOOP_ARTIFACT_DIR/demo-a"` 출력(회차 표, 시나리오별 PASS 수와 PASS 회차의 T_recover median/max)을 `docs/experiments/`의 실험 기록에 붙이고, 원자료 디렉터리 위치와 환경(kubectl version·policyHash·index digest·netPath·nodeTimeSync)을 남긴다. 숫자만 바꿔 통과시키지 않는다.

## Policy 정리의 객체 식별 경계 (SAF-01)

정리의 disable, ledger 초기화, enable 및 실패 시 fallback disable은 preflight에서
기록한 Policy UID와 변경 직전에 읽은 resourceVersion을 JSON Patch `test`로
검증한다. 조건 검증과 필드 변경은 같은 API 요청에 포함된다. 같은 이름의 새
Policy, 삭제 중인 객체, 조회 실패 또는 누락된 resourceVersion은 변경하지 않는다.
ledger 초기화와 enable은 같은 snapshot에서 disabled 및 activeIncident 부재도
확인한다. 조회 뒤 생긴 status/spec 변경은 resourceVersion 조건으로 거절된다.

충돌한 작업을 강제로 덮어쓰거나 enable/ledger를 재시도하지 않는다. 정리 실패는
기존처럼 nonzero exit와 `reset.tsv`로 남긴다. 독립 안전 조치인 fallback disable은
새로 읽은 **동일 UID** snapshot에 한 번 더 조건부 요청할 수 있다. enable의 응답이
유실됐고 이후 조회도 실패하면 비활성화를 확인할 수 없으므로 성공으로 보고하지
않는다. UID가 달라졌다고 확인되면 이후 Policy mutation은 계속 금지한다.

이 보호는 Policy 객체에 한정된다. 기존 load Pod의 이름 기반 삭제, owner overlay
전체 재적용, reset 전체 시간 예산(SAF-02), 문서 sudo 권한 계약(SAF-03), 물리 Primary
중단 및 NodePort 전제는 별도 검토·수정 대상이다. 따라서 이 수정만으로 현재
ClusterIP 환경의 IMAGE_PULL 또는 Enforce 실험에 이 스크립트를 사용할 수 있다고
판정하지 않는다. 실제 실험·배포·권한 변경은 별도 승인 대상이다.

## Runtime v1 offline acceptance (`demo-a-summary/v2`)

`report` without `-acceptance` retains the legacy v1 verdict and timing meanings.
Runtime acceptance is explicitly selected with the following CLI; all paths below
are local evidence files, and this command makes no HTTP or Kubernetes requests:

```bash
demo-driver report -acceptance runtime-v1 -evidence evidence.json \
  -probe probe.jsonl -incident incident.json -pods pods.json -env env.json \
  -inject-at 2026-10-13T00:00:00Z -mode Enforce -scenario offline-example \
  -expect-node fixture-node -expect-version fixture-version \
  -platform-digests sha256:platform-digest -out summary.json
demo-driver summarize ./results
```

Use actual identities from the supplied run, rather than the placeholders above.
The typed contract is `RuntimeEvidence` in `cmd/demo-driver/acceptance.go`; a
reproducible synthetic positive control is `acceptanceFixture` in its tests.
There is no evidence collector in this change. Existing `run.sh` does not produce
this envelope and **is not approved for execution** by this offline workflow.
Its historical reset ordering and raw disabled checks do not establish v2 stop
acknowledgement or cleanup. Do not use its v1 PASS as Runtime acceptance.

The envelope version is `demo-a-evidence/v1`, independently of the result version
`demo-a-summary/v2`. Required evidence includes:

- `evidenceKind` (`synthetic` or `observed`), `runID`, and Policy UID/generation/hash,
  Incident UID/sequence, action ID, Deployment UID, selected Node UID, attempt Pod
  UID in `identity`. Incident snapshot must be enabled Enforce with action budget
  1; its hash is recomputed. The successful attempt, decision, candidate, pinned
  image and imageID are cross checked with the existing incident/pods/env inputs.
  The unique raw Pod selected by attempt UID must explicitly supply name,
  namespace, spec.nodeName and exactly one controller=true ReplicaSet owner
  (apps/v1). Name is compared with attempt.PodName; namespace with Incident and
  approved Service; Node with attempt.PodNodeName/NodeName and selected Node;
  owner UID with attempt.ReplicaSetUID. Missing identity is UNPROVEN, supplied
  contradictions and ambiguous controllers are FAIL. No values are filled from
  declarations. This checks Pod-to-ReplicaSet identity only, not a longer owner
  chain or live API state.
- `service` namespace/name/UID/pathID/URL, repeated as `runtimeService` in env.json.
  Each probe line adds `pathID` and `url`; all samples must use this approved route.
  HTTP redirects are returned as failures without following the destination.
- `sources`: four `{role, ref, sha256}` entries for `probe`, `incident`, `pods`,
  `env`. SHA-256 binds the exact local input bytes. Other refs identify supplied
  observations; the checker does not fetch them, certify their truth, or build
  an evidence repository.
- `faultRequested`, `decision`, `patched`, `apply.observed`, `readySource`,
  `readyObserved`, plus a `probe` timing entry for each probe line in file order.
  Apply proof includes Deployment UID, opaque pre/post RV, pre/target template
  hash, attempt annotation, hostname, source (`update-response` or `fresh-get`)
  and ref. Snapshot consistency is an apply observation, not a new CAS proof;
  conditional write behavior relies on the existing recovery/controller tests.
- `coverage`: explicit complete windows and refs for `watchdog`, `admin`,
  `cleanup`, `desired-state`; `interventions` must be an explicit array, even
  when empty. An intervention records actor, restore/desired-state kind, target
  UID, request stamp, optional confirmation stamp and ref. Request time censors
  attribution; a later confirmation does not move that boundary. Coverage must
  span fault request through stable recovery. Cleanup restoration must follow
  stop acknowledgement and stable recovery.
- `stop`: same Policy UID, newer stopped generation, explicit enabled/mode,
  deleting and active-incident absence, Quiesced=True/FenceClosed at that
  generation, matching acknowledged/live v2 fence with Policy UID/name,
  Deployment UID and closedThroughGeneration >= stopped generation, request/ack
  stamps and ref. `stopped:true` alone is insufficient. `cleanup` additionally
  requires succeeded status, workload/old-Pod/Service checks, completion and ref.
  Explicit activeIncidentAbsent=false or Quiesced=False linked to the current
  Policy UID and stopped/observed generation is cleanup FAIL. Missing/Unknown
  values, missing acknowledgement or unknown order are UNPROVEN; stale state
  is not attached as a current counterexample. Explicit generation/fence
  contradictions remain FAIL, which additional UNPROVEN reasons cannot erase.
  Neither observation proves actual process termination.

A stamp is `{recorder, segment, sequence, elapsedMs, at}`. Elapsed milliseconds
are explicit nonnegative values scoped to a recorder/segment; sequence is
positive and unique. `at` is UTC. No sample is silently sorted or deduplicated.
Wall/elapsed discontinuities require a new segment. Request start is linked to
probe `t`, completion to `t + latencyMs`. A request starting before
apply.observed (including one crossing that observation) is excluded and resets
post-apply stability; a later independent 30-second window can still pass.
A current-Pod response completed before a comparable actual decision, patch or
Ready source remains a causal FAIL. Missing clock evidence for required
apply/decision/patch ordering remains UNPROVEN. Cross-clock comparisons require `clocks`
entries `{recorder, segment, offsetMs, uncertaintyMs, ref}` for both clocks, where
corrected UTC = recorded UTC + offset. Overlapping uncertainty intervals leave
ordering unproven. Optional Ready source placement duration can be null with a
reason while independent apply/Service causality passes. Ready source and the
later Ready observation are separate events. Refs and declarations do not
provide cryptographic authentication.

Acceptance requires a normal baseline lasting at least 30 seconds (>=5 healthy
responses, request-start gaps <=3 seconds), then two consecutive completed
Service failures after the fault request. Recorder errors and missing records
are not outage observations. After apply confirmation, the current attempt UID
must serve HTTP 200 with the expected version/platform/Node for >=30 seconds,
>=5 responses and request-start gaps <=3 seconds. Errors, redirects, different
UIDs and long gaps reset the window. Fixtures advance recorded time; tests do
not sleep for these windows.

Results preserve `controllerOutcome`/`controllerReason` and independently emit
`serviceRecovery`, `attribution`, `cleanup` as PASS/FAIL/UNPROVEN with reasons,
plus `overallPass`. Missing evidence and unknown ordering cannot produce PASS.
A cleanup failure preserves recovery details but prevents overall PASS.
FAIL/UNPROVEN writes structured output and exits nonzero. Malformed JSON,
duplicate fields, unsupported versions and unknown envelope fields produce a
separate input error. Token validation precedes typed decode: after JSON escape
unquoting, exact duplicate keys are rejected in every object. For destination
struct fields, duplicates also include encoding/json aliases (ASCII case and
Unicode simple fold, such as status/ſtatus), regardless of order. The validator
uses the destination field names, embedded fields and tagged-field dominance;
final decode uses that same encoding/json contract. A single alias remains
accepted as encoding/json would decode it. Free maps (labels, annotations,
nodeTimeSync) and unknown additional objects retain case-sensitive keys:
App/app are distinct, but App/\u0041pp are duplicates. Kubernetes JSON may
retain additional fields. The shared legacy probe and summary readers use
these input rules without changing legacy recovery verdicts. Limits are
16 MiB per file, 64 KiB per probe line, 10,000 probe records and JSON depth 64.
An envelope also limits clocks to 64, coverage/source entries to 16 each,
interventions to 1,000 and per-probe latency to 60,000 ms.

Synthetic PASS verifies the checker only. This offline program always emits
`liveProof:false`, including for observed input: `observed` declares provenance
and does not authenticate it. Summarize keeps legacy v1 and Runtime v2 separate,
and splits v2 synthetic/observed counts. Its observed input PASS count is not an
independently proven live recovery count; the latter remains zero.

INDEPENDENT_REVIEW_PENDING · LIVE_RUNTIME_NOT_PROVEN · REMOTE_CI_NOT_RUN ·
DEPLOYMENT_NOT_APPROVED · ENFORCE_BLOCKED

### P2-C read-only observer (local integration verified; live collection not run)

`demo-driver observe -config observer.json` collects the explicitly named targets,
creates runtime-v1 inputs, and calls the existing `report` implementation. The
configuration shape is in `observer.example.json`; its `.invalid` hosts and image
placeholders are deliberately unusable. This command does **not** inject faults,
change policies, stop controllers, restore nodes, or execute cleanup. `Enforce`
in the report arguments describes the incident being judged, not execution
permission. Do not use the existing `run.sh` to test this adapter.

The API origin must be HTTPS (no URL path), with normal hostname/CA validation.
An empty CA path uses system trust; an optional bearer is read only from the
explicit file. There is no kubeconfig support, HOME/KUBECONFIG/in-cluster lookup,
exec/auth-provider, client key lookup, proxy, TLS bypass, discovery, or shell
execution. The Service has a separate client, CA setting and exact approved URL;
it receives no API Authorization. Both clients reject redirects before following
them. The approved Service path must actually route through the intended Service;
the adapter does not substitute PodIP or port-forward access or certify ingress
routing merely from a user-supplied URL.

The transport permits GET only, exact approved origin/path/query, no caller
headers/body/Host override, URLs up to 4096 bytes, response headers up to 16 KiB,
API bodies up to 1 MiB and Service bodies up to 64 KiB. Per-request timeout is
1–60000ms; the real context deadline is at most one hour. At most 1000 serial
polls are allowed. Serial polling and Service measurement can take longer than
the configured minimum interval; actual gaps remain in the evidence. Stored
files have a 16 MiB individual / 64 MiB total recorder budget. No Secret, logs,
exec, proxy, tokenRequest, unscoped LIST or mutation API is exposed.

| Read/input | Retained projection and runtime-v1 mapping |
| --- | --- |
| Named Policy GET | name/namespace/UID/RV/generation, policy spec/hash and status; fixes initial policy identity; later Quiesced/fence state supplies stop ack observations |
| Named Deployment GET | metadata identity, computed `TemplateHash`/`SpecHash`, attempt annotation, hostname selector, parsed execution fence; bounded exact precondition read history and a subsequent target fresh GET supply `apply` |
| Named Incident GET | identity, policy snapshot, baseline, decisions and attempt status; supplies UID/generation/hash/action links and original Controller/Ready timestamps |
| Attempt-named Pod GET | original name/namespace/UID/node/controller owner, container images/imageIDs, Ready condition time; supplies `pods.json`, `readyObserved`, Ready source link |
| Attempt-named ReplicaSet GET | metadata/controller owner; verifies Pod → ReplicaSet → pinned Deployment |
| Configured expected Node GET | UID, hostname label, Ready boolean; verifies attempt's selected Node (no Node LIST) |
| EndpointSlice LIST | namespace + exact `kubernetes.io/service-name` selector and `limit=100`; retains slice metadata/Service owner, endpoint targetRef and conditions, not addresses; rejects truncation/unrelated results |
| Named Service GET | metadata identity; fixes Service UID and links the configured `pathID`/URL |
| Exact Service GET | start/completion stamps, HTTP status or classified failure, Pod/node/version/architecture identity; supplies `probe.jsonl` and `evidence.probe` |
| Explicit external receipt file | fault request, actor coverage/intervention, source clock witnesses, stop request, cleanup receipt; no API objects, apply proof, probes or acceptance verdict can be supplied here |

Policy, Deployment, Service and the explicitly named Incident are pinned on their
first successful read. Attempt Pod/ReplicaSet/Node and selected EndpointSlices
are pinned when first encountered; these are separate acquisition events, not a
claim that all identities were known at startup. Every later read checks the UID.
A UID replacement ends the run incomplete, without a summary or COMPLETE marker.
Every GET has its own observation stamp. These reads are never an atomic snapshot,
and `apply.observed` is a fresh GET observation, not a server commit timestamp.
The initial Policy generation/hash must match the incident snapshot. The initial
Deployment baseline remains separate from the bounded history (128 fresh GETs).
The observer selects an actually read, exact opaque `PreResourceVersion`, with
matching UID, pre-template/spec hash, baseline attempt and hostname, observed
before patch and before the target fresh GET. Metadata/status/fence-only RV
updates are allowed. Unread or evicted RVs are not inferred. This polling history
does not prove unobserved executions between reads; execution receipts are outside
this adapter's contract.

`raw-NNNN.json` is a **retained projection**, not a complete wire-body dump. Pod
env, volumes, PodIP, logs, arbitrary annotations, managedFields and free-form API
error/diagnostic messages are excluded. Deployment template hashing uses the
complete typed template transiently in memory, then discards its sensitive fields;
the reduced projection cannot independently reconstruct the full template hash.
Manifest SHA-256 values attest the actual saved projection bytes, not the omitted
wire body. Policy spec and incident decision fields are retained for the existing
hash and acceptance contracts. These files can still contain workload identities
and therefore are private: run directory 0700, files 0600. API response bodies,
Authorization and credential paths are not included in error messages. Reflection
of the configured bearer into an output is blocked before writing that file.

Each process creates a random recorder/segment and sequence. UTC and elapsed
milliseconds are recorded during collection. The recorder waits for the next
observable millisecond when needed for the existing millisecond report schema;
it does not invent elapsed offsets or restamp old events at conversion time.
Controller and kubelet stamps remain unbounded unless an explicit receipt witness
matches their exact source timestamps and supplies clock evidence. No default
zero uncertainty is assumed. Recorder/serialization failures, invalid responses,
redirects and API gaps are not converted into Service outage samples. A completed
Service timeout/transport failure is classified as a measurement-path failure;
it cannot diagnose Node failure or a code defect by itself.

External receipts use `opsloop-observer-receipts/v1` and are read once, at the end
of collection. A separate actor may publish that file while observation runs;
this adapter has no write/command interface to that actor. The file has `runID`,
`evidenceKind`, `provider`, `faultRequested`, `decision`, `patched`, `readySource`,
`clocks`, optional `collectorClock`, `coverage`, `interventions`, optional
`stopRequested`, and optional `cleanup`. Stamp/ClockBound/Coverage/Intervention/
Cleanup shapes reuse `acceptance.go`. `collectorClock` attests the observer host's
UTC offset/uncertainty; only its lane key is bound to the generated recorder and
segment. Original receipts are saved byte-for-byte, with receipt-backed references
linked to that file. Receipt identity is checked but provider authenticity is
**not** cryptographically established. No external receipt, source clock or
coverage is inferred from an absence of events. Cleanup and each coverage completion must be provably before the captured
collection horizon, the final Service probe completion Stamp. Receipt reading and
serialization cannot extend that boundary. Same recorder/segment comparisons use
elapsed time and sequence; other lanes require explicit offset and uncertainty.
Missing bounds, overlapping uncertainty, and definite future completions exclude
only the affected receipt. Original receipt bytes/times remain unchanged. A disabled Policy does not create a stop request; an actual
Quiesced/fence observation is linked only if a stop request receipt exists.
Pod absence never proves physical process termination.

Output uses `<outputRoot>/<runID>` and refuses any existing run directory. Raw
observations are written exclusively and synced as they arrive. Final probe,
incident, pods and env files are closed before their hashes enter evidence, then
`report` generates summary. A manifest and atomic COMPLETE publication finish the
capture. COMPLETE means files were finalized, **not** acceptance PASS. Missing
receipts or API/projection gaps are recorded separately from acceptance and cannot
silently reuse an old apply proof. The diagnostic ledger retains target, capture
Stamp, raw reference, class and affected proof. Initial unacquired-object 404 and
first endpoint preparation are pending; actual later identity/precondition/apply/
owner/endpoint reads may establish proof. Gaps during the fault-to-selected-stable
interval and endpoint loss after readiness invalidate dependent apply evidence.
Early gaps require actual subsequent evidence; a last good GET alone is insufficient.
Every fresh Deployment read is checked for desired-state changes (including changes
that revert), separately from metadata/status RV changes. Contradictions before
the selected Service stable end conservatively remove apply confirmation; raw
history remains. Changes after stabilization, including receipt-declared cleanup,
are outside that attribution interval and are still retained.

Stop history records individual Policy/Deployment GET stamps and references and
a separate aggregate ack stamp. Both GETs must follow the supplied stop request
and precede cleanup. Stale candidates may be followed by a valid ack. After a
valid ack, reactivation, active incident, Quiesced/generation/fence contradiction,
or a required observation gap prevents reuse of that episode, even if a later
snapshot returns to normal. A real later request followed by new ack and cleanup
can establish a new episode. Expressible counterexamples use real StopProof fields;
otherwise cleanup remains UNPROVEN with reasons in raw/manifest. These observations
do not certify physical termination or continuous stopping between polls. `collector=complete[-with-gaps]` plus
`acceptance=FAIL/UNPROVEN` is valid; the CLI still returns nonzero for non-PASS.
Fatal interruption/write/UID errors preserve raw/partial files without COMPLETE.
`started.json` means incomplete unless a valid final manifest and COMPLETE exist.
Restart requires a new run ID and segment; there is no distributed resume engine.

The tests in `cmd/demo-driver/observer*_test.go` start only local TLS API/HTTP
Service servers and use synthetic credentials. The PASS scene serves API objects,
Service baseline/outage/new-Pod samples and explicit actor test-double receipts;
the actual collector creates evidence and invokes report. It never copies a
finished evidence or summary fixture. Clock injection is test-only, so the
30-second windows do not impose wall-clock waits. Negative scenes cover absent
outage, old/other Pod identities, HTTP 503/redirects, API timeout/403/404, UID
replacement, owner gaps, sample gaps, segment changes, unbounded source clocks,
external intervention, coverage/stop/cleanup gaps, credential reflection,
interruption and storage failure. Transport tests additionally enforce origin,
path, method, TLS, sizes and cancellation. `P2C_CLI=/path/to/built/demo-driver`
enables a separate production-executable smoke test against the local servers;
that short real-time run correctly remains UNPROVEN. The child-process synthetic
PASS test invokes the same CLI dispatcher with an injected test clock.

Status: INDEPENDENT_REVIEW_PENDING; LIVE_COLLECTION_NOT_RUN;
LIVE_RUNTIME_NOT_PROVEN; REMOTE_CI_NOT_RUN; DEPLOYMENT_NOT_APPROVED; ENFORCE_BLOCKED.
