# Controller 중지 요청과 중지 완료 계약

Policy와 Deployment는 서로 다른 Kubernetes 객체다. `enabled=false` 또는
`mode=ObserveOnly` PATCH 성공은 **중지 요청 접수**이며, 응답 직후부터 workload
쓰기 0회를 보장하지 않는다. 마지막 Policy GET 이후 이미 진행 중인 요청은 완료될
수 있다. 완료를 확인하기 전에는 정리·원복·권한 회수·Policy 삭제를 시작하지 않는다.

## 완료를 판단하는 조건

다음 항목을 함께 읽고 확인한다. condition 하나나 observedGeneration만 사용하지 않는다.

1. 요청 대상 Policy UID가 같고 삭제 중이 아니며, 현재 disabled 또는 ObserveOnly다.
2. `status.conditions[type=Quiesced]`가 `status=True`, `reason=FenceClosed`다.
3. 해당 condition의 `observedGeneration == metadata.generation`이고
   `status.activeIncident`가 비어 있다. 새 ObserveOnly 예약은 같은 status CAS에서
   Quiesced를 False/ReservationPending으로 바꾼다.
4. condition message는 JSON이다. `version=2` (v1은 명시적 CAS로 승격), `policyName`, `policyUID`,
   `deploymentUID`, `closedThroughGeneration`을 확인한다. 대상 Deployment UID와
   소유 Policy UID가 일치하고 차단 상한이 현재 중지 generation 이상이어야 한다.
5. Deployment 최상위 `metadata.annotations[opsloop.io/execution-fence]`가
   같은 owner/Deployment UID와 충분한 차단 상한을 유지한다. resourceVersion은
   opaque 값이다. 숫자로 순서를 비교하거나 condition 기록 시점의 RV와 영구적으로
   같아야 한다고 가정하지 않는다.

이 완료는 이전 Policy 세대의 **아직 저장되지 않은 workload 쓰기**가 이후 저장될
수 없다는 뜻이다. 완료 전에 저장된 실행은 live template으로 Patched 등 결과를
정리한다. 모든 네트워크 응답 도착이나 rollout 완료를 기다리는 기능은 아니다.
재활성화/정책 변경으로 generation이 증가하면 이전 완료는 현재 상태의 증거가 아니다.

## 실행 및 중지 직렬화

Deployment annotation 형식은 다음과 같다.

```json
{"version":2,"policyUID":"...","policyName":"demo-api","deploymentUID":"...","closedThroughGeneration":7}
```

- 최초 실행은 Deployment snapshot 이후 live Policy를 읽고, 같은 Deployment RV로
  owner를 claim한다. claim은 metadata만 바꾸며, 그 뒤 Policy를 다시 확인한다.
- Policy UID/삭제/enabled/mode, Incident snapshot generation/spec hash 및
  activeIncident 이름/UID/sequence가 일치해야 한다. APIReader 실패는 허용하지 않는다.
- `safetyHold`가 존재하면 모든 실행 세대를 거절한다. 일반 spec 편집/Node 회복은 hold를 해제하지 않는다.
- 실행 세대가 `closedThroughGeneration`보다 커야 한다. owner와 차단 상한은
  재활성화 시에도 삭제·감소·자동 교체하지 않는다. 손상/미지원 버전은 fail-closed다.
- 기존 baseline/Resume 검사, deadline, metrics freshness 검사를 유지하며,
  실제 workload 변경도 같은 Deployment RV로 저장한다. 충돌 뒤 RV만 갱신하여
  과거 허가를 재사용하지 않는다. 다음 reconcile에서 전체 검증을 다시 수행한다.
- 중지는 같은 Deployment에 차단 상한을 올리는 CAS를 먼저 저장한다. 그 후 읽은
  Deployment로 Reserved 결과를 정리하고 같은 Policy의 active 예약을 해제한다.
  마지막으로 같은 Policy 세대에 완료 condition을 status CAS로 저장한다.
- workload가 먼저 저장되면 close CAS가 충돌하고 재관측한다. close가 먼저 저장되면
  과거 workload RV가 충돌한다. 나중에 새 Deployment snapshot을 읽은 오래된 실행은
  보존된 generation 상한으로 차단한다.
- fence 저장 응답 유실·재시작은 live fence를 확인하여 이어간다. 정리 실패나 ack 충돌은
  완료로 보고하지 않는다. 재활성화와 늦은 ack의 경합도 Policy RV로 거절한다.

Kubernetes Deployment는 최상위 annotation 변경에도 generation을 올린다.
fence는 Pod template/spec을 바꾸지 않으므로 자체적으로 새 rollout을 유발하지 않는다.
테스트와 관측은 generation 증가와 실제 template/nodeSelector 변경을 구분한다.

