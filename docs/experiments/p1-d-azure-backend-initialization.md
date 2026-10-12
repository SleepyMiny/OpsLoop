# P1-D — Azure Remote Backend 초기화 및 ARM64 배포 gate

검증일: 2026-10-13 KST. 기준 commit: `00207e0`. 기존 [허용 지역 조사](p1-c-azure-allowed-regions.md), [Architecture v0.1](../architecture/architecture-v0.1.md), [배포 runbook](../runbooks/azure-arm64-deployment.md), [`infra/terraform/p1`](../../infra/terraform/p1)을 재사용했다. Backend 생성·Provider 등록은 이전 승인 단계의 결과이며 이번에는 재실행하지 않았다.

## 1. 실제 Backend 연결 검증

| 검증 | 이번 명령 결과 |
| --- | --- |
| 활성 구독 | Azure for Students / Enabled; 승인된 구독 ID와 일치 |
| Network / Storage / ContainerRegistry | 모두 Registered; 자동 등록 또는 재등록 없음 |
| 기존 Storage / private container | 존재; Entra + CLI data-plane 조회 성공 |
| 실행 환경·접근 | 실제 Arch 호스트; 현재 public IPv4가 기존 firewall 허용 규칙과 일치 |
| 보안·권한 | default Deny, Shared Key·anonymous blob access 비활성; 실제 운영자 container scope Storage Blob Data Contributor 확인; 설정 변경 없음 |
| private 설정 | `~/.config/opsloop/private/backend.hcl` 존재, 운영자 소유·0600; CLI/Entra 및 대상 일치; 내용 공개·추적 없음 |
| 초기화 전 State | 해당 Remote State Blob 없음; root의 local State 없음 |
| 초기화 후 State | Blob 실제 존재, 181 bytes, lease unlocked / available |
| 연결 확인 | default workspace, `terraform state list`의 관리 리소스 0개 |
| 제한 준수 | State 대체·마이그레이션·삭제·force-unlock·reconfigure 없음; Plan/apply/destroy 없음 |

State 존재는 전후 Blob API 결과로 확인했다. 초기화 자체가 Blob을 만들 것이라고 추정하지 않았으며 실제 관측한 결과다. 이번 초기화로 빈 State가 생겼지만 VM을 비롯한 testbed 리소스는 생성되지 않았다. 기존 State 내용이나 실제 Backend 설정값은 저장소에 복사하지 않았다.

실행 명령의 private 파일 경로는 다음과 같으며 State·설정·인증정보 내용은 출력하지 않았다.

```bash
terraform -chdir=infra/terraform/p1 fmt -check
terraform -chdir=infra/terraform/p1 init -input=false -lockfile=readonly -backend-config="$HOME/.config/opsloop/private/backend.hcl"
terraform -chdir=infra/terraform/p1 validate
terraform -chdir=infra/terraform/p1 workspace show
terraform -chdir=infra/terraform/p1 state list
```

