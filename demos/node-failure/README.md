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
7. **reset**(정상·실패 모두): `start k3s-agent` → Primary Ready(최대 300s) → 부하 Pod 삭제 → Policy `enabled=false` → activeIncident 해제 대기(최대 60s) → `apply --server-side --field-manager=opsloop-deploy --force-conflicts -k deploy/owner` → demo-api Pod가 정확히 1개이고 Primary에서 Ready(최대 300s) → status의 `circuits`·`recentFailures`를 null → 마지막에 `enabled=true`. 어느 단계든 실패하면 그 자리에서 비0 종료하고 Policy는 disabled로 남는다(안전 상태). 다음 preflight가 실패하므로 소유자가 원인을 확인한 뒤 수동으로 마무리한다.

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
