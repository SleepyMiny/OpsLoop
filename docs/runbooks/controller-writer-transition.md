# Controller writer identity 전환 게이트

이 문서는 실행 승인이 아니다. 아래 Kubernetes/권한/정책 변경은 각각 소유자의 승인 후
수행한다. 현재 배포된 9e7f91b 이미지에는 새 fence 및 CTL02/03 수정이 없다.

## 경계와 로컬 준비물

기본 `config/rbac`와 `config/manager`는 그대로 유지한다. 기본 identity
`opsloop-system/opsloop-controller`는 Deployment 읽기만 허용되며, Incident 생성과
CRD status 쓰기는 여전히 가능하다. 전체 리소스에 대해 read-only라는 뜻이 아니다.

승인된 successor를 실행할 때만 다음 별도 구성을 사용한다.

- `config/manager-fenced`: 기본 manager의 대체 overlay. 같은 Deployment 이름,
  replicas1, Recreate, `opsloop-controller-fenced-v1` ServiceAccount.
- `config/rbac/enforce`: 위 identity용 ServiceAccount, 기존 관측용 ClusterRole binding,
  leader election Role binding, `opsloop-demo/demo-api` Deployment `update`만 허용하는
  기존 namespace Role과 전용 binding. patch/delete/create/와일드카드 권한 없음.
- 기본 `opsloop-controller`는 새 Enforce binding의 subject가 아니다.
- 이미지 placeholder도 `opsloop-controller-fenced-v1`로 분리했다. owner overlay는
  이 새 key에 **새로 리뷰·검증·게시한 successor digest**를 명시해야 한다.
  예전 `opsloop-controller` key와 9e7f91b digest를 자동 상속하지 않는다.

Kustomize 구성/ServiceAccount 이름은 바이너리 attestation이 아니다. 구버전 바이너리를
새 identity로 실행할 수 있는 관리자/배포자는 이를 우회할 수 있다. 해당 identity를
사용하는 Pod 생성·변경 주체와 게시 digest의 신뢰는 운영 전제다. 이번에 admission
webhook/이미지 서명 강제 정책을 추가한 것은 아니다.

## 전환 성공 조건 (모두 충족 전 BLOCKED)

1. **정책/작업 기록**: 대상 Policy UID·세대·현재 spec, active reservation/Incident와
   Deployment UID/template/fence를 읽고 저장한다. 새 프로토콜 writer라면 stop 계약의
   Quiesced 조건 전체를 확인한다. 구버전에는 이 ack가 없으므로 추정하지 않는다.
   기존 action의 적용 여부를 결정할 수 없으면 전환하지 않는다.
2. **구 writer 종료**: 기존 Deployment를 replicas0으로 하고 모든 구 controller 실행
   위치를 확인한다. 별도 manager/외부 프로세스/분할된 노드도 포함한다. Pod API 객체가
   삭제됐거나 Ready가0인 사실만으로 실제 프로세스가 종료됐다고 판단하지 않는다.
   Recreate와 leader election도 강제 process fencing을 대신하지 못한다.
3. **구 자격/권한 회수**: 구 identity의 Deployment update를 주는 모든 RoleBinding,
   ClusterRoleBinding, group grant, 외부 authorizer 권한을 확인·회수한다. 기존
   `opsloop-controller-enforce` binding이 구 SA를 가리킨다면 새 subject로 교체될
   예정이라는 사실만 믿고 grant부터 진행하지 않는다. 먼저 구 grant 회수와 실제
   거절을 확인한다. 허가받은 old process/credential을 모두 제거·폐기해야 한다.
4. **이미 승인된 요청 종료**: RBAC 회수는 이미 승인되어 API 서버에서 진행 중인 요청을
   취소하는 기능이 아니다. API server별 전파와 이전 update 요청의 종료를 확인한다.
   단일 `can-i=no` 또는 고정 sleep은 drain 증거가 아니다. 운영 API server의 요청
   timeout/inflight/audit 증거와 구 요청 부재를 확인할 수 없으면 BLOCKED로 남긴다.
5. **현재 상태 재관측**: 위 단계 뒤 Deployment UID/RV/spec과 Incident 결과를 다시 읽어
   저장된 작업을 정리한다. 남은 Reserved/결과 불명 action을 무시하지 않는다.
   새 스키마의 workloadUID가 없는 미확인 예약은 현재 UID로 채워 이어가지 않는다.
   해당 예약의 검토/정리는 별도 승인 범위다. WorkloadUID와 IncidentUID가 모두 없는
   legacy pending 예약은 active pointer를 비우지만, 동명 Incident는 원본 UID를 증명할
   수 없어 status를 바꾸거나 삭제하지 않는다. 따라서 Quiesced=True도 이 고아 Incident의
   결과 확정/정리 완료를 증명하지 않는다. 잔존 nonterminal Incident 검토가 끝나지 않으면
   Enforce 전환은 BLOCKED다. schema가 먼저 반영되어야 새 예약의 UID가 보존된다.
