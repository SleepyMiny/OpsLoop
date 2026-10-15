# OpsLoop 전시용 대시보드

전시장에서 OpsLoop가 하는 일을 한 화면으로 보여 주는 웹 대시보드다. 관람객은 해설 문장과 관제판(mimic panel) 화면을 따라가며 다음 흐름을 본다.

> 장애 감지 → 규칙 기반 분류 → 근거 수집(Kubernetes API + Prometheus) → Hard filter → 점수 → nodeSelector 변경 → HTTP 검증 → 복구
> 반복 Application 장애만: Runtime 예산 소진 → 증거 보존 → 격리 → Engineering Runner → Feedback loop → PR(사람이 검토)

## 현재 상태와 정직성 원칙

OpsLoop의 Controller·CRD·Demo API·Runner는 아직 구현되지 않았다([README](../../README.md), [Implementation Plan](../../docs/implementation-plan.md)). 그래서 이 대시보드의 기본 동작은 **Demo Mode**다.

- 화면 상단에 항상 `Demo Mode · 시뮬레이션 데이터` 배지가 보인다.
- 모든 시간·사용률·점수는 시뮬레이션 값이다. 화면은 이를 실측이라고 말하지 않는다.
- 단, 복구 대상 선택은 대본이 아니다. [`src/domain/evaluate.ts`](src/domain/evaluate.ts)가 architecture §8의 hard filter와 점수 공식을 시뮬레이션 지표에 적용해 계산한다. 부하 조건을 바꾸면 결과도 정책대로 바뀐다.
- 헤더의 "무엇이 실제인가요?"(단축키 `I`)는 구축됨 / 준비됨 / 시뮬레이션을 구분해 보여 준다. 내용은 [`src/app/realityStatus.ts`](src/app/realityStatus.ts) 한 곳에서 관리한다.
- Live 모드는 설정이 없거나 연결에 실패하면 그 사실을 표시한다. Demo 데이터로 몰래 대체하지 않는다.

## 실행

Node 22.12 이상(또는 24)이 필요하다(Vitest 5 기준, 개발 확인은 Node 24.18). 네트워크·자격 증명·백엔드 없이 동작한다(글꼴도 번들에 포함).

```bash
npm --prefix frontend/dashboard install
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
| `?source=live` | Live 어댑터 사용(아래 참고) |

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
│   └── live/              # 읽기 전용 BFF용 HTTP 어댑터와 Go CRD 변환(fromCrd)
├── components/    # 화면 영역: cluster, candidates, incident, engineering, policy, shared
├── app/           # 셸: 헤더, 해설, 재생 바, 탭, 오류 경계, 단축키
└── styles/        # 디자인 토큰(밝은/어두운 테마)
```

컴포넌트는 `DashboardSnapshot`만 받는다. Demo 엔진이나 `fetch`를 직접 부르지 않는다.

## 실제 데이터로 바꾸기

1. CRD·Prometheus를 읽어 [`docs/live-data-contract.md`](docs/live-data-contract.md) 형태의 snapshot을 돌려주는 읽기 전용 BFF를 만든다.
2. `.env.local`에 `VITE_OPSLOOP_SOURCE=live`, `VITE_OPSLOOP_API_BASE=<BFF 주소>`를 둔다(이 파일은 Git에 올리지 않는다).
3. BFF는 Go `api/v1alpha1` CRD 객체를 그대로 보내도 된다. 단위·시각·phase 변환은 `src/data/live/fromCrd.ts`가 맡는다(백엔드 커밋 `f47c7d1` 기준). CRD에 없는 장애 주입 시각은 `driverReports`로 함께 보낸다.

다른 형태의 출처(예: WebSocket)가 필요하면 `DashboardDataSource`를 구현하는 클래스를 하나 더 만들고 `createDataSource`에서 고르면 된다.

## 전시 안정성

- 네트워크 없이 동작: 글꼴·스크립트를 모두 번들에 포함, 외부 CDN 없음.
- 같은 시나리오는 언제 재생해도 같은 결과(시드 고정 PRNG). 되감기는 처음부터 다시 계산한다.
- 노트북이 잠들었다 깨어나도 한 번에 250 ms 이상 앞으로 건너뛰지 않는다.
- 영역별 오류 경계: 한 패널이 실패해도 나머지는 계속 보이고 다음 갱신에서 다시 그린다.
- 1920×1080·1366×768 가로 화면은 스크롤 없이 한 화면에 맞춘다. 태블릿·휴대폰에서는 세로로 쌓인다.
