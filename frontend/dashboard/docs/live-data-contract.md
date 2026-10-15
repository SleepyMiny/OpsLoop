# Live 데이터 계약 (초안)

대시보드가 실제 데이터를 받으려면 **읽기 전용 BFF**가 필요하다. 이 문서는 그 BFF가 돌려줄 JSON 형태를 정의한다. 아직 저장소에 BFF는 없고, 이 문서는 [architecture v0.1](../../../docs/architecture/architecture-v0.1.md)과 Go `api/v1alpha1` CRD를 화면 관점에서 연결한 초안이다.

## 왜 BFF인가

- Controller는 controller-runtime 표준 `/metrics`, `/healthz`, `/readyz`만 노출한다(Demo A 범위 백엔드 세션 확인). 상태의 원본은 `RecoveryPolicy`·`RecoveryIncident` CRD의 status다.
- 브라우저가 kubeconfig나 Kubernetes API 자격 증명을 가지면 안 된다. 전시장 단말은 특히 그렇다.
- 실제 CPU·Memory는 Prometheus가 권위다(architecture §11). 대시보드가 Prometheus에 직접 붙으면 CORS·인증·질의 버전 관리가 화면으로 새어 나온다.

따라서 BFF는 클러스터 안에서 CRD·Node·Pod·Deployment를 **조회만** 하고 Prometheus를 질의해 아래 snapshot 하나로 합친다. 쓰기 권한(RBAC verb `create`·`update`·`patch`·`delete`)은 주지 않는다.

## Endpoint

```
GET {VITE_OPSLOOP_API_BASE}/api/v1/snapshot
Accept: application/json
```

- 대시보드는 5초마다 polling하고 4초 timeout을 둔다(`src/data/live/httpSource.ts`).
- 쿠키·자격 증명을 보내지 않는다(`credentials: 'omit'`).
- API base URL은 빌드 시 환경 변수로만 받는다. URL 파라미터로 바꿀 수 없다.

## 응답 형태

TypeScript 정의가 기준이다: [`src/domain/types.ts`](../src/domain/types.ts)의 `DashboardSnapshot`.

| 필드 | 출처(권위) | 비고 |
| --- | --- | --- |
| `observedAt` | BFF 조립 시각 | epoch ms |
| `cluster.nodes[]` | Kubernetes API(Ready, `taints`, unschedulable, arch, allocatable, requests) + Prometheus(`usage`) | `usage.*.observedAt`은 **원본 표본 시각**. 값이 없거나 NaN이면 `value: null` |
| `cluster.nodes[].usageHistory` | Prometheus range query | 선택. 없으면 sparkline을 그리지 않음 |
| `workload` | Deployment·이미지 index | `nodeSelectorHostname`은 Pod template의 `kubernetes.io/hostname`. `tolerations`, 그리고 OpsLoop가 모델링하지 않는 제약(`unsupportedConstraints`, 예: podAntiAffinity)을 함께 보낸다. 제약이 하나라도 있으면 모든 후보가 `UNSUPPORTED_CONSTRAINT`로 거부된다 |
| `http.probes[]` | Demo Driver 측정 기록 | Driver가 저장소를 갖게 되면 연결. 없으면 빈 배열 |
| `policy` | `RecoveryPolicy` spec/status | 가중치·임계값은 정책 데이터 |
| `activeIncident`, `recentIncidents[]` | `RecoveryIncident` | `status.runtime`은 Controller, `status.engineering`은 Runner 소유 |
| `status.runtime.timings.faultInjectedAt` | Demo Driver 기록(Laptop Host 시계) | CRD 필드가 아니다. BFF가 Driver 기록을 결합할 수 있을 때만 채운다. 없으면 T_detect·T_recover는 "API 장애 신호 기준"으로 표시된다(§18 정의와 구분) |
| `narration` | — | Demo 전용. Live에서는 보내도 버린다 |

`source`는 BFF가 무엇을 보내든 대시보드가 `live`로 덮어쓴다.

## Go `api/v1alpha1` CRD를 그대로 받기

Go CRD 타입은 백엔드 브랜치 `claude/orchestrate-project-status-e3aa92`의 커밋 `f47c7d1`(`api/v1alpha1`)에 있다. BFF는 `policy`, `activeIncident`, `recentIncidents[]`에 **CRD 객체를 변환 없이** 넣어도 된다(`kind: RecoveryPolicy` / `kind: RecoveryIncident`로 구분). 대시보드가 [`src/data/live/fromCrd.ts`](../src/data/live/fromCrd.ts)에서 화면 모델로 바꾼다. 화면 모델 형태(`kind` 없음)로 보내도 그대로 받는다.

