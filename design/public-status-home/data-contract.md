# 공개 데이터 계약 v1

기계 스키마: `public-status.schema.json`. 예시: `public-status.synthetic.json` (**합성**).
원본 관측 시각은 epoch milliseconds, ratio는0..1. 내부 IP·node name·UID·raw label·annotations·
namespace·registry·Pod env/이름·Policy spec·message·logs·token은 허용 필드가 아니다.
위험 문자열을 지우는 방식이 아니라 DTO를 명시적으로 새로 구성한다.
Worker도 같은 스키마와 교차 필드 검사를 다시 수행한다. 모든 object는 additionalProperties=false.

| 필드 | 허용 의미 |
|---|---|
| schemaVersion / source | 정수1 / observed. simulation/replay 입력 거절 |
| collectedAt | 이 collection envelope의 완료 시각. source 측정 시각 대체 금지 |
| sources.{kubernetes,prometheus} | ok/partial/unavailable, observedAt 또는 null, 고정 errorCode enum |
| nodes | 정확히 control/physical/cloud 3개 공개 alias. 실제 노드명으로부터 서버 로컬에서만 매핑 |
| nodes[].site/os/arch | 고정 placement enum, linux 또는 null, amd64/arm64 또는 null |
| readiness / readyObservedAt | 실제 Ready condition 값 및 node lease 갱신 시각. 오래된 true도 갱신 시각을 유지 |
| cpu / memory | value ratio 또는 null, 원본 observedAt 또는 null. 시간/값은 쌍으로 존재 |
| exporter | up 값0/1 또는 null, 해당 시계열의 실제 sample timestamp |
| workloads | controller/demo-api 공개 ID, desired/ready replica 수, 관측 시각. 이름·UID·image 없음 |
| policy | 실제 mode/enable/Monitoring, 관측 범위 incidentCount/activeIncidentCount, 관측 시각만. spec/조건 message 없음 |

추가 의미 규칙:

1. alias/site 쌍 고정, 중복 ID 금지. 알 수 없는 새 노드는 raw 이름을 내보내지 않고
   로컬 진단 및 고정 INVALID/부분 실패로 처리한다. 최소 scope가3노드이므로 전체 cluster inventory라고 주장하지 않는다.
2. 관측 실패의 count/ready/metric는 null. 실제0만0. Incident list 권한 오류를0개로 만들지 않는다.
   Policy 상태는 observedGeneration 일치 여부를 확인하고, 불일치면 monitoring을null로 둔다.
   incidentCount는 matching Policy/Workload UID의 현재 보관 객체 수, activeIncidentCount는
   Policy의 activeIncident 참조와 실제 객체를 대조한 활성 수다. 참조 불일치는0이 아닌 null/partial.
   terminal 과거 Incident가 있다는 이유만으로 현재 장애나 health=degraded로 표시하지 않는다.
3. K8s 조회 실패 시 이전 Ready를 새 observedAt로 재포장하지 않는다. 실패 heartbeat는
   sources.kubernetes=unavailable, node readiness/time 및 workload/policy 정보=null이다.
   부분 GET 실패는 sources=partial이고 성공한 component만 자체 원본 시각으로 표시한다.
4. Prometheus 7개 고정 query를 같은 evaluation 시각으로 실행한다. query timestamp가
   아닌 CPUTime/MemAvailTime 원본 시각 사용. exporter는 Up sample.Time 사용.
   CPUCount 최소3, finite/bounds·instance 중복·IP 식별 확인 전 value를 정상으로 채우지 않는다.
   ambiguous/missing/NaN은 null 및 partial. stale 값은 원본 시각으로 보존하되 current로 판정 금지.
5. 수집 스냅샷은 Kubernetes 객체 간 원자적인 transaction이 아니다. 수집 창을 제한하고
   component별 시각을 보존한다. 이 관측 DTO는 후보 결정·복구 실행 허가에 사용하지 않는다.
6. 예상과 다른 실제 mode=Enforce가 보이면 숨기거나 ObserveOnly로 치환하지 않는다.
   공개 요약에 실제 mode를 보여주고 경고한다. Worker에는 Enforce 전환 기능이 없다.

## 시각 / freshness

- Collector tick30s, Home poll30s. collectedAt age **75초 초과**는 stale.
- 원본 node lease 및 metric **90초 초과**는 불완전한 데이터. 미래 **5초 초과**도 invalid.
- receivedAt=Worker 저장 승인 시각, serverTime=API 응답 시각. 둘은 원본 시각과 별도다.
- frontend는 serverTime 기준 offset과 monotonic 경과를 사용해 poll 사이에도 stale 배지를 갱신한다.
  오래된 JSON을 새로 수신한 사실만으로 freshness를 되살리지 않는다.
- 새 Collector는 실패해도 정상적으로 HTTPS를 보낼 수 있으면 component-error heartbeat를 보낸다.
  Collector/인터넷/Ubuntu 자체가 꺼지면 업데이트가 끊기며75초 이후 stale이다.
  마지막 정상 수신값만으로 어느 장비의 전원이 꺼졌는지 단정하지 않는다.

API가 계산하는 dataState와 health는 별개다:

| 상태 | dataState | health |
|---|---|---|
| 현재 유효한 관측, Ready/metrics/정책 정상 | current | healthy |
| 현재 유효한 관측, NotReady/replica 부족 등 | current | degraded |
| query 일부 실패·누락·원본 표본 stale | partial | unknown (확인된 문제는 개별 카드에 별도 표시) |
| Collector 갱신 지연 | stale | unknown, 마지막 관측이라고 표시 |
| 저장소/네트워크 실패·데이터 없음 | unavailable | unknown |

프로토타입은 위 계산의 최소 모델이다. source=ok는 HTTP 성공뿐 아니라 필요한 field의
완전성 검사 후 지정해야 한다. schema 단독 검사만으로 관측 정확성을 입증하지 않는다.

## HTTP 계약

`GET /api/status`: exact path, query 없음. Cookie/인증정보 없이 공개 가능한 summary만 응답.
200 envelope: `{schemaVersion:1, serverTime, receivedAt, dataState, health, snapshot}`.
snapshot은 본 schema. dataState/health는 저장된 클라이언트 문자열을 믿지 않고 Worker가 계산.
오래된 데이터는200+stale로 명시하며, frontend fetch 오류 시 기존 값은 '마지막 관측' 상태로만 보존.
raw 403/HTML200/invalid JSON도 연결 실패이며 Demo로 전환하지 않는다.

| 조건 | 응답 |
|---|---|
| 최초 수집 없음 | 503 + 고정 NO_OBSERVATION, snapshot=null |
| D1 오류/일일 한도 초과 | 503 + 고정 STORE_UNAVAILABLE, raw exception 제외 |
| API 과다 요청 | 429 + Retry-After, UI backoff. 마지막 값의 시각 유지 |
| method 불일치 | 405 + Allow: GET (수집 endpoint는 POST만) |
| 알 수 없는 `/api/*` | JSON404, index.html로 fallback 금지 |

모든 API 응답: application/json, no-store, nosniff, API용 CSP default-src none.
CORS wildcard/credentials 허용 없음. 접근 가능한 정보는 공개 정보이며 CORS는 비밀보호 수단이 아니다.
초기안에는 API 응답 캐시/ETag/304를 추가하지 않아 오래된 시각이 정상으로 보이는 경로를 줄인다.
