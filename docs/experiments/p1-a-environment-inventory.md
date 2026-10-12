# P1-A 환경 검증 — 2026-10-12

기준: [Architecture v0.1](../architecture/architecture-v0.1.md), ADR-001~005. 실제 Azure 생성·변경과 bootstrap은 수행하지 않았다. P1-A 조사·정적 준비 결과이며 **P1 완료 또는 배포 승인 아님**.

## Inventory

| 대상 | 실제 관측 | 판정 |
| --- | --- | --- |
| Host | Arch Linux, Linux 7.2.9-arch1-1, x86_64, Core Ultra 5 225H, 14 cores | Host는 Kubernetes Node로 등록하지 않음 |
| RAM / disk | RAM 31,483 MiB, Available 약 26,567 MiB, swap 8,191 MiB; `/` 가용 약 844 GiB | 4 vCPU / 8 GiB Laptop VM을 위한 정적 여유 존재; 지속 부하 검증 아님 |
| 가상화 | VT-x, `/dev/kvm` 존재; virsh/VBoxManage 없음, libvirtd/virtqemud inactive; 프로세스 이름에서 실행 VM 식별 못함 | Ubuntu VM 존재 여부를 단정하지 않음 |
| 시간 / 전원 | Asia/Seoul, NTP synchronized; logind 설정은 lid suspend 기본값 주석, drop-in 없음; KDE power manager 실행 | 실제 GUI 전원 정책·절전 방지 미검증 |
| LAN | Wi-Fi `192.168.45.95/24`, gateway `192.168.45.1`, MTU 1500 | 현재 LAN만 확인 |
| Docker | daemon active, bridge/host/none; bridge `172.17.0.0/16` | 추가 사용자 bridge 없음 |
| Tailscale | daemon active, BackendState Running, Host `archgram`, `100.80.16.28`, MTU 1280, peers 0 | 로그인/Host 실행 확인; 노드 간 연결·ACL·direct/relay 미검증 |
| Azure | 기본 구독 `Azure for Students`, Enabled | 구독 로그인 확인; 잔여 credit·만료·비용 상한 미조회 |
| Laptop Ubuntu VM / Physical Server | tailnet peer 없음; 명확한 hostname/IP/SSH 관리 경로 미식별 | 임의 LAN 스캔·SSH 하지 않음; OS/자원/route 미검증 |

git/gh/az/terraform/docker/buildx/kubectl/helm/tailscale/ssh/go/jq/curl/codex 실행 파일을 확인했다. 관측 버전: Terraform 1.15.9, Tailscale 1.102.5, Go 1.27.1-X:nodwarf5, kubectl 1.37.1, Helm 4.3.0, Buildx 0.37.2. 설치된 버전이 후속 k3s/chart와 호환된다는 증거는 아니다. k3s version과 chart version은 P1-B에서 지원 범위 확인 후 고정한다.

## Azure 판정

우선 후보는 **Korea Central / Standard_D2pls_v6**다. 실제 SKU API는 ARM64, 2 vCPU, 4 GiB, Gen2, accelerated networking 지원을 반환했다. Korea Central의 제한은 `type=Zone`, `zones=[2]`, `NotAvailableForSubscription`이며 Location 전체 제한이 아니다. 초기 Terraform에는 zone을 지정하지 않는다. zone 2는 선택하지 않는다. 한국 Host와 지리적으로 가까운 후보라는 선택이며 latency를 측정한 결과는 아니다.

Japan East에서도 D2pls_v6 및 D2pls_v5의 restrictions 빈 목록을 확인했다. Southeast Asia에서는 두 SKU 모두 Location 단위 `NotAvailableForSubscription` 제한이 있어 추천에서 제외한다. Korea Central을 사용할 수 없을 때 Japan East를 차선 region으로 재검사한다. 대안은 D2pls_v5(ARM64 2 vCPU / 4 GiB) 또는 D2plds_v6(ARM64 2 vCPU / 4 GiB, local disk 추가)이며 별도 family quota와 비용 확인이 필요하다. D2ps 계열 2 vCPU / 8 GiB는 메모리 증가 대안으로만 다룬다. 동일 SKU의 zone 제한과 region 전체 제한을 혼동하지 않는다.

| 항목 | 결과 | P1-B gate |
| --- | --- | --- |
| Compute / Network / ContainerRegistry / Storage providers | 모두 `NotRegistered` | 사용자 승인 후 필요한 provider 등록; Storage는 backend 선택에 따라 필요 |
| quota: Korea Central / Japan East / Southeast Asia | `az vm list-usage` 모두 `[]`; 일본은 query 없이도 `[]` | 지역 total 및 선택 family의 limit/currentValue를 확인할 때까지 차단. 빈 목록은 0 또는 무제한이 아님 |
| 기존 VNet | `az network vnet list` → `[]` | 이 구독의 조회 결과 범위에서 VNet 없음 |
| Ubuntu image | Korea Central / Japan East: `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`, Arm64, V2, plan null | 실행 직전 동일 exact version 재조회; image 존재가 VM allocation 보장은 아님 |
| capacity | apply/allocation 없음 | SKU 카탈로그·restriction 조회로 실시간 capacity 보장 불가 |
| ACR 이름 / RBAC / policy | 이름 미선정; 생성 및 역할 할당 권한 미검증 | 이름 가용성·RG 생성 권한·정책 확인; P1 이후 OIDC 역할은 별도 승인 |

