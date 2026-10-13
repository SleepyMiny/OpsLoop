# Live 데이터 계약 (초안)

대시보드가 실제 데이터를 받으려면 **읽기 전용 BFF**가 필요하다. 이 문서는 그 BFF가 돌려줄 JSON 형태를 정의한다. 아직 저장소에 BFF는 없고, 이 문서는 [architecture v0.1](../../../docs/architecture/architecture-v0.1.md)의 계약을 화면 관점에서 옮긴 초안이다.

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
| `cluster.nodes[]` | Kubernetes API(Ready, taint, unschedulable, arch, allocatable, requests) + Prometheus(`usage`) | `usage.*.observedAt`은 **원본 표본 시각**. 값이 없거나 NaN이면 `value: null` |
| `cluster.nodes[].usageHistory` | Prometheus range query | 선택. 없으면 sparkline을 그리지 않음 |
| `workload` | Deployment·이미지 index | `nodeSelectorHostname`은 Pod template의 `kubernetes.io/hostname` |
| `http.probes[]` | Demo Driver 측정 기록 | Driver가 저장소를 갖게 되면 연결. 없으면 빈 배열 |
| `policy` | `RecoveryPolicy` spec/status | 가중치·임계값은 정책 데이터 |
| `activeIncident`, `recentIncidents[]` | `RecoveryIncident` | `status.runtime`은 Controller, `status.engineering`은 Runner 소유 |
| `narration` | — | Demo 전용. Live에서는 보내도 버린다 |

`source`는 BFF가 무엇을 보내든 대시보드가 `live`로 덮어쓴다.

## Go `api/v1alpha1`과 맞출 항목

CRD Go 타입은 별도 브랜치에서 작성 중이다. 타입이 커밋되면 **Go json 태그가 기준**이고 `types.ts`를 맞춘다. 차이가 예상되는 지점과 처리 방향은 다음과 같다.

| 항목 | 대시보드 | Kubernetes 관례 | 처리 |
| --- | --- | --- | --- |
| 시각 | epoch ms `number` | `metav1.Time` (RFC 3339 문자열) | BFF에서 변환 또는 `parseSnapshot`에 변환 추가 |
| 메모리·CPU | bytes·millicores 정수 | `resource.Quantity` 문자열 | BFF에서 정수로 변환 |
| 비율 | 0..1 | 정해지지 않음 | 0..1 유지 제안 |
| reject reason | `NODE_NOT_READY` 등 문자열 | 같음 | 그대로 |
| history 길이 | events 40개, attempts는 예산 이내 | bounded | Controller 상한을 따름 |
| `decision.sourceNodeName` | 결정 시점에 workload가 있던 노드 | §5 필드 목록에 없음 | §7 기준 상태 저장의 일부로 Go 계약에 추가 제안. 이전 Pod가 삭제된 뒤에도 경로 출발점을 알 수 있음 |

## 실패 시 동작

- base URL 없음 → `unconfigured` 상태, Demo 데이터로 대체하지 않는다.
- 요청 실패 → 마지막 정상 snapshot을 유지하되 헤더 배지가 "연결 끊김"으로 바뀐다.
- 구조가 맞지 않는 응답 → 화면을 그리지 않고 오류를 표시한다(`parseSnapshot`).
