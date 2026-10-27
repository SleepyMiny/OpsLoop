# M3 설치·실행·운영 Runbook

이 절차는 개발자와 전시 운영자를 위한 로컬 실행 경로입니다. 기존 Hybrid 클러스터를 새로 만들거나 Controller를 재배포하지 않습니다. 실제 리소스 변경·새 RBAC·Enforce·장애 주입·이미지 게시에는 별도 승인이 필요합니다.

## 준비

- 소스 기준: 최종 `source-manifest.json`과 일치하는 M3 소스. 승인 전 로컬 미커밋 구현이 원격 main에 있다고 가정하지 않습니다.
- 도구: Go 1.27.3, Node.js 24.22.0, Python 3.14.8에서 로컬 검증합니다. `go.mod`/`go.sum`, frontend `package-lock.json`을 보존합니다. [도구·캐시 상세](toolchain.md)를 확인합니다.
- Go race 검사는 C 컴파일러를 사용합니다. envtest는 실제 운영 클러스터 대신 로컬 API server·etcd 바이너리를 사용합니다.
- Replay는 브라우저와 정적 빌드만 있으면 동작합니다. Live는 기존 Tailscale 연결과 서버 프로세스만 읽는 kubeconfig가 추가로 필요합니다.
- 브라우저 환경 변수에 kubeconfig, GitHub token, 모델 인증정보를 넣지 않습니다. `.env`·키·Secret 원문을 복사하여 소스 폴더에 넣지 않습니다.

## 의존성 준비와 로컬 빌드

설치 단계는 패키지 다운로드를 사용할 수 있습니다. 로컬 검증 단계는 이미 준비된 의존성으로 실행합니다. CI의 offline 검증은 사전 설치까지 인터넷을 전혀 사용하지 않는다는 뜻이 아닙니다.

```bash
go version
node --version
python3 --version
go mod download
npm --prefix frontend/dashboard ci
```

네트워크가 없는 장비는 검증된 Go module cache와 운영체제·아키텍처가 맞는 npm 의존성을 준비합니다. 다른 OS의 node_modules를 그대로 복사하지 않습니다. Python 핵심 회귀는 표준 라이브러리 `unittest`를 사용합니다. 운영 스크립트별 추가 도구는 해당 기존 Runbook을 따릅니다.

```bash
npm --prefix frontend/dashboard run build
mkdir -p build
CGO_ENABLED=0 go build -trimpath -o build/dashboard-bff ./cmd/dashboard-bff
CGO_ENABLED=0 go build -trimpath -o build/opsloop-controller ./cmd/opsloop-controller
```

기대 결과는 `frontend/dashboard/dist/`와 로컬 실행 파일입니다. Controller 빌드는 배포나 실행 승인이 아닙니다.

## 봉인 증거 대조와 Replay 재생산

원본 Demo A/B ZIP은 변경하지 않습니다. exporter는 고정된 파일명과 SHA-256을 요구하므로 다른 실행을 같은 이름으로 대체할 수 없습니다.

```bash
python3 tools/m3_evidence.py --archives "$HOME/Downloads"
```

출력은 `frontend/dashboard/src/data/m3/evidence.json`, 검토된 다운로드 자료 `frontend/dashboard/public/m3-evidence/`, `reports/m3/evidence-index.json` 및 Replay 검증 receipt입니다. 데이터 변경 후 frontend를 다시 빌드합니다. 개인정보를 줄인 다운로드 자료는 `transformation`으로 표시하며 `sources.sha256`은 원본, `downloadSha256`은 다운로드 bytes의 해시입니다. 원본 bytes는 봉인 ZIP과 로컬 `reports/m3/replay/originals/`에서 별도로 보존합니다.

| 원본 | SHA-256 |
| --- | --- |
| `OpsLoop_DemoA_HybridE2E_20261027_030110_KST.zip` | `c396116dd0670a82c78c2ac9574c0e7a66521739e9f9862b02abd549c0a04ce0` |
| `OpsLoop_DemoB_EngineeringE2E_20261027_064911_KST.zip` | `43dae1309ebb2b22f29ec48cc8bfef3beebfad51355e2aa92e40d7a0e93e2bee` |

A package 545개, B package 481개 파일 해시와 ZIP CRC를 대조합니다. A Observer manifest의 2,407개 항목 중 ZIP에 있는 349개는 대조하고 빠진 2,058개는 PARTIAL로 기록합니다. 기존 로컬 Observer 원본은 별도로 발견하여 동일 manifest의 2,407개 파일 해시를 모두 검증했습니다(`reports/m3/replay/local-original-observer-verification.json`). 필요하면 `--observer-original /absolute/path/to/original/observer`로 원본 경로를 지정합니다. ZIP의 부분 수록 상태와 로컬 원본 전체 대조 결과는 별도입니다. `complete-with-gaps`·`liveProof:false`는 그대로 유지합니다. 이는 누락 원본을 만들어내거나 전체 actor/clock 증명을 인정하는 절차가 아닙니다. 원본 ZIP이 없거나 해시가 다르면 성공 자료를 재구성하지 말고 `evidence-index.json`의 오류를 해결합니다.