fmt/init/validate는 모두 exit 0이다. `python3 -m unittest discover -s tests -v`는 25개 모두 통과했다. Terraform 1.15.9 / AzureRM 4.64.0 및 lockfile을 유지했다. `.terraform/`의 backend metadata는 Git 제외된 로컬 runtime 파일이다. 현재 기본 workspace/State에 다시 새 State 생성 명령을 적용하거나 저장소 밖 설정을 재작성하지 않는다. Entra + container 범위 권한은 [공식 backend 계약](https://developer.hashicorp.com/terraform/language/backend/azurerm)을 따른다.

## 2. 현재 ARM64 조건 — 통과와 미확정을 구분

03:30 KST의 선택 후보 한정 조회 결과다. 다른 지역·SKU 전체 조사는 반복하지 않았다.

| 항목 | 결과 |
| --- | --- |
| Region / SKU | eastasia / Standard_B2pls_v2, Arm64, 2 vCPU / 4 GiB |
| SKU restrictions / quota | restrictions `[]`; regional cores 0/6, standardBpsv2Family 0/10 |
| Zone / allocation | catalogue zone 3; 현재 구성은 regional, zone 미지정; 실제 capacity 미확정 |
| 적용 지역 정책 | subscription scope Allowed Locations에 eastasia 포함; 정책 변경 없음 |
| OS image | Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040, Arm64 / V2 / plan null |
| 32 GiB OS disk | 최소 원본 disk 크기를 확인하지 못함; compatibility UNKNOWN |
| 이름 | 계획된 RG는 현재 없음; private 입력 초안의 ACR 이름 check-name 가용; 미래 예약/보장 아님 |
| 주소 | VNet 10.80.0.0/16, subnet 10.80.1.0/24 포함 관계 정상; Pod 10.42.0.0/16, Service 10.43.0.0/16 및 현재 Arch 전체 route와 중복 없음 |
| Cluster version | 두 기존 AMD64 Node Ready / v1.35.9+k3s1, 예정 ARM agent version과 일치 |

현재 호스트 route에는 LAN 192.168.45.0/24, Docker 172.17.0.0/16, libvirt 192.168.122.0/24와 tailnet peer /32가 포함된다. 다른 노드의 전체 route 및 향후 VPN route까지 검증한 것은 아니다. Policy/quota/SKU 사전 조건 통과가 실제 allocation capacity나 모든 리소스 write 권한을 입증하지 않는다.

32 GiB 판단을 위해 정확한 이미지 GET 및 Canonical의 Azure release metadata를 확인했다. GET의 `osDiskImage`에는 Linux 정보만 있고 minimum size가 없다. [공식 API schema](https://learn.microsoft.com/en-us/rest/api/compute/virtual-machine-images/get?view=rest-compute-2025-04-01#osdiskimage)도 minimum size를 제공하지 않는다. 공개 Azure simplestreams metadata는 2023년 갱신 자료였고, 현재 공개 Noble release 목록에는 해당 20260904 artifact 대응을 확보하지 못했다. 일반 Ubuntu 최소 설치 크기·다른 VHD·SKU `OSVhdSizeMB`(최대 지원 크기)를 대체 근거로 쓰지 않는다. 정확한 publisher build 확인 또는 exact VHD의 virtual-size 근거가 필요하다. 검증 경로는 [runbook 3.4](../runbooks/azure-arm64-deployment.md#34-32-gib-os-disk-검증-방법과-plan-진입-조건)를 따른다. VM/disk 생성이나 remote ARM validation은 실행하지 않았다.

## 3. 비용·상한·종료 절차

2026-10-13 03:36 KST, 공식 [Azure Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices)의 East Asia / USD / Consumption GET 결과다. VM은 Linux 일반 종량제만 선택하고 Spot/Windows/Cloud Services를 제외했다. 730시간은 비교용 한 달 가정이며 실제 청구를 보장하지 않는다.

| 예정 리소스 | 요율 | 730시간 기준 |
| --- | --- | --- |
| B2pls_v2 Linux VM | US$0.0464/hour | US$33.87 |
| 32 GiB E4 Standard SSD LRS | US$2.40/month | US$2.40 |
| Standard static IPv4 Public IP | US$0.005/hour | US$3.65 |
| ACR Basic | US$0.1666/day | US$5.07 |
| 합계 | 운영 시간과 저장분의 환산 | **US$44.99** |

추가: E4 disk operations US$0.002/10K, ACR 추가 저장분 US$0.10/GB-month·유료 Tasks 사용분, egress, backend storage/version/transactions, 세금·환율. Shared disk의 추가 mount meter는 단일 OS disk 기준에 합산하지 않았다. 기존 Backend 비용은 [runbook의 실제 조회·예시](../runbooks/azure-arm64-deployment.md#32-최소-backend-구성과-최초-bootstrap)를 참조한다. B-series는 CPU credits 기반이며 credit 소진 후 지속 CPU 성능이 제한된다. [Bpsv2 공식 설명](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/bpsv2-series).

Credit US$100은 사용자 확인 정보이며 이번 CLI로 실잔액·만료일을 입증한 것은 아니다. 기본 비용만 두 730시간 기간에 약 US$89.98이므로 추가 사용과 만료를 고려한 종료 계획이 필요하다.

**미승인 운영 제안:** 최초 실험은 최대 7일, 월 예산 US$50, 7일 지출 경계 US$12로 검토한다. 7일 기본 비용은 월 disk 비용을 시간 환산했을 때 약 US$10.35이며 Azure의 실제 meter 청구/추가 사용을 보장하지 않는다. 운영자·정리 담당자·시작/종료 날짜 및 소비 후 잔액을 확정해야 한다. 이는 자동 비용 차단이나 승인된 상한이 아니다. Azure budget/자동 종료 리소스는 만들지 않았다.

종료 담당자는 실험 종료 후 workload/artifact 철수 → 동일 remote State·private inputs를 사용한 destroy Plan 검토 → **해당 destroy Plan의 별도 사용자 승인** → testbed만 정리 → 잔존 청구 리소스 read-only 확인 순서로 진행한다. Backend는 보존한다. VM deallocate만으로 OS disk / Public IP / ACR 비용은 끝나지 않는다. 현재는 destroy Plan도 실행하지 않았다.

## 4. 보안·Bootstrap·입력 준비

Terraform 기능 코드는 변경하지 않았다. NSG는 priority 200 deny-all inbound이고 현재 `bootstrap_ssh_source_cidr=null`이라 SSH도 차단한다. 6443/10250/UDP8472/NodePort를 인터넷에 열지 않는다. NIC 연결은 NSG association 뒤 수행되도록 선언되어 있다. Standard Public IP는 명시적 egress에 사용되며 outbound Tailscale relay는 가능하지만 direct/relay 지연과 MTU는 실제 worker 구축 전 미검증이다. ACR은 public endpoint를 사용하되 admin/anonymous 접근 비활성이다; 이후 pull 권한 발급은 별도 단계다.

현재 발견한 기존 SSH 공개키는 Ed25519이며 root의 SSH 변수 검증은 RSA를 요구한다. 승인된 RSA 2048-bit 이상 공개키를 선택해야 하며 기존 개인키를 읽거나 새 키를 생성하지 않았다. Private `p1.tfvars.draft`는 0600으로 작성했고 실제 구독/후보/ACR 이름과 gate false, SSH null을 보관한다. RSA placeholder가 남아 있어 **실행 가능한 tfvars가 아니다**. `backend-init-readiness-20261013.json`도 Git 밖 0600 검증 기록이다. Backend 설정 파일은 변경하지 않았다.

VM에는 Terraform custom-data/provisioner로 Tailscale/k3s를 넣지 않는다. 따라서 최초 관리 접속은 승인된 현재 운영자 public /32의 임시 SSH 예외 또는 별도 관리 경로가 필요하다. 현재 선택·승인된 경로가 없다. 향후 ARM 노드의 Tailscale identity/tag 승인과 ACL의 server TCP6443 / 노드 TCP10250 / UDP8472 허용을 확인하고 tailnet 외 포트는 닫는다. 현재 기존 노드 정상 상태를 미래 worker의 ACL 승인으로 해석하지 않는다.

[`bootstrap/nodes/k3s-agent.yaml.example`](../../bootstrap/nodes/k3s-agent.yaml.example)을 unique node name / 실제 tailnet node IP / server `https://100.73.4.76:6443` / flannel `tailscale0` / root-only token-file로 준비한다. agent 설치는 ARM64 binary·checksum과 `v1.35.9+k3s1`을 확인한다. SSH 개인키는 운영자 Host에 유지하고 VM 복사/agent forwarding을 하지 않는다. Tailscale/k3s 인증정보는 별도 보호 파일 전달로 관리하며 Terraform State/Plan/cloud-init/argv/logs에 넣지 않는다. 실제 Tailscale 설치/가입, token 전달, k3s 설치/worker join은 수행하지 않았다.

## 5. 결론과 후속 승인

Backend 연결은 성공했고 빈 State가 존재한다. image minimum disk, RSA 공개키, 초기 접속·future node ACL, 승인된 비용 상한·종료 책임 및 credit 만료일이 남아 Plan은 실행하지 않았다. Private Plan 파일도 생성하지 않았다. `deployment_prerequisites_confirmed=false`를 유지하며 증거 없이 바꾸지 않는다.

모든 필수 조건이 해소되면 이미 승인된 범위에서 완성된 private `p1.tfvars`로 [runbook의 Plan 명령](../runbooks/azure-arm64-deployment.md#4-계획-검토와-승인된-배포)을 수행하고 결과는 Git 밖 0600 파일로 보관한다. 구성상 예정은 RG / VNet / subnet / NSG / subnet association / Public IP / NIC / VM(인라인 OS disk) / Basic ACR이며 **실제 Plan 결과가 아니다**. 최소 디스크 근거가 없는 현재 상태에서 gate를 우회하지 않는다.

실제 VM·네트워크·ACR 생성은 saved Plan 및 비용 검토 후 별도 apply 승인, OS/Tailscale/k3s bootstrap과 기존 Kubernetes worker join은 별도 승인, 비용 정리는 별도 destroy 승인이 필요하다. 이번 Provider·Storage firewall·RBAC·기존 클러스터 변경 및 Git commit/push는 없다. P1/P2 전체 완료로 표시하지 않는다.

수정 문서의 local 링크·파일 경로, Git 추적 대상의 민감한 파일·새 내용의 비밀 패턴을 점검했고 문제를 발견하지 않았다. 실제 Backend 계정·구독 설정값이 수정 파일에 포함되지 않았음을 확인했다. 기존 Observability values의 SHA-256과 provider lockfile은 변경 전과 동일하다. 이번 저장소 수정은 README, implementation plan, runbook, tfvars example 주석과 이 검증 기록이며 commit/push하지 않았다.
