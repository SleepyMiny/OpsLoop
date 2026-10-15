# ADR-002 — Runtime Recovery Placement

- 상태: Accepted — Architecture v0.1 설계 결정; PR #2에서 코드·envtest 구현, 클러스터 수용시험 미실행
- 날짜: 2026-10-07
- 관련: [Architecture](../architecture/architecture-v0.1.md), [ADR-003](ADR-003-observability-driven-recovery.md), [Implementation Plan](../implementation-plan.md)

## 배경

OpsLoop는 실시간 자원 상태로 Recovery Target을 선택해야 한다. Kubernetes의 기본 스케줄링과 kubelet·Deployment 복구를 재구현하거나 서로 경쟁하는 임의 Pod 조작을 추가하면 실패 원인과 복구 책임을 검증하기 어렵다. Node가 단절된 경우 이전 Pod의 종료 확인을 기다리는 전략은 새 Node 복구를 막을 수 있다.

## 결정

- Custom Scheduler를 만들지 않는다.
- OpsLoop는 hard filter·score를 통해 Node를 선택한다.
- Deployment Pod template의 `nodeSelector[kubernetes.io/hostname]`에 선택 Node의 실제 hostname label 값을 patch한다.
- 기본 Scheduler가 제약 안에서 최종 배치한다. `spec.nodeName`을 직접 설정하지 않는다.
- 초기 대상은 replica 1개의 stateless Deployment 한 개다.
- `RollingUpdate`, `maxSurge=1`, `maxUnavailable=0`을 사용한다.
- 고유 `opsloop.io/attempt-id` annotation으로 동일 Node restart와 새 ReplicaSet을 구분한다.
- 기본 kubelet restart·eviction을 유지하고 OpsLoop action 예산과 별도로 기록한다.
- intent를 먼저 저장하고 UID·baseline template/image·resourceVersion 사전조건을 검증해 변경한다. 구현은 resourceVersion을 실은 `Update`를 쓴다(아래 구현 상태 기록, 구현: PR #2).
- 외부 desired state 변경은 SUPERSEDED로 중단한다. force delete와 자동 failback은 하지 않는다.

## 고려한 대안

| 대안 | 평가 |
| --- | --- |
| Custom Scheduler | 요구에 비해 범위가 크고 Kubernetes 판단을 중복 구현 |
| spec.nodeName | 기본 Scheduler 경로 우회 |
| 매 action마다 Node label 토글 | cluster 공용 metadata 변경과 경쟁 상태 증가 |
| 복잡한 nodeAffinity | 다중 후보 집합에는 유용하지만 단일 선택 대상을 검증하는 초기 범위에는 불필요 |
| Recreate | unreachable old Pod 종료 대기가 failover를 막을 수 있음 |
| 강제 Pod 삭제 | API 객체 삭제가 실제 프로세스 종료·fencing을 보장하지 않음 |

## 선택 이유

nodeSelector는 선택 결과를 Deployment에서 직접 확인할 수 있고 검증 경로가 단순하다. surge는 old Pod의 종료와 새 대상의 시작을 분리한다. 영속 action ID와 조건부 patch는 Controller 재시작·중복 전달·사용자 변경에서 복구를 이어가거나 안전하게 중단할 수 있게 한다.

## 결과와 Trade-off

- 새 Pod를 위한 surge resource와 quota가 필요하다. 기존 Pod가 종료됐다고 확인하기 전에 요청 자원을 빼지 않는다.
- Scheduler가 자원 변화로 Pending을 만들 수 있다. preflight가 scheduling 성공 보장은 아니다.
- partition 중 old/new 프로세스가 동시에 존재할 수 있다. stateless·중복 허용 범위에 한정하며 Stateful 서비스의 single-writer 보장이 없다.
- 성공은 현재 attempt의 Pod UID·ReplicaSet·Node와 직접 연결된 HTTP·안정화·EndpointSlice 결과로 판정한다.
- 전체 rollout 완료·old Pod 삭제를 성공 전제로 두지 않는다. `oldPodsOutstanding`을 별도로 표시한다.
- Application 소진 시 scale=0도 동일 precondition을 사용하며 quarantine 요청과 실제 종료 확인을 구분한다.
- Helm/GitOps와 nodeSelector·replicas 소유권을 조정해야 한다. 초기 Helm upgrade와 복구는 동시에 실행하지 않는다.

## 검증 기준과 재검토 조건

P4에서 Demo A 양방향 각 5회, patch 직후 재시작, 중복 action, 외부 image 변경, Pending, partition, 후보 없음, old Pod 종료 지연을 확인한다. Stateful·다중 replica·fencing 요구가 생기면 이 전략을 일반화하지 말고 별도 설계한다.

## 근거

- [Kubernetes Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
- [Kubernetes Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)

## 구현 상태 기록 (2026-10-13)

결정의 책임 경계는 유지한다. 구현에서 구체화하거나 달라진 점은 다음과 같다. 상세는 [Architecture §7](../architecture/architecture-v0.1.md#7-runtime-recovery-state-machine)과 [§14](../architecture/architecture-v0.1.md#14-security-boundary)에 있다.

- "patch"는 JSON Patch `test` 대신 live resourceVersion을 실은 Deployment `Update`로 구현했다. 선택 hostname label 값의 nodeSelector와 `opsloop.io/attempt-id` annotation을 결정적으로 기록한다. Controller ClusterRole은 Deployment를 읽기만 하고, 쓰기는 Enforce 승인 뒤 적용하는 namespace Role의 verb `update`(대상 `demo-api` 한 개)로 한정한다. (구현: PR #2)
- 외부 desired-state 변경은 template을 제외한 spec 전체의 `SpecHash`와 `TemplateHash`로 감지해 SUPERSEDED로 중단한다. 재개는 예약 시 계산한 `targetTemplateHash`로 이미 적용했는지 판정하며, terminal 진입 시 남은 Reserved attempt는 `ReservedOutcome`으로 `Patched`·`Cancelled`·`Superseded` 중 하나로 확정한다. (구현: PR #2)
- 증거 범위: envtest(kube-apiserver 1.35.0)로 조건부 변경 1회, 변경 전·후 종료 뒤 재개, 외부 image·replicas·paused 변경과 workload 교체의 SUPERSEDED, Pending의 `PLACEMENT_TIMEOUT`, 구 Pod가 남은 상태의 성공 판정을 확인했다. envtest에는 기본 Scheduler·kubelet이 없으므로 이 ADR의 Demo A 양방향 5회, partition, old Pod 종료 지연의 실제 cluster 검증은 실행하지 않았다.
