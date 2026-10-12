# P2 관측 스택 확인 — 2026-10-12

기준: [ADR-003](../adr/ADR-003-observability-driven-recovery.md), [Architecture](../architecture/architecture-v0.1.md). **관측 스택의 현재 설치와 수집을 읽기 전용으로 확인했다. Demo API·Application/OpsLoop metric·이미지 pipeline·native ARM 검증을 포함하는 P2 전체 완료는 아니다.** 확인 시각: 2026-10-12 08:27 UTC / 17:27 KST.

## Helm 설정과 배치

`monitoring` namespace의 `opsloop-mon`: revision **1**, status **deployed**, chart **kube-prometheus-stack-91.8.1**, app/operator **v0.94.1**. 사용자 작성 [values](../../infra/observability/kube-prometheus-stack.values.yaml)는 변경하지 않았다. SHA256은 `55bbc062740adf58f79fa0bf661828c3cc3f0c2a15383c1aaadd52b84f9e6ddb`다.

`helm list`의 release metadata와 `helm get values -o json`을 subprocess 메모리에서 처리하고, 지정된 **36개 non-secret 설정 경로**만 로컬 YAML과 비교했다. 불일치는 0개다. raw Helm values·admin 비밀번호·Secret manifest·kubeconfig를 출력하거나 파일로 저장하지 않았다. 비교 범위는 비활성화한 control component/rule, placement·replica·retention·scrape/evaluation·resources·PVC·service 설정이며 임의 추가 Helm key나 모든 chart default의 동일성을 주장하지 않는다.

| 구성요소 | 관측 이미지 버전 | 배치 |
| --- | --- | --- |
| Prometheus | v3.15.0-distroless | control, 1개 |
| Grafana | 13.2.2-distroless | control, 1개 |
| kube-state-metrics | v2.20.0 | control, 1개 |
| Prometheus Operator / reloader | v0.94.1 | control |
| node exporter | v1.12.1-distroless | 각 노드 1개, 총 2개 |
| Alertmanager | v0.34.1 | control, 1개 |

모니터링 Pod 총 **7개 Running/Ready, restart 0**. node exporter를 제외한 모든 Pod가 `opsloop-control`에 있다. namespace의 Service **8개 모두 ClusterIP**다. Grafana 로그인·Dashboard·alert notification 전달은 검증하지 않았다.

Prometheus requests 100m/512Mi, limits 1CPU/1536Mi; retention **3d / 6GB**. Grafana requests 50m/128Mi, limits 500m/512Mi. 정확한 추가 resource 값은 사용자 values에서 관리한다. 실행 중 resource headroom·장기 disk 증가·retention 회수는 측정하지 않았다.

## 저장소와 복구 한계

| 대상 | 관측 |
| --- | --- |
| Prometheus PVC | 10Gi, Bound, local-path |
| Grafana PVC | 2Gi, Bound, local-path |
| 두 PV | nodeAffinity `opsloop-control`, reclaimPolicy Delete |
| local-path StorageClass | provisioner `rancher.io/local-path`, Delete, WaitForFirstConsumer |

local-path 데이터는 control VM 디스크에 묶인다. PVC를 다른 노드로 옮기는 것만으로 데이터 복구가 되지 않는다. Delete 정책이므로 삭제·재설치를 복구 절차로 삼지 않는다. snapshot/backup·복원·디스크 장애·control 재시작 테스트는 미실행이다. 보존·백업·복구를 별도 승인된 작업으로 준비해야 한다.

## 실제 수집 targets

기존 Kubernetes Service proxy를 통한 **GET**으로 `/api/v1/targets`를 조회했다. port-forward·exec·Pod 생성은 실행하지 않았다. 실제 chart Service 이름은 `opsloop-mon-kube-prometheu-prometheus`이며 chart의 이름 축약을 검증 도구가 Service metadata에서 찾아 처리한다.

| target | 수 | 확인한 매핑 |
| --- | --- | --- |
| apiserver | 1 | `100.73.4.76:6443` |
| CoreDNS | 1 | `/metrics` |
| kubelet | 2 | 두 노드 각각 tailnet IP `:10250`, `/metrics` |
| cAdvisor | 2 | 두 노드 각각 `/metrics/cadvisor` |
| kubelet probes | 2 | 두 노드 각각 `/metrics/probes` |
| node exporter | 2 | `100.73.4.76:9100`, `100.82.241.77:9100` |
| kube-state-metrics | 1 | `/metrics` |
| Grafana / Operator | 1 / 1 | `/metrics` |
| Prometheus / Alertmanager | 2 / 2 | 본체·reloader |

합계 **17개 모두 UP, lastError 없음**. exporter는 instance IP로 실제 Node를 대조했고 kubelet은 node label과 instance를 함께 대조했다. exporter target에는 node label이 없어 아직 Controller의 최종 identity·duplicate series 처리 계약이 검증된 것은 아니다. actual CPU/memory PromQL·원본 표본 시각·NaN·누락·stale·query timeout·API 불일치 처리는 미검증이다. target UP만으로 resource-aware 복구 후보가 안전하다고 판정하지 않는다.

## 설계와 현재 설정의 차이

1. ADR 초기 scrape interval은 10초, max sample age는 30초다. 실제 Prometheus 기본 scrape/evaluation과 exporter는 **30초**, cAdvisor는 **10초**다. sample age 30초와 같은 길이의 exporter scrape는 지연·실패 때 freshness 여유가 부족할 수 있다. 사용자 values는 보존했으며 P3 전에 지연을 측정하고 scrape 또는 freshness 정책을 명시적으로 결정한다.
2. 최소 스택에서 Alertmanager는 필수가 아니지만 현재 release에는 1개가 실행 중이다. 현재 자원·기능 차이로 기록하며 제거하거나 upgrade하지 않았다.
3. Application/OpsLoop metrics·custom dashboard·알림 전달·장기 관측·관측 장애 시 WAITING 동작은 남아 있다.

[읽기 전용 검증 스크립트](../../scripts/README.md)의 live 실행은 위 baseline에 대해 PASS(exit 0)였다. offline fixture는 노드·수집 누락/오류·IP identity·placement·storage·Helm drift·민감정보 비출력을 검사한다. 설치·Helm upgrade·restart·값 수정·외부 게시를 수행하지 않았다.
