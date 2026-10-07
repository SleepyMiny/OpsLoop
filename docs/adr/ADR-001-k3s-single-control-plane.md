# ADR-001 — k3s Single Control Plane

- 상태: Accepted — Architecture v0.1 설계 결정, 미구현
- 날짜: 2026-10-07
- 관련: [Architecture](../architecture/architecture-v0.1.md), [Implementation Plan](../implementation-plan.md), [ADR-005](ADR-005-hybrid-multi-architecture-testbed.md)

## 배경

OpsLoop는 Managed Kubernetes가 대신 수행하는 부분을 줄이고 Node 상태, Scheduling, Controller, CRD, 자원, Failure와 Placement를 직접 관찰하는 Testbed다. Laptop Ubuntu VM, Physical Server, Azure ARM64 VM의 세 Node가 WAN을 포함해 연결된다. 실험 목적은 worker 장애 복구이며 WAN control-plane HA 구축이 아니다.

## 결정

- Laptop Ubuntu VM 한 곳에 k3s server/control plane을 둔다.
- 단일 server의 기본 SQLite datastore를 사용한다. 외부 DB나 etcd cluster를 추가하지 않는다.
- Physical Server와 Azure VM은 k3s agent/worker다.
- Laptop VM은 workload 실행이 가능한 Recovery Candidate다.
- Laptop Host는 cluster Node가 아니며 장애 주입, HTTP 측정, Engineering Runner를 담당한다.
- OpsLoop·Prometheus·Grafana를 Laptop VM에 배치하고 자원을 예약한다.
- k3s version을 고정하고 datastore에 맞는 backup/restore 절차를 구현 단계에서 검증한다.

## 고려한 대안

| 대안 | 평가 |
| --- | --- |
| AKS로 시작 | 운영 관찰·제어의 일부를 managed service가 담당하고 초기 목적과 어긋남 |
| 세 Node에 etcd HA | WAN 지연·partition·운영 복잡성이 증가하고 다른 실험 결과를 왜곡 |
| kubeadm 기반 cluster | 목적은 충족할 수 있으나 bootstrap·운영 부담이 증가 |
| 외부 SQL datastore | 이 규모에서 불필요한 서비스·인증·장애 경계 추가 |
| Host를 네 번째 Node로 등록 | 장애 주입·측정·Runner와 복구 대상의 경계를 흐림 |

## 선택 이유

k3s는 단일 server 실험을 작게 시작하면서 Kubernetes API·기본 Scheduler·Controller·CRD를 직접 다룰 수 있다. SQLite는 이 구조에 별도 데이터 서비스가 필요하지 않게 한다. control plane은 안정된 실험 기준점으로 두고 worker의 상태와 architecture에 따른 복구 결정을 관찰한다.

## 결과와 Trade-off

- Laptop VM과 Host는 SPOF다. 절전·전원·자원 고갈 시 자동 복구를 지속할 수 없다.
- control plane failure 자동 복구는 초기 수용 범위 밖이다.
- Laptop 부하 실험은 시스템·관측 resource를 침해하지 않도록 한도를 둔다.
- HA나 Production 적합성을 주장하지 않는다.
- Production 대안은 AKS 또는 적절한 region 내 HA와 private networking으로 별도 설명한다.

## 검증 기준과 재검토 조건

P1에서 Node 3개의 Ready·architecture·identity, API 접근 제한, version 고정, backup/restore 계획을 확인한다. control plane 장애 허용이나 Production 요구가 생기면 이 결정을 새 ADR로 재검토한다. 세 Node라는 이유만으로 HA라고 표현하지 않는다.

## 근거

- [K3s Cluster Datastore](https://docs.k3s.io/datastore)
- [K3s Requirements](https://docs.k3s.io/installation/requirements)