## 로컬 Replay / Exhibition

```bash
python3 scripts/m3-serve.py --mode replay
```

브라우저에서 `http://127.0.0.1:4173/`를 엽니다. 기본은 Exhibition의 과거 기록 Overview입니다. 이 서버는 loopback에서 정적 빌드를 제공하고 Replay 모드에서는 Kubernetes나 모델 API를 호출하지 않습니다. 종료는 실행 터미널에서 `Ctrl+C`입니다.

| URL | 화면 |
| --- | --- |
| `/?source=exhibition` | 전시 기본 Overview, 과거 기록 표시 |
| `/?source=replay&view=runtime` | Demo A Runtime 기록 |
| `/?source=replay&view=engineering` | Demo B Engineering 기록 |
| `/?source=replay&view=evidence&run=demo-b` | Demo B 원본 근거·UID·해시 |
| `/?source=simulation` | 명시적 개발용 합성 시뮬레이션; M3 실증이 아님 |
| `/?source=legacy-replay` | 과거 ObserveOnly 자료; M3 A/B 기록과 별도 |

## 읽기 전용 Live

먼저 기존 환경에 대한 읽기만 수행합니다. 아래 명령은 현재 Policy·노드 상태를 확인하며 Secret을 조회하지 않습니다.

```bash
kubectl --kubeconfig "$HOME/.kube/opsloop.yaml" --request-timeout=5s get nodes
kubectl --kubeconfig "$HOME/.kube/opsloop.yaml" --request-timeout=5s get recoverypolicies.opsloop.io -A
```

```bash
python3 scripts/m3-serve.py --mode live \
  --kubeconfig "$HOME/.kube/opsloop.yaml" \
  --bff-bin build/dashboard-bff \
  --engineering-artifacts .opsloop/demo-b-20261027/verification/engineering-dashboard-artifacts.json
```

`/?source=live&scope=a`와 `/?source=live&scope=b`를 선택합니다. Launcher는 A (`opsloop-demo-a/demo-api`)와 B (`opsloop-demo-b/engineering-demo`)를 별도 loopback BFF로 관측합니다. `/api/a/api/v1/snapshot`과 `/api/b/api/v1/snapshot`은 해당 BFF의 고정 GET endpoint로만 전달됩니다. 대시보드에서 Namespace를 입력해 임의로 관측 범위를 늘리지 않습니다.

`--engineering-artifacts`는 현재 장비에 보존된 고정 artifact manifest를 명시적으로 연결하는 선택 옵션입니다. 시작 시 bundle·모델 결과·독립 검토·PR receipt와 각 해시를 검증하고, 매 snapshot에서 Incident/Policy/Deployment UID, run ID, epoch, grant hash가 일치하는 기록만 결합합니다. 불일치하거나 grant가 더 이상 연결되지 않으면 과거 PR을 현재 실행에 붙이지 않습니다. 현재 Pod 상태는 그대로 유지합니다. 파일 검증 실패 시 launcher는 시작을 거부합니다. 이 옵션을 빼면 Live는 현재 BFF 관측만 표시하며 PR 상세는 Replay로 확인합니다. 새 모델 호출이나 GitHub 변경은 수행하지 않습니다.

CPU·메모리도 조회하려면 Live 명령에 `--prometheus`를 추가합니다. launcher는 localhost `19090`에서 기존 Kubernetes Service proxy의 고정 instant query 경로만 허용합니다. `monitoring/opsloop-mon-kube-prometheu-prometheus:9090`을 GET으로 조회하며 브라우저에 Kubernetes 자격 증명을 전달하지 않습니다.

```bash
python3 scripts/m3-serve.py --mode live --prometheus \
  --kubeconfig "$HOME/.kube/opsloop.yaml" \
  --bff-bin build/dashboard-bff \
  --engineering-artifacts .opsloop/demo-b-20261027/verification/engineering-dashboard-artifacts.json
```

이 옵션 없이 실행하거나 Prometheus 조회에 실패하면 CPU·메모리는 미확인/null입니다. 4173·8090·8091 포트와 Prometheus 옵션 사용 시 19090 포트가 비어 있어야 합니다. `--port`는 정적 진입점만 바꾸며 BFF 포트는 고정입니다. `--kubectl`로 기존 클라이언트 경로를 지정할 수 있습니다. proxy는 리소스를 변경하는 port-forward/배포 명령이 아닙니다. API Service proxy 조회 권한이 없는 계정에서 RBAC를 자동 확대하지 않습니다.

현재 Policy disabled와 과거 Guard STOP_CONFIRMED는 별개 관측입니다. Live에서 `Running/NotReady`이면 그대로 설명합니다. PR #6 링크나 과거 PASS가 현재 서비스 복구를 뜻하지 않습니다. BFF는 Registry platform manifest와 직접 Service HTTP를 수집하지 않으므로 관련 미확인 상태를 인정합니다.

## 검증

[toolchain.md](toolchain.md)의 실제 경로를 사용하여 Go module cache와 envtest를 지정합니다.

