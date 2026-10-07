# ADR-002 — Runtime Recovery Placement

- 상태: Accepted — Architecture v0.1 설계 결정, 미구현
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
- intent를 먼저 저장하고 UID·baseline template/image·resourceVersion 사전조건을 검증해 변경한다.
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