6. **새 identity만 시작**: 기본 CRD/RBAC가 준비된 뒤 새 전용 Enforce RBAC와
   `manager-fenced`를 승인 범위에서 준비하고 reviewed successor digest로 시작한다.
   Policy는 처음에는 disabled/ObserveOnly를 유지한다. 구 identity는 fresh-RV workload
   update/patch 금지, 새 identity는 지정 target update만 허용됨을 실제 API에서 검증한다.
   `auth can-i`만으로 실제 요청 거절을 대체하지 않는다. 새 Pod identity/imageID도 확인한다.
7. **Enforce 별도 승인**: 로컬 CTL02/03·Fence 회귀 통과는 실제 Enforce 승인이 아니다.
   당시 3노드/운영 서비스/메트릭/후보·trigger 상태, 격리 실험 범위와 정리 계획을 확인하고
   별도 승인한다. 같은 identity로 구버전을 실행하는 경로가 남으면 진행하지 않는다.

처음부터 ObserveOnly이고 Enforce grant가 없던 설치도 새 writer identity를 쓰기 전에는
구 프로세스/자격/승인된 요청 부재를 확인한다. 기존 enabled ObserveOnly 정책을 자동으로
Enforce로 바꾸지 않는다.

## 실패·원복

- 어느 단계라도 확인이 안 되면 새 update 권한 부여/Enforce 활성화를 중단한다.
- 새 writer를 중지할 때는 [stop 계약](controller-stop-contract.md)의 완료 증거를 먼저
  확인한다. 신규 write와 미확정 작업이 닫힌 다음 권한 회수/프로세스 종료를 순서대로 한다.
- 구버전 image/구 SA의 update grant를 되살리는 자동 rollback은 금지한다. 필요하면
  update 권한 없는 기본 ObserveOnly 배포로 돌아가는 별도 승인안을 준비한다.
- fence annotation 삭제/감소/owner 자동이전, 기존 Pod 삭제, 물리 서비스 재시작을 원복으로
  수행하지 않는다. 정책이 교체됐거나 UID가 다르면 기존 stop/cleanup을 적용하지 않는다.

## 테스트 증거와 한계

`internal/deploymentsecurity/authorization_test.go`는 실제 manifest RBAC를 로컬 envtest에
적용한다. SA 이름과 같은 username/groups의 임시 x509 identity로 authorization을 검증한다.
구 SA fresh-RV update403, 새 SA 지정 target update 허용, 다른 target/namespace 및
patch/delete/Secret read403, additive 구 grant를 부여했다 회수한 뒤 거절을 확인한다.
이것은 실제 클러스터, projected token 발급/폐기, Pod 종료, 분할 노드, multi-apiserver
전파 또는 승인된 inflight 요청 취소의 검증이 아니다.

Kubernetes RBAC는 권한 합산 방식으로 deny rule이 없다. 다른 grant가 남으면 이 최소 Role을
적용해도 구 writer를 차단하지 못한다. 출처:
[RBAC 공식 문서](https://kubernetes.io/docs/reference/access-authn-authz/rbac/),
[Authorization 공식 문서](https://kubernetes.io/docs/reference/access-authn-authz/authorization/),
[ServiceAccount 관리](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/).


## Fence v2 추가 전제

새 successor는 내부 `deploymentExecution` 경계에서 최종 허가와 same-RV workload/fence CAS를
함께 소유한다. v1 fence는 owner/UID/floor 보존 CAS로 v2로 승격되며 safetyHold는 모든
generation 쓰기를 차단한다. 기존 v1 reader는 v2를 거절하지만 pre-Fence writer는 여전히
프로토콜을 무시하므로 위 구 writer/권한/이미 승인된 요청 종료 게이트를 생략할 수 없다.
새 CRD의 `status.runtime.safetyHaltReason`, `status.runtime.workloadDispatchActionID`를 먼저 준비하고 기존 pending/held action을
live UID/hash/action과 annotation으로 정리한다. Node 회복이나 일반 정책 편집으로 hold를
자동 해제하지 않는다. 해제 도구/운영 절차와 실환경 전환은 별도 검증·승인 전 BLOCKED다.