## ObserveOnly / 권한 / owner 수명

처음부터 fence 없는 ObserveOnly 경로는 Deployment metadata도 수정하지 않는다.
완료 True를 보고하지 않는다. 예약 없는 감시 상태는 Unknown/FenceNotEstablished이며,
예약이 생기면 False/ReservationPending일 수 있다. 정상 Monitoring은 계속 가능하다.
기존 Enforce fence 또는 예약이 있는 정책의 중지는 fence를 닫아야 한다.
Update Forbidden이면 완료를 확인할 수 없다. 기본 RBAC를 넓히지 않으며, Enforce
권한 회수는 완료 확인 후 별도 승인 절차로 한다.

다른 UID의 동명 Policy나 다른 owner는 fence를 인계받지 못한다. 교체된 Deployment에
이전 UID의 완료를 적용하지 않는다. Policy 삭제 자체는 stop completion이 아니다.
삭제 전 중지 완료가 필요하며 삭제된 owner의 자동 회수/이전 기능은 구현하지 않는다.

## 보장하지 않는 범위와 배포 전제

- raw Policy PATCH 응답 직후 workload 쓰기 0회: **미지원**. 실제 workload 경계의
  disable/ObserveOnly 두 한계 재현을 제품 회귀와 별도로 보존한다.
- 모든 Deployment writer는 이 프로토콜을 사용해야 한다. 구버전 Controller가 새 RV로
  쓰거나 외부 도구가 fence를 제거/감소시키는 동작을 API 서버에서 강제 차단하지 않는다.
  혼합 버전 writer를 제거하고 annotation 보존을 확인하기 전 Enforce를 허용하지 않는다.
  외부 writer까지 강제하려면 별도의 admission/권한 경계 설계와 검증이 필요하다.
- ABORTED/Quiesced는 자동 rollback, 기존 Pod/process fencing, Service 정상 보장이 아니다.
- CTL-02는 수집 이후 live Node/trigger 검증을 추가했으나 마지막 GET 이후 Deployment
  Update와의 강한 원자성은 제공하지 않는다. CTL-03은 관측/예약 UID·hash 결합과 재시작
  보호를 추가했다. 전역 rollout 제한과 실환경 전환 조건은 별도이며 Enforce는 BLOCKED다.
- UID가 없는 legacy pending 예약은 active pointer만 해제되고 원본 UID를 알 수 없는
  동명 Incident는 남을 수 있다. Quiesced=True를 그 고아의 상태 확정/삭제 승인으로
  사용하지 않는다. [writer 전환 게이트](controller-writer-transition.md)를 함께 확인한다.
- 기존 node-failure cleanup harness는 이 완료 확인을 자동 수행하지 않는다.
  해당 Enforce 정리 절차를 그대로 실행하지 않는다. 기존 SAF-01 UID/RV 보호는 유지한다.

검증 범위는 로컬 envtest/API CAS와 결정적 경합 테스트다. 실제 클러스터 중지/재시작,
구버전 업그레이드, 분할 네트워크, 실제 rollout/서비스 복구는 별도 승인 후 검증한다.

## 구 writer 배포 전환

전용 writer identity/RBAC/새 digest와 구 프로세스·권한 회수의 조건은
[전환 게이트](controller-writer-transition.md)를 따른다. 전용 identity만으로 이미 승인된
구 요청을 취소하거나 바이너리 버전을 인증한다고 해석하지 않는다.


## Fence v2 전환 및 안전 hold

v2는 v1의 owner/Deployment UID/floor를 같은 Deployment RV CAS로 보존하여 승격한다.
GET만으로 저장하지 않으며, v1과 v2 혼합 writer의 무중단 wire 호환은 제공하지 않는다.
기존 v1 fence reader는 v2를 fail-closed로 거절한다. Fence 자체를 모르는 예전 writer는
v2로 차단되지 않으므로 [writer 전환 게이트](controller-writer-transition.md)가 필수다.
CRD의 optional `status.runtime.safetyHaltReason`, `status.runtime.workloadDispatchActionID`를 먼저 반영해야 pending/사전 dispatch 표식이 보존된다.
이 status는 재시도/증거일 뿐 실행 권한이 아니다.

Node 안전 중단은 [관측 및 hold 계약](controller-node-safety-contract.md)을 따른다.
중지 close는 기존 hold identity/reason을 보존하고 floor만 올린다. enabled hold는
Quiesced=True가 아니며, disabled/ObserveOnly의 Quiesced는 기존 stop 완료 의미만 갖는다.
첫 버전에는 hold 제거/자동 해제 명령이 없다. annotation 수동 삭제는 검증된 해제 절차가 아니다.
