# OpsLoop 전시용 대시보드

전시장에서 OpsLoop가 하는 일을 한 화면으로 보여 주는 웹 대시보드다. 관람객은 해설 문장과 관제판(mimic panel) 화면을 따라가며 다음 흐름을 본다.

> 장애 감지 → 규칙 기반 분류 → 근거 수집(Kubernetes API + Prometheus) → Hard filter → 점수 → nodeSelector 변경 → HTTP 검증 → 복구
> 반복 Application 장애만: Runtime 예산 소진 → 증거 보존 → 격리 → Engineering Runner → Feedback loop → PR(사람이 검토)

## 현재 상태와 정직성 원칙

Controller·CRD·Demo API·Prometheus와 IMAGE_PULL/NODE_NOT_READY ObserveOnly 실증은 구현·라이브 검증됐습니다. 자동 재배치/Enforce 및 Engineering Runner/PR 자동화는 실환경 검증되지 않았습니다.

| 모드 | 데이터와 의미 | 네트워크 |
| --- | --- | --- |
| `?source=demo` (기본) | 결정적 시뮬레이션. 자동 복구·Engineering 장면은 가상입니다 | 불필요 |
| `?source=live` | BFF의 현재 Kubernetes + Prometheus 관측. 현재 Incident가 없으면 없음 | BFF 필요 |
| `?source=replay` | 체크섬 검증 후 허용 필드로 추출한 과거 두 ObserveOnly 실증 | 불필요 |

Live 실패 시 명시적인 오류·지연과 마지막 관측을 표시하며 Demo로 대체하지 않습니다. Replay는 보존된 시점만 선택하고 중간 단계·현재 노드 상태·HTTP probe를 만들어 넣지 않습니다. `OBSERVED`는 조치 없는 종료이며 자동 복구 성공이 아닙니다.

## 실행

Node 22.12 이상(또는 24)이 필요하다(Vitest 5 기준, 개발 확인은 Node 24.18). 네트워크·자격 증명·백엔드 없이 동작한다(글꼴도 번들에 포함).

```bash
npm --prefix frontend/dashboard ci
```

```bash
npm --prefix frontend/dashboard run dev
```

검증:

```bash
npm --prefix frontend/dashboard run typecheck
```

```bash
npm --prefix frontend/dashboard test
```

```bash
npm --prefix frontend/dashboard run build
```

`build` 결과(`dist/`)는 상대 경로(`base: './'`)로 만들어져 정적 파일 서버나 `npm run preview`로 열 수 있다. **실제 환경 배포는 이 작업 범위가 아니다.**

## 시연 방법

| URL 파라미터 | 뜻 |
| --- | --- |
| `?kiosk=1` | 무인 부스 모드: 5초 동안 입력이 없으면 재생 바·커서를 숨기고 화면 꺼짐을 막는다(Wake Lock 지원 시) |
| `?scenario=<id>` | 시작 시나리오 지정 (`node-failure-laptop`, `application-failure-pr`, `node-failure-arm64`, `no-candidate`) |
| `?loop=0` | 자동 순환 끔 |
| `?at=<초>` | 시나리오의 해당 시뮬레이션 시각에서 시작(예: `?scenario=node-failure-arm64&at=110`) |
| `?play=0` | 일시 정지 상태로 시작 |
| `?source=live` / `?source=replay` | 현재 관측 / 과거 실증 선택 |

발표자 단축키: `Space` 재생/정지, `←`/`→` 장면 이동, `1`~`4` 시나리오, `R` 처음부터, `L` 자동 순환, `F` 전체 화면, `T` 테마, `I` 실제/시뮬레이션 안내.

### 시나리오

| 시나리오 | 보여 주는 것 | 설계 근거 |
| --- | --- | --- |
| 노드 장애 → 노트북 VM으로 복구 | Azure ARM64에 부하가 있으면 CPU_PRESSURE로 탈락시키고 노트북 VM 선택 | §17 Demo A |
| 반복 앱 장애 → 검증된 PR | restart → relocation 두 번 실패, 예산 소진, 증거·격리, Feedback loop, PR에서 멈춤 | §17 Demo B, §13 |
| 노드 장애 → Azure ARM64로 복구 | 노트북 VM이 바쁘면 같은 이미지 index의 arm64 manifest로 native 실행 | §17 Demo A |
| 후보 없음 → 기다리고, 넘기지 않음 | stale metric을 0%로 보지 않음, WAITING 후 운영 실패, Engineering 전환 없음 | §7, §11 |

