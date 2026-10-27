# OpsLoop 통합 전시 Dashboard

기존 React 관제 화면과 읽기 전용 BFF에 Demo A/B의 봉인 증거를 연결합니다. 전시 운영자는 현재 클러스터 상태와 과거 실증을 구분하여 두 복구 루프를 설명할 수 있습니다. [M3 설치·운영](../../docs/m3/deployment-runbook.md), [5~8분 시연](../../docs/m3/exhibition-runbook.md), [통합 아키텍처](../../docs/m3/architecture.md)를 함께 사용합니다.

## 실증 범위

- Demo A: readiness 장애 → 실제 Controller 후보 평가 → Azure ARM64 재배치 → 새 Pod·Endpoint → 직접 HTTP 13회 연속 정상·32.42초 안정화 → Policy disabled gen8. 기능 E2E PASS이며 전원 차단/runtime-v1 전체 actor·clock 증명은 UNPROVEN입니다.
- Demo B: 실제 빈 주문 오류 → Runtime 소진 → Evidence → 실제 Luna 수정 → 수정 전 FAIL/독립 검증 15/15 PASS → [Draft PR #6](https://github.com/SleepyMiny/OpsLoop/pull/6). 수정본 merge·운영 배포·서비스 복구는 수행하지 않았습니다.
- 위는 과거 실행의 범위입니다. M3 전체 수용 여부는 최종 `acceptance-matrix.md`·`verification.json`으로 판정합니다. 로컬 테스트 PASS를 원격 CI나 현재 서비스 정상으로 해석하지 않습니다.

## 모드와 URL

| URL | 의미 | 데이터 연결 |
| --- | --- | --- |
| `/` 또는 `?source=exhibition` | 전시 진입점, 과거 기록 Overview | 봉인 A/B 파생 자료 |
| `?source=replay` | 과거 실행 재생 | API 없이 로컬 자료 |
| `?source=replay&view=runtime` | Demo A Runtime 기록 | 실제 후보·재배치·HTTP·안전 종료 근거 |
| `?source=replay&view=engineering` | Demo B Engineering 기록 | 오류·Evidence·AI receipt·검증·PR |
| `?source=replay&view=evidence&run=demo-b` | Demo B 증거 목록 | UID·run/epoch·원본/다운로드 해시·시각 |
| `?source=live&scope=a` | `opsloop-demo-a/demo-api` 현재 관측 | GET 전용 BFF A |
| `?source=live&scope=b` | `opsloop-demo-b/engineering-demo` 현재 관측 | GET 전용 BFF B |
| `?source=simulation` | 기존 개발용 합성 시뮬레이션 | M3 실증 아님 |
| `?source=legacy-replay` | 기존 ObserveOnly 기록 | A/B M3 실증과 별도 |

`?source=demo`는 전시 기록 진입점으로 취급합니다. 모드 선택이나 Replay 재생은 Enforce·장애 주입·모델 호출·Deployment 변경을 실행하지 않습니다. Live 실패는 오류/지연으로 남기며 가짜 정상 데이터나 과거 성공으로 자동 대체하지 않습니다.

Replay 화면의 `기록 재생`, `일시 정지`, `이전`, `다음`, slider로 사건을 탐색합니다. 단계당 5초는 설명용 간격이며 실제 경과 시간이 아닙니다. 원본 사건 시각은 별도로 표시됩니다. `PR_CREATED`와 `DEPLOYED/RECOVERED`는 별도 상태입니다. Live에서 `--engineering-artifacts`를 명시하면 고정 자료 검증과 현재 UID/run/epoch binding 확인을 통과한 과거 Engineering 증거만 연결합니다. 옵션이 없으면 BFF 현재 관측만 표시하며 실제 Demo B AI·검증·PR 상세는 Replay에서 확인합니다.

## 실행

Go·Node·Python의 실제 버전과 의존성 경로는 [toolchain](../../docs/m3/toolchain.md)을 따릅니다. 저장소 루트에서 실행합니다.

```bash
npm --prefix frontend/dashboard ci
npm --prefix frontend/dashboard run build
python3 scripts/m3-serve.py --mode replay
```

`http://127.0.0.1:4173/`를 엽니다. 글꼴·스크립트는 빌드에 포함되므로 Replay는 외부 CDN·클러스터 연결 없이 동작합니다. 정적 빌드 `dist/`만 제공하며 저장소 전체를 웹 루트로 노출하지 않습니다.

Live는 서버용 kubeconfig와 로컬 BFF 빌드가 필요합니다.

```bash
mkdir -p build
CGO_ENABLED=0 go build -trimpath -o build/dashboard-bff ./cmd/dashboard-bff
python3 scripts/m3-serve.py --mode live --prometheus \
  --kubeconfig "$HOME/.kube/opsloop.yaml" --bff-bin build/dashboard-bff \
  --engineering-artifacts .opsloop/demo-b-20261027/verification/engineering-dashboard-artifacts.json
```

BFF A/B는 각각 loopback 8090/8091을 사용합니다. Launcher가 `/api/a/api/v1/snapshot`, `/api/b/api/v1/snapshot`만 고정 BFF로 전달합니다. `--prometheus`는 localhost 19090에 고정된 Kubernetes Service instant query GET proxy를 엽니다. kubeconfig는 서버 프로세스만 읽고 브라우저에는 보내지 않습니다. 새로운 RBAC나 서비스 공개는 수행하지 않습니다. 선택적 Engineering manifest는 현재 장비의 경로입니다. 시작 시 원본 해시·독립 검토·PR receipt를 검증하고 매 GET에서 정확한 run binding으로만 결합합니다. 현재 Pod 상태를 덮어쓰지 않습니다. 다른 장비에서는 원본 자료 경로를 재지정하고 해시를 다시 확인합니다. 옵션을 빼거나 조회가 실패하면 CPU·메모리 지표는 null/미확인입니다.

개발용 Vite는 `npm --prefix frontend/dashboard run dev`로 실행할 수 있습니다. 기존 단일 BFF proxy와 M3 A/B launcher 경로가 다르므로 통합 Live 시연은 `m3-serve.py`를 사용합니다. `npm run preview`는 단독으로 A/B BFF proxy를 제공하지 않습니다.

## 검증과 자료 생성

```bash
npm --prefix frontend/dashboard run typecheck
npm --prefix frontend/dashboard test
npm --prefix frontend/dashboard run build
python3 -B -m unittest discover -s tests -p test_m3_evidence.py -v
```

전체 Go/Python/Frontend/race/교차 빌드는 [M3 검증 runner](../../scripts/m3-verify.py)를 사용합니다. 실제 브라우저 검증과 캡처는 최종 `dashboard/` receipt를 확인합니다. CI는 원격 receipt가 없으면 `NOT_RUN`입니다.

```bash
python3 tools/m3_evidence.py --archives "$HOME/Downloads"
```

Exporter는 고정 Demo A/B ZIP SHA-256과 manifest를 검사합니다. `src/data/m3/evidence.json`은 allowlist로 추출한 과거 요약이며 원본 실험 전체가 아닙니다. `public/m3-evidence/`에는 검토된 다운로드 파일만 둡니다. 개인정보 최소화 변환이 있으면 원본 SHA-256과 다운로드 SHA-256, 변환 설명을 구분합니다. 원본 ZIP·전체 로그·kubeconfig·Secret을 웹에 복사하지 않습니다. 자세한 전체/부분 대조 범위는 `reports/m3/evidence-index.json`을 확인합니다.

## 구현과 경계

| 경로 | 역할 |
| --- | --- |
| `src/m3/` | M3 전시 shell, 실제 증거 탐색·재생 |
| `src/data/m3/evidence.json` | 검증된 A/B 파생 입력 |
| `src/data/live/` | BFF HTTP adapter, CRD 변환, UID/run binding |
| `src/app/`, `src/components/` | 기존 Live 관제 화면·후보·Incident·Engineering·Policy |
| `src/data/demo/` | 명시적 개발용 합성 시뮬레이션 |
| `src/data/replay/` | 기존 ObserveOnly Replay |
| `src/public/`, `src/status/` | 기존 별도 공개 profile; M3 로컬 전시와 다른 산출물 |

BFF는 GET `/api/v1/snapshot`만 제공하고 Kubernetes 전송도 GET만 허용합니다. `deploy/dashboard-bff/rbac.yaml`은 기존 `opsloop-demo`용 준비 예제이며 A/B 권한을 자동 부여하지 않습니다. 운영 적용·scope 변경은 별도 승인 대상입니다.

현재 노드·Pod·Policy는 Kubernetes, 사용량은 Prometheus가 권위입니다. 최근 FAILED 이력은 현재 서비스 장애로 바꾸지 않습니다. 실제 Pod readiness와 직접 Service HTTP도 구분합니다. Live BFF는 Registry platform index·직접 HTTP probe/복구 시간을 수집하지 않으므로 결측은 미확인으로 남습니다. 현재 Guard 상태를 직접 관측하지 않았다면 과거 STOP_CONFIRMED로 현재 상태를 대체하지 않습니다.

기존 계약은 [Live contract](docs/live-data-contract.md), [ObserveOnly Replay](docs/evidence-replay.md), [공개 static profile](docs/public-static.md)에 보존합니다. `build:public`·`build:status`는 별도 공개 산출물이고 이번 M3 launcher와 동일하지 않습니다. 외부 게시에는 별도 승인이 필요합니다.