| CRD 형식 | 화면 모델 | 변환 |
| --- | --- | --- |
| `metav1.Time` (RFC 3339) | epoch ms | `Date.parse`, 실패 시 필드 경로를 담은 오류 |
| `metav1.Duration` (`"20s"`, `"30m"`) | 초 | Go duration 파서 |
| `resource.Quantity` (`"512Mi"`) | bytes | DecimalSI(n/u/m/k/M/G/T/P/E), BinarySI(Ki..Ei), 지수 표기(1e6). 양수 소수 bytes는 Go Value()처럼 올림. 음수·숫자 없는 표기·안전 정수 범위 초과는 필드 경로 오류 |
| 정책 percent(`maxCPUUtilizationPercent: 85`), 가중치 합 100 | 0..1 | ÷ 100 |
| `cpuUtilizationPermille`, `*HeadroomPermille` | 0..1 | ÷ 1000 |
| `scoreMilli`, `penaltyMilli` | 점 | ÷ 1000. 가중 기여도는 `spec.policy` 가중치로 다시 계산 |
| `memAvailableBytes` / `memTotalBytes` | Memory 사용률 | 1 − available/total |
| `phase: ""` | `DETECTED` | `TARGET_SELECTED`는 Controller가 저장하지 않으므로 화면에도 지속 단계로 나오지 않는다. `OBSERVED`(ObserveOnly 종료)는 그대로 |
| `actionType: Relocation \| None` | `relocation \| null` | v1alpha1에는 restart가 없다(Demo A 범위) |
| `attempts[].outcome` | `result` | Reserved·Patched → pending(PodReady 이후 placed), Succeeded → verified, Failed·Superseded·Cancelled → failed |
| `status.recentFailures[]` + `budget.cooldown` | `cooldowns[]`, 감점 입력 | 실패 시각 + cooldown. 정책은 workload 하나를 대상으로 하므로 workload UID를 붙인다 |
| `spec.detectedAt`, 첫 attempt의 `decision.decidedAt`·`podReadyAt`, `status.runtime.completedAt` | `timings` | CRD에는 timings 객체가 없어 흩어진 시각을 모은다 |
| `spec.baseline.hostname` | `decision.sourceNodeName` | 경로 출발점 |
| (없음) | `events[]` | CRD 시각만으로 만든 사건 목록(로그 본문 없음) |

CRD에 없어서 BFF가 함께 보내야 하는 값:

- `driverReports: [{ incident, faultInjectedAt }]`: Demo Driver의 장애 주입 시각(§18). 없으면 T_detect·T_recover를 "API 장애 신호 기준"으로 표시한다.
- `policyHash`: 현재 정책 spec의 hash. 없으면 활성 Incident의 `spec.policyHash`를 쓴다.
- `cluster.nodes`, `workload`, `http.probes`: Kubernetes API·Prometheus·Demo Driver에서 조합한다(위 표).

v1alpha1에 아직 없는 정책 항목(격리, Engineering)은 화면에서 "비활성"으로 표시한다.

## 실패 시 동작

- base URL 없음 → `unconfigured` 상태, Demo 데이터로 대체하지 않는다.
- 요청 실패·4초 timeout → 마지막 정상 snapshot을 유지하되 헤더 배지가 "연결 끊김"으로 바뀐다.
- 응답은 오지만 `observedAt`이 polling 3회(15초)보다 오래됨 → `stale`, 헤더 배지 "데이터 지연".
- 시각·수량이 숫자가 아니거나(변환되지 않은 RFC 3339·`resource.Quantity`), arch·readiness·pressure가 알 수 없는 값 → 해당 필드 경로를 담은 오류로 거부한다(`parseSnapshot`). NaN으로 계산하거나 모르는 라벨을 그리지 않는다.
- 평가 함수도 같은 원칙을 따른다: 숫자가 아닌 표본은 `METRICS_MISSING`, 5초 넘게 미래인 표본은 `METRICS_STALE`, 해석할 수 없는 allocatable은 `INSUFFICIENT_REQUESTED_*`로 거부한다.

## Live 후보 미리보기의 정수 계약

Controller가 저장한 `runtime.decision`은 현재 메트릭으로 다시 평가하지 않는다.
후보의 적격 여부, rejectReasons, selectedNodeUID, scoreMilli를 그대로 표시한다.
현재 후보 미리보기는 별도의 계산이며 Controller의 실행 권한을 대신하지 않는다.

Live BFF는 `cluster.nodes[].usage`에 기존 cpu/memory/memAvailableBytes 외에
`memTotalBytes`(원본 총 메모리 bytes), `cpuSampleCount`(CPU 쿼리 표본 수),
`exporterUp`(원본 up 값)을 제공해야 한다. 숫자 또는 null이며, 누락·null은
미리보기에서 METRICS_MISSING으로 처리한다. 기존 payload도 저장 Decision 표시는
가능하지만 이 세 값 없이 현재 후보를 적격이라고 추정하지 않는다.
BFF는 각 쿼리의 노드당 단일 결과, CPU/메모리 원본 시각을 보존해야 한다.
메모리 observedAt은 available/total 중 오래된 시각이어야 한다. 중복 시리즈나
쿼리 오류를 임의의 첫 값으로 축약하지 말고 null로 전달한다.

CPU는 clamp 후 정수 permille 반올림, reserve는 정수 percent 곱의 내림,
요청 headroom은 정수 permille 내림, 메모리 headroom은 정수 반올림으로 계산한다.
점수는 Go의 scoreMilli를 따라 0.001점까지 유지하고 동점은 노드 이름 순이다.
Demo의 합성 비율 fallback은 Live의 원본 메트릭 계약과 구분한다.
숫자 모델의 최대 byte 값은 Number.MAX_SAFE_INTEGER이다. 그보다 큰 유효한
Kubernetes Quantity도 오류로 표시하며 반올림 손실을 숨기지 않는다.

Go b4fb78d 기준 계약 벡터 및 재생성 방법: [contracts/README.md](../contracts/README.md).