긴 대기 구간(Kubernetes 유예, deadline)은 빨리 감기로 재생하고 재생 바에 배속을 표시한다.

## 구조

```text
src/
├── domain/        # 계약 타입, 평가 함수(§8), 한국어 라벨 — UI·데이터 출처와 무관
├── data/
│   ├── source.ts          # UI와 데이터 출처 사이의 유일한 경계(DashboardDataSource)
│   ├── useDashboard.ts    # React 구독 훅
│   ├── createDataSource.ts
│   ├── demo/              # 결정적 시뮬레이션 엔진과 시나리오
│   ├── replay/            # 검증된 허용 필드 증거와 보존 시점 선택
│   └── live/              # 읽기 전용 BFF용 HTTP 어댑터와 Go CRD 변환(fromCrd)
├── components/    # 화면 영역: cluster, candidates, incident, engineering, policy, shared
├── app/           # 셸: 헤더, 해설, 재생 바, 탭, 오류 경계, 단축키
└── styles/        # 디자인 토큰(밝은/어두운 테마)
```

컴포넌트는 `DashboardSnapshot`만 받는다. Demo 엔진이나 `fetch`를 직접 부르지 않는다.

## Live BFF 실행 (로컬 검증)

저장소 루트에서 실행합니다. kubeconfig는 **서버 프로세스만** 읽습니다. 브라우저·Vite 환경 변수에는 자격 증명을 넣지 않습니다.

```bash
go run ./cmd/dashboard-bff --kubeconfig="$HOME/.kube/opsloop.yaml" \
  --prometheus-url=http://127.0.0.1:19090
VITE_OPSLOOP_API_BASE=/ npm --prefix frontend/dashboard run dev
```

Prometheus URL은 기존의 승인된 localhost 접근 경로를 사용합니다. 비어 있으면 지표를 null로 반환합니다. 개발 서버의 `/api`는 `127.0.0.1:8090` BFF로만 proxy합니다. `npm run preview`/정적 배포는 이 proxy를 제공하지 않으므로 별도 **same-origin `/api` reverse proxy**가 필요합니다. 기존 `HttpDataSource`는 `credentials: omit`이며 BFF는 CORS를 열지 않습니다.

BFF는 GET `/api/v1/snapshot`만 제공합니다. Kubernetes 전송도 GET만 허용하고, 배포 시에는 [`deploy/dashboard-bff/rbac.yaml`](../../deploy/dashboard-bff/rbac.yaml)의 별도 읽기 전용 Identity를 사용합니다. 해당 manifest는 준비만 했고 적용하지 않았습니다. 외부 노출용 인증·reverse proxy·BFF 이미지/Deployment는 이번 최소 범위에 없습니다. 자세한 [응답·결측·스코프 계약](docs/live-data-contract.md)을 확인하세요.

실제 로컬 BFF와 `HttpDataSource`를 연결하는 집중 테스트:

```bash
OPSLOOP_LIVE_BFF_URL=http://127.0.0.1:8090 \
  npm --prefix frontend/dashboard test -- src/data/live/liveIntegration.test.ts
```

환경 변수가 없으면 이 **실클러스터 전용 1개 테스트**는 명시적으로 SKIP됩니다. 일반 CI는 Go 직렬화 fixture와 HTTP adapter·화면 회귀를 오프라인으로 검증합니다.

## Evidence Replay

[`docs/evidence-replay.md`](docs/evidence-replay.md)에 스키마·출처 검증·비밀정보 제거·원본 보존 규칙을 정의합니다. `src/data/replay/evidence.json`은 공개 가능한 **파생 입력**이며 원본 실험 bundle이 아닙니다. 원본·kubeconfig·Secret·토큰은 Git에 넣지 않습니다.

## 전시 안정성

- 네트워크 없이 동작: 글꼴·스크립트를 모두 번들에 포함, 외부 CDN 없음.
- 같은 시나리오는 언제 재생해도 같은 결과(시드 고정 PRNG). 되감기는 처음부터 다시 계산한다.
- 노트북이 잠들었다 깨어나도 한 번에 250 ms 이상 앞으로 건너뛰지 않는다.
- 영역별 오류 경계: 한 패널이 실패해도 나머지는 계속 보이고 다음 갱신에서 다시 그린다.
- 가로 화면은 관제판 배치를 사용하고 긴 기록은 패널 내부에서 스크롤한다. 상세 출처는 접어서 표시한다. 태블릿·휴대폰에서는 세로로 쌓인다.