```bash
python3 scripts/m3-verify.py \
  --go /absolute/path/to/go1.27.3/bin/go \
  --node /absolute/path/to/node-v24.22.0/bin/node \
  --gomodcache /absolute/path/to/prepared/gomodcache \
  --envtest-assets /absolute/path/to/envtest
```

검증 runner는 소스 scan, Go vet/test/race, Python 회귀, frontend typecheck/test/build, Linux AMD64/ARM64 교차 빌드를 실행합니다. 의존성 다운로드, AI 호출, Git 게시, 운영 Kubernetes 접근은 실행하지 않습니다. 일부 테스트는 로컬 socket을 사용합니다. OS sandbox가 이를 차단하면 실패/미실행으로 기록하고 환경을 해결하며 PASS로 바꾸지 않습니다.

결과는 새 `reports/m3/checks/<UTC 시각>/verification.json`과 개별 log입니다. envtest 경로를 생략하면 해당 범위는 `NOT_RUN`/skip입니다. 원격 Actions는 별도 receipt 없이는 `NOT_RUN`입니다.

## 이미지와 Kubernetes 배포 자료

기존 `Dockerfile`은 Go builder와 distroless base를 digest로 고정하고 `APP`, `TARGETOS`, `TARGETARCH`, `VERSION`을 받습니다. M3 `.dockerignore`는 `.opsloop/`, `reports/`, 인증·환경 파일과 캐시를 제외합니다. 빌드 전 최종 source manifest와 context 목록을 검토하고, 개인 파일이 섞인 작업 디렉토리 전체를 그대로 업로드하지 않습니다. 다음은 **로컬 이미지 산출 예제**이며 이번 문서 작업에서 실행한 명령이 아닙니다.

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg APP=cmd/opsloop-controller --build-arg VERSION=m3-local \
  --output type=oci,dest=build/controller-m3.oci.tar .
```

Demo API는 `APP=apps/demo-api`, BFF는 `APP=cmd/dashboard-bff`입니다. `apps/engineering-demo`는 결함 실증 기준 소스이므로 이를 수정 이미지라고 부르지 않습니다. 새 OCI 결과는 새 digest를 산출해야 하며 기존 성공 이미지 digest·manifest를 덮어쓰지 않습니다. 로컬 교차 빌드 PASS와 OCI image build/native ARM64 실행 PASS는 각각 따로 기록합니다.

기존 CRD는 `config/crd/`, Controller는 `config/manager/`·`config/manager-fenced/`, 권한은 `config/rbac/`, 애플리케이션 예제는 `deploy/`에 있습니다. `deploy/dashboard-bff/rbac.yaml`은 `opsloop-demo`용 준비 예제이며 A/B 관측 권한을 자동 포함하지 않습니다. 이 문서는 `kubectl apply`를 실행하도록 지시하지 않습니다. 운영 배포 전 정확한 Namespace·UID·ServiceAccount·image digest·현재 spec 보존·영향·복원 명령을 별도 승인받습니다.

## 장애 대응과 다른 장비로 이전

| 상황 | 대응 |
| --- | --- |
| API 또는 Tailscale 단절 | Live의 오류/마지막 관측 시각을 설명하고 사용자가 Replay로 전환합니다. Enforce 재활성화나 장애 재주입은 하지 않습니다. |
| Prometheus 결측 | null/미확인으로 유지합니다. API 상태가 정상이라고 사용량을 0으로 채우지 않습니다. |
| BFF 또는 포트 충돌 | launcher를 종료하고 소유 프로세스·listen 포트를 확인합니다. 기존 운영 프로세스를 임의 종료하지 않습니다. |
| Replay 원본 불일치 | exporter 오류와 기대 SHA를 확인합니다. 다른 자료를 성공 원본으로 대체하지 않습니다. |
| Guard 현재 상태 미확인 | 과거 STOP_CONFIRMED와 분리하여 미확인으로 기록하고 승인 없는 재실행을 하지 않습니다. |

이전 장비에는 최종 소스 manifest, lockfile, 빌드 결과 또는 재빌드 도구, 허용된 Replay 파생 자료, runbook을 함께 옮깁니다. 원본 ZIP은 별도 보존하고 해시로 확인합니다. 사용자 kubeconfig·Tailscale 계정·ACR/GitHub/모델 인증은 저장소나 결과 ZIP에 포함하지 않고 소유자가 새 장비에 별도 주입합니다. Engineering artifact manifest에는 로컬 원본 파일 경로가 들어갑니다. 다른 장비에서 선택적 Live 연결을 사용하려면 승인된 비민감 receipt 원본을 옮긴 뒤 manifest의 파일 경로를 새 위치로 재지정하고 같은 SHA-256으로 다시 검증합니다. 기존 canonical 절대 경로를 그대로 사용할 수 있다고 가정하지 않습니다. 먼저 Replay만 실행한 뒤 Live 연결을 점검합니다. 이 절차는 새로운 VM·클러스터·Azure 리소스를 요구하지 않습니다.