provider 미등록은 quota 빈 응답의 가능한 관련 조건이나, 원인으로 확정하지 않았다. provider 등록은 Azure 변경이므로 P1-A에서 실행하지 않았다. [Azure quota 문서](https://learn.microsoft.com/en-us/azure/virtual-machines/quotas)는 regional quota와 family quota를 모두 요구하고 capacity를 별도로 구분한다. [Dplsv6 문서](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dplsv6-series)는 accelerated networking을 필수로 명시한다.

## CIDR plan

| 네트워크 | 확정할 P1 주소 계획 | 상태 |
| --- | --- | --- |
| 현재 LAN | `192.168.45.0/24` | 관측 |
| Docker bridge | `172.17.0.0/16` | 관측 |
| Azure VNet | `10.80.0.0/16` | 신규 계획 |
| Azure worker Subnet | `10.80.1.0/24` | VNet 내부 |
| Kubernetes Pod | `10.42.0.0/16` | k3s server 설정에 명시 |
| Kubernetes Service | `10.43.0.0/16`, DNS `10.43.0.10` | k3s server 설정에 명시 |
| Tailscale | `100.64.0.0/10`; IPv6 `fd7a:115c:a1e0::/48` | underlay 예약; Pod/Service는 IPv4 단일 stack |

Python ipaddress로 LAN/Docker/VNet/Pod/Service/Tailscale IPv4의 15쌍이 모두 비중첩임과 Subnet 포함 관계를 확인했다. 기본 route `0.0.0.0/0`는 주소 할당 대역으로 비교하지 않는다. Host all-table routes에 다른 subnet route가 관측되지 않았다. **두 Ubuntu 노드·다른 LAN/VPN의 미확인 경로까지 충돌 없다고 확정할 수는 없다.** 이 주소를 설계 기준으로 채택하되 P1-B에서 각 노드 route/address와 tailnet advertised routes를 확인한 뒤 실행한다.

## Architecture와 차이·위험

- Host Linux 조건은 충족한다. 3-Node topology는 아직 식별/검증되지 않았다. VM 4 vCPU / 8 GiB는 권장값이며 현재 할당값이 아니다.
- 선호 ARM SKU는 카탈로그에서 확인됐으나 provider 등록과 quota가 미해결이다. Terraform은 배포 gate를 닫은 검토용 준비물로 제공한다.
- Tailscale MTU 1280 위 VXLAN encapsulation을 검증해야 한다. Flannel payload MTU를 1230 수준의 시험 출발점으로 고려하되 자동 계산값·실제 link MTU·대형 HTTP payload를 확인하기 전 확정하지 않는다. public UDP 8472는 열지 않는다.
- NSG 기본 deny inbound는 public Kubernetes 노출을 막지만 Tailscale direct 경로가 제한되고 relay로 연결될 수 있다. 직접 연결 가능 여부는 실제 노드에서 검증한다.
- IPv4 Pod/Service CIDR은 underlay와 분리한다. tailscale subnet-router로 Pod/Service route를 광고하지 않으며 Host의 HTTP 검증은 node tailnet IP의 고정 Service/NodePort 경로를 사용한다.
- Architecture 목표 디렉터리 `infra/testbed` 대신 P1 범위 분리를 위해 실제 root는 `infra/terraform/p1`이다. backend bootstrap은 별도 root로 후속 준비하며 testbed state와 분리한다.
- managed OS disk는 VM `os_disk` 블록으로 관리한다. 별도 디스크 리소스를 중복 생성하지 않는다. Standard static Public IP는 명시적 egress를 위해 추가하며 inbound 허용을 뜻하지 않는다. [Azure outbound 문서](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access).
- 비용 견적·잔여 credit·예산 상한, sleep 방지, 서버 전원 장애 후 복구 경로, ACL 변경 권한, remote state 및 k3s backup은 P1-B 선행 작업이다.

## 수행 범위와 검증

읽기 전용 Host/Azure CLI 및 공식 문서 조회, 로컬 파일 작성, Terraform fmt/init(-backend=false)/validate만 수행했다. Azure CLI는 기존 인증을 사용했고 token/credentials를 조회·출력·복사하지 않았다. 민감정보를 포함하지 않는 선택된 inventory만 기록했다.

Terraform 결과와 P1-B 절차: [P1 runbook](../runbooks/p1-platform-preparation.md). apply/plan/Azure 변경/Tailscale 설정/k3s 설치/firewall 변경/GitHub 설정/commit/push는 실행하지 않았다.
