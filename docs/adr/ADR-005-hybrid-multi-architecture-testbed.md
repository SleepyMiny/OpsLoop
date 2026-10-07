# ADR-005 — Hybrid Multi-Architecture Testbed

- 상태: Accepted — Architecture v0.1 설계 결정, 미구현
- 날짜: 2026-10-07
- 관련: [Architecture](../architecture/architecture-v0.1.md), [ADR-001](ADR-001-k3s-single-control-plane.md), [Implementation Plan](../implementation-plan.md)

## 배경

Local AMD64 Primary와 Cloud ARM64 Recovery Candidate의 차이를 실제로 관찰해야 한다. 제한된 Cloud 자원과 WAN 연결은 후보 평가·이미지 호환·CI/CD·네트워크 재현성을 검증하는 요소다. 이 실험을 Enterprise Production Architecture로 과장하거나 Azure 서비스를 불필요하게 늘리지 않는다.

## 결정

- Laptop VM AMD64 server, Physical AMD64 worker, Azure ARM64 worker의 세 Node를 사용한다.
- Tailscale underlay 위에서 k3s Flannel VXLAN을 사용한다. node-ip와 flannel interface를 명시한다.
- Kubernetes 포트는 public에 직접 개방하지 않는다. tailnet ACL과 OS firewall을 적용한다.
- Azure ARM 후보는 Standard_D2pls_v6, 2 vCPU / 4 GiB이며 region·size·image는 변수화한다.
- Terraform으로 RG, VNet, Subnet, NSG, NIC, VM, Managed OS Disk, ACR 및 필요한 인증·egress·state 리소스를 관리한다.
- 명시적 egress를 위한 NIC Standard Public IP를 사용하고 public inbound는 차단한다. bootstrap SSH가 필요하면 운영자 IP만 일시 허용한다.
- OS·Tailscale·k3s bootstrap은 Terraform 리소스 생성과 분리해 idempotent하게 만든다.
- ACR에 같은 tag/index의 linux/amd64·linux/arm64 image를 게시한다.
- GitHub Actions는 모든 PR을 검증하되 보호된 main만 OIDC로 ACR에 게시한다.
- Node pull에는 별도 repository-scoped read-only token을 사용한다. OIDC가 Node pull 인증을 대신하지 않는다.

## 고려한 대안

| 대안 | 평가 |
| --- | --- |
| AMD64만 사용 | architecture 호환과 동일 image index 복구를 입증하지 못함 |
| Raspberry Pi | 대체 가능한 후보이나 현재 목표는 Azure ARM64 VM |
| public Kubernetes 포트 | 불필요한 노출과 운영 위험 증가 |
| 초기 VPN Gateway·Private Endpoint | Production 대안으로 유효하나 MVP 비용·구성 범위를 늘림 |
| 기본 outbound에 의존 | Azure API·subnet 기본값에 따라 재현성 저하 |
| 모든 branch의 ACR push | repair code에 게시 권한을 부여하는 trust 문제 |
| architecture별 서로 다른 tag | 동일 버전·배치 전환 추적이 복잡해짐 |
| Full AKS Migration | 초기 운영 관찰 목표와 다른 프로젝트가 됨 |

## 선택 이유

Tailscale은 Local과 Azure를 사설 실험망으로 연결하는 초기 부담을 줄인다. ARM worker는 실제 배치와 image 호환을 검증할 수 있다. Terraform·version 고정·Multi-Arch provenance는 환경 재현과 장애 원인 분석을 지원한다. 인증·검증·게시를 분리하면 AI branch가 권한을 획득하지 않고도 PR gate를 통과할 수 있다.

## 결과와 Trade-off

- Tailscale의 direct/relay, WAN latency, 이중 encapsulation의 MTU가 결과에 영향을 준다.
- 단일 control plane이므로 Hybrid는 HA를 뜻하지 않는다.
- Public IP 보유와 public Kubernetes 노출은 다른 문제다. NSG·OS firewall·ACL 검증이 필요하다.
- D2pls_v6는 Accelerated Networking을 요구하며 실제 region·quota·Ubuntu ARM image는 확인해야 한다.
- 4 GiB worker에 Prometheus나 Engineering build를 함께 올리지 않는다.
- remote state bootstrap과 실험 state를 분리하고 destroy·secret 교체·cleanup 절차를 유지해야 한다.
- QEMU smoke는 native ARM 검증이 아니다. Demo A에서 실제 ARM 실행이 필수다.
- 장기 client secret 없는 CI를 지향하지만 Node pull token의 별도 만료·교체 책임은 남는다.
- Production의 AKS·Azure VPN·private networking·HA·fencing은 별도 요구와 비용으로 설계한다.

## 검증 기준과 재검토 조건

P1에서 region·quota·image, network·MTU·public 미노출·명시적 egress를 확인한다. P2/P4에서 같은 index의 AMD64·ARM64 native 실행과 resource-aware 선택을 입증한다. Private-only 또는 Production 운영 요구가 생기면 Azure network와 control-plane 설계를 새 ADR로 재검토한다.

## 근거

- [Azure Dplsv6](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dplsv6-series)
- [Azure Default Outbound Access](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access)
- [K3s Network Options](https://docs.k3s.io/networking/basic-network-options)
- [Docker Multi-platform Builds](https://docs.docker.com/build/building/multi-platform/)
- [GitHub Azure OIDC](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-azure)
- [ACR Repository Permissions](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-token-based-repository-permissions)
