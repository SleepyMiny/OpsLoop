# Node 관측 및 영속 safety hold

이 구현은 최신 관측으로 실행을 허용하고, 실행 후 확인된 이상이나 확인 불능에
Deployment safetyHold를 저장하는 계약이다. Node와 Deployment는 별도 객체이므로
마지막 Node GET 뒤 최초 raced workload 쓰기는 여전히 저장될 수 있다. 원래 strict
cordon/ReadyFalse/trigger-clear 3FAIL은 이 한계를 진단하며 FIXED로 바꾸지 않는다.

`deploymentExecution`은 최종 live Policy/예약/UID/hash/fence, Node 적격성, metrics freshness,
deadline 검사와 같은 Deployment RV workload Update를 함께 소유한다. Reconciler만
Policy/Incident status를 쓴다. permit을 반환해 나중에 쓰는 API는 없다.

workload 요청 전 Reconciler는 같은 Incident UID/현재 Reserved action ID에 귀속된
`status.runtime.workloadDispatchActionID`를 status CAS로 먼저 저장하고 응답 값 보존까지
확인한다. API 오류나 CRD pruning으로 표식 저장이 확인되지 않으면 workload 요청을
보내지 않는다. 표식 저장 뒤 최종 live 검사를 다시 수행하며 Deployment RV는 교체하지 않는다.
명확한 pre-dispatch 거절/409는 status CAS로 표식을 지우고, 정상 workload 응답과 post guard
이후에는 Patched status와 함께 지운다. timeout 뒤 safetyHaltReason/hold 저장이 모두 실패해도
사전 표식이 남으므로 다음 재시도/재시작은 hold→live settlement를 먼저 수행한다.
오래된 worker의 clear는 Incident RV CAS로 다른 action 표식을 덮지 못한다.

정상 Update 뒤, Reserved AlreadyApplied 재개 뒤, VERIFYING의 HTTP/Endpoint 관측 뒤
구조 guard가 trigger UID/지속성 및 candidate UID/hostname/selector/platform/Ready/cordon/
금지 taint/pressure를 확인한다. 첫 read error/timeout은 Unknown이며 hold 사유다.
사후에는 자원/metrics/full eligibility를 다시 적용하지 않아 자기 Pod 요청량을 이중계산하지 않는다.
별도 자원 예약이나 실행 뒤 지속적 자원 정상을 보장하지 않는다.

이상/Unknown, Reserved 취소, 예약 이후 deadline, workload 저장 응답 불확실에서는 다음 순서를 따른다.

1. bounded `status.runtime.safetyHaltReason`으로 미완료 정리를 보존한다. status 충돌이어도 hold CAS를 시도한다.
2. 같은 owner/Deployment UID의 v2 fence에 holdID/incidentUID/actionID/policyGeneration/reason을
   CAS하고 floor를 단조 증가시킨다. hold 응답 유실/409/조회 실패는 pending으로 남긴다.
3. live hold 확인 뒤 같은 Deployment의 template UID/hash/action을 읽어 적용 결과를 정리한다.
   저장됐으면 Patched를 유지하고 미적용이면 Cancelled, 외부 변경이면 Superseded다.
4. 확인된 뒤 MANUAL_REQUIRED/SAFETY_HALTED와 완료 시각을 기록하고 active 예약을 해제한다.
   Monitoring은 False/SafetyHalted다. API 불능이나 UID/owner 불일치에는 terminal/release/다음 action이 없다.

hold가 먼저 저장되면 옛 RV workload 요청은 conflict이며 새 RV 참여 writer도 hold로 거절한다.
workload가 먼저 저장되면 hold CAS가 conflict하고 다음 reconcile에서 Patched로 정리한다.
모든 세대, Node 회복, 새 예약, 재시작에서도 hold가 유지된다. 자동 해제는 없다.
Quiesced는 disabled/ObserveOnly stop 완료에만 쓰며 enabled hold의 정상 회복 증거가 아니다.

이는 rollback/Pod 삭제/실제 process fencing이 아니다. 이미 시작된 rollout의 계속 진행도
막지 않는다. GET 사이 순간 Node 변화, 마지막 guard 이후 변화, 관측 뒤 hold 저장 전 crash,
API 불능 동안의 유한 시간 중단은 보장하지 않는다. resourceVersion은 opaque이며 서로 다른
객체의 RV 크기로 시간순서를 추론하지 않는다. 기존 v1 reader는 v2를 거절하지만 Fence를
모르는 old writer나 외부 writer의 우회는 RBAC/운영 전환 없이는 차단되지 않는다.

검증은 로컬 envtest/API CAS와 결정적 interceptor 경합이다. 실제 scheduler, kubelet,
네트워크 분할, 물리 Node/Pod/process, 실제 서비스 복구는 검증하지 않았다.
Enforce는 새 계약 수용, 리뷰/회귀, CRD와 v2 writer 전환, 검증된 수동 hold 복구 계획 및
별도 라이브 실험 승인 전 BLOCKED다.


## 응답 영속 확인과 호환 비용

workload 성공 응답 뒤 Patched status 저장이 실패해도 사전 dispatch 표식은 남는다.
따라서 이 경우 재시작은 정상 VERIFYING 재개가 아니라 hold/manual/Patched로 보수적으로
정리한다. 외부 template 변경이 있으면 적용 결과는 Superseded로 남긴다. API 응답 수신만으로
영속 ACK를 증명했다고 간주하지 않는다. 사용 가능한 다음 세대가 있다고 자동 우회하지 않는다.

일반 Policy 변경/disable/ObserveOnly 등으로 기존 예약의 허가가 취소되면 immutable Incident의
owner/Deployment UID/세대에 동일 Deployment floor-close CAS를 하고 fresh live로 정리한 뒤
ABORTED/release한다. 이미 다른 worker의 미확인 dispatch 표식이 있으면 hold/manual을 우선한다.
새 Policy UID 또는 다른 active 예약은 release하지 않는다. floor-close는 기존 safetyHold를 보존한다.

이전 binary가 만든 marker 없는 Reserved AlreadyApplied는 기존 live UID/hash/action 판별과
구조 guard로 재개한다. 과거 dispatch/ACK 불확실성을 소급 증명하지 못하는 PARTIAL 호환 경로다.
기존 binary의 실제 inflight 요청은 marker 보장 밖이며 writer 전환 게이트가 필수다.


## 최종 관측과 status 사이의 남는 경계

VERIFYING/AlreadyApplied의 마지막 guard는 Probe/Endpoint I/O 뒤 Deployment UID/spec/template,
owner/floor/hold를 새로 읽고 Node 구조를 확인한다. 따라서 그 authority 관측 전에 이미
저장된 hold나 workload 변경을 오래된 caller snapshot으로 무시하지 않는다.
그러나 마지막 Deployment authority GET·Node guard와 Incident status CAS는 서로 다른 객체의
연산이다. 그 마지막 authority GET 이후 새 hold/Node 변화와 terminal status 저장을 절대적으로
직렬화하지는 못한다. 이 간격의 교차객체 원자성, 모든 순간 변화 탐지 또는 지속적 정상은
보장하지 않는다. 저장된 Deployment hold가 쓰기 authority이며 status를 해제 허가로 사용하지 않는다.
