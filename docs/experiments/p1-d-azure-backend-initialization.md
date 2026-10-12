# P1-D — Azure Remote Backend 초기화 및 ARM64 배포 gate

검증일: 2026-10-13 KST. 기준 commit: `00207e0`. 기존 [허용 지역 조사](p1-c-azure-allowed-regions.md), [Architecture v0.1](../architecture/architecture-v0.1.md), [배포 runbook](../runbooks/azure-arm64-deployment.md), [`infra/terraform/p1`](../../infra/terraform/p1)을 재사용했다. Backend 생성·Provider 등록은 이전 승인 단계의 결과이며 이번에는 재실행하지 않았다.

**최신 상태는 11절을 따른다.** 1~5절은 Backend 최초 초기화 당시, 6절은 `fbe6e1b` 이후 SSH/운영 조건 확인 당시, 7절은 disk 상속 계약 확인 당시, 8절은 Saved Plan 생성 당시, 9절은 최종 Plan 감사 당시, 10절은 Budget 생성 전 사전 점검 당시의 역사적 증거다. 과거 RSA-only·32 GiB 강제 입력·Budget 미생성·미승인 운영 제안은 최신 계약이 아니다.

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

## 6. Checkpoint 이후 Plan 조건 조사 — 2026-10-13

위 1~5절은 Backend 초기화 당시 기록이다. 이후 문서·예제 5개만 commit `fbe6e1bbb0d34f706ca5356cc742ece6bf74bda8`으로 만들고 GitHub main에 일반 push했다. 원격 SHA도 일치했다. SSH fetch는 passphrase/agent 부재로 실패하여 유효한 기존 gh 인증으로 명시적 HTTPS 주소를 사용했다. 저장된 remote·SSH 인증 설정을 바꾸지 않았고 force push하지 않았다. private backend/tfvars/State/Plan/SSH 키는 포함하지 않았다. 아래 후속 코드·문서 수정은 그 Checkpoint와 구분한다.

### SSH 계약 정정

고정 [AzureRM 4.64.0의 admin_ssh_key 계약](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/website/docs/r/linux_virtual_machine.html.markdown#arguments-reference)은 RSA 2048-bit 이상 및 Ed25519를 지원한다. [Azure 공식 SSH 문서](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/ssh-from-windows#supported-ssh-key-formats)도 Ed25519 256-bit 지원을 명시한다. 이전 RSA-only는 OpsLoop 변수 제한이었으며 Azure 지원 제약이 아니다. `variables.tf`를 두 형식 허용으로 수정하고 private key·certificate·지원하지 않는 key type·복수 key 행은 거부하도록 유지했다. 실제 key material 검증은 Provider와 `ssh-keygen`으로 확인한다. Terraform key 생성 리소스와 개인키 인자는 추가하지 않았다.

사용자가 지정한 Azure 전용 공개키를 `ssh-keygen -l -f`로 읽기 전용 확인했다: **Ed25519 / 256-bit**. 대응 개인키는 존재하고 mode 0600이며 내용을 읽지 않았다. RSA 신규 생성은 필요하지 않아 수행하지 않았다. 기존 Control Plane/GitHub용 키를 재사용하지 않는다. 별도 `/tmp`에서 Azure backend/provider 없이 Terraform console의 variable validation을 확인하여 Ed25519/RSA 공개키 입력은 허용, ECDSA·private key header·malformed payload·복수 key·멀티라인 comment는 거부했다. 콘솔은 validation error에도 exit 0일 수 있어 exit code만으로 판정하지 않았다.

### 디스크 — 추가 metadata와 64 GiB 대안

04:02 KST exact image 조회에서 Arm64/V2 및 features를 확인했다. `SecurityType=TrustedLaunchSupported`, accelerated networking 및 SCSI/NVMe 정보가 있지만 **OS disk minimum은 여전히 없다**. image feature와 SKU 제약을 혼동하지 않는다. 현재 B2pls_v2의 TrustedLaunchDisabled는 이전 조회에서 True이며 Terraform은 secure boot/vTPM/Trusted Launch를 요청하지 않는다. [고정 Provider의 disk_size_gb](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/website/docs/r/linux_virtual_machine.html.markdown#arguments-reference)는 image 크기를 변경하는 optional 값이며 특정 Marketplace version의 minimum을 보증하지 않는다.

공식 metadata/Provider 계약에서 exact minimum을 확보하지 못했다. 32 GiB compatibility UNKNOWN, 64 GiB도 source minimum을 확인하기 전에는 UNKNOWN이다. 64 GiB는 용량 여유를 늘리는 보수적 **검토 대안**이며 임의로 선택하거나 통과 처리하지 않았다. Provider의 크기 생략/image default 상속도 가능한 계약이지만 실제 source 크기·요금 tier 미확정으로 현재 입력을 바꾸지 않았다. Canonical exact build/VHD virtual-size의 근거 또는 minimum 확인이 필요하다.

64 GiB E6 Standard SSD LRS는 04:02 KST Retail GET에서 US$4.80/month, operations US$0.002/10K였다. 아래 VM/PIP/ACR 요율은 03:36 KST의 기존 선택 후보 조회를 재사용한다.

| OS Disk 검토안 | SSD 월 요율 | VM/PIP/ACR 포함 730시간 | 48시간 환산 예시 | 호환성 |
| --- | --- | --- | --- | --- |
| 32 GiB E4 LRS | US$2.40 | US$44.99 | 약 US$2.96 | 미검증 |
| 64 GiB E6 LRS | US$4.80 | US$47.39 | 약 US$3.12 | 미검증 |

48시간 예시는 VM 48시간 + Public IP 48시간 + ACR 2일 + SSD 월 요율의 48/730 환산이다. 디스크 operations·egress·Backend·세금 등은 별도이며 청구 보장이 아니다. 64 GiB 변경은 월 US$2.40의 증가이고 아직 선택하지 않았다.

### 확정된 운영 조건과 남은 Gate

사용자는 A(운영자 public /32의 임시 SSH 후 제거) 절차 준비, 별도 Azure용 Ed25519 공개키, 최대 48시간, 월 US$20 예산 **경고**, 사용자 본인의 종료·정리 책임을 확정했다. 시작/종료의 실제 시간은 배포 승인 시 기록한다. 이는 Apply/VM 생성/Bootstrap 승인이 아니다. 이전 7일/US$50/US$12 제안은 현재 조건으로 대체한다. 예산 경고는 강제 과금 차단이 아니며 Azure budget·자동 종료 리소스를 생성하지 않았다.

실험 종료 시 별도 승인된 VM deallocate와 잔여 유료 자원 확인, 필요시 승인된 destroy Plan/정리 절차를 수행하고 Backend를 보존한다. Deallocate 후에도 SSD/PIP/ACR의 730시간 잔존 비용은 32 GiB 약 US$11.12, 64 GiB 약 US$13.52로 환산된다. 따라서 deallocate만으로 비용 0이 되지 않는다. US$100 credit은 사용자 확인이고 실제 만료일은 계속 미확인이다.

private tfvars 초안은 선택된 공개키와 현재 운영자 /32만 반영하고 0600·gate false를 유지한다. 실제 private 값·SSH key data는 Git에 넣지 않는다. A 제거/검증/복구와 B 비교는 [runbook 5.1](../runbooks/azure-arm64-deployment.md#51-최초-관리-접속--a-선택-실행은-별도-승인)을 따른다. token은 Terraform/cloud-init/Run Command output에 전달하지 않고 tailnet SSH의 보호 파일/stdin 경로로 준비한다.

정확한 image minimum/선택 disk 호환성, 미래 Worker의 tailnet ACL 확인 및 credit 만료 확인은 남아 있다. 실제 Allocation Capacity는 catalogue/Plan으로 보장할 수 없다. disk 근거 없이 gate를 열지 않으며 **P1 Plan은 여전히 미실행**이다. 실제 Bootstrap/Worker 가입, VM·네트워크·ACR 생성, Backend/RBAC 변경은 수행하지 않았다.

## 7. 원본 이미지 OS disk 상속 — 2026-10-13

### 계약 검증과 최소 수정

[고정 AzureRM 4.64.0 문서](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/website/docs/r/linux_virtual_machine.html.markdown#arguments-reference)의 `os_disk.disk_size_gb`는 Optional이며 image 크기를 변경할 때 사용하는 값이다. [Terraform null 의미](https://developer.hashicorp.com/terraform/language/expressions/types#types)는 resource 인자의 생략이다. [동일 버전 schema](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/internal/services/compute/virtual_machine.go#L212-L218)는 Optional/Computed, 기본 크기 지정 없음, explicit size 0~4095 validation이다. 실제 설치된 Provider의 `terraform providers schema -json`에서도 Optional/Computed를 확인했다.

[OS disk expand 함수](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/internal/services/compute/virtual_machine.go#L264-L298)는 `FromImage`를 사용하고 `disk_size_gb > 0`일 때만 `DiskSizeGB` 포인터를 채운다. [Linux VM create 호출](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/internal/services/compute/linux_virtual_machine_resource.go#L579-L584)이 이 함수를 사용한다. 따라서 null은 API 크기 override를 생략해 원본 이미지 크기를 사용한다. 이는 문서·소스·설치 schema 검증이며 실제 VM provisioning/boot 결과가 아니다.

`os_disk_size_gb`의 default를 null, nullable을 true로 바꿨다. null-safe 조건식으로 `floor(null)`을 평가하지 않으며 explicit override는 기존 최소 30 GiB 정책과 Provider 상한 4095 GiB 안의 정수만 허용한다. 숫자 override는 별도로 image minimum을 확인해야 한다. `main.tf`는 기존 variable 전달을 유지하고 example 및 Git 밖 0600 private 초안만 null로 수정했다. Private Backend 내용·State·승인된 공개키·지역·SKU·/32 입력은 보존했다.

**임의의 32/64 GiB 크기를 강제하던 undersize 문제는 상속 방식으로 해소했다.** 32/64 GiB override의 호환성을 새로 증명하거나 원본 이미지 크기를 추정하지 않았다. Storage type은 StandardSSD_LRS지만 실제 capacity·과금 tier·정확한 합계는 미확정이며 Plan에서도 computed로 남을 수 있다.

### 선택 후보 읽기 전용 재확인

04:31 KST 관측을 기준으로 실제 활성 Azure for Students/Enabled와 private target subscription 일치를 확인했다. 세 승인 Provider와 Compute 모두 Registered다. 다른 후보/지역 대량 조사는 반복하지 않았다.

| 조건 | 이번 확인 결과 |
| --- | --- |
| eastasia / B2pls_v2 | Arm64, 2 vCPU / 4 GiB, Gen2 지원, accelerated networking True, restrictions `[]` |
| Quota | Regional 0/6, Bpsv2 Family 0/10, VM count 0/25000 |
| Policy | Subscription scope, enforcement Default, notScopes 없음; Allowed Locations의 eastasia 포함. REST GET으로 실제 definition의 Indexed/deny 조건도 확인. Policy 변경 없음 |
| Exact image | `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`, Arm64/V2, plan null; disk metadata는 Linux 정보만 반환 |
| ACR 이름 | private 입력 이름의 check-name 가용; 예약/생성하지 않음 |
| SSH / IP | 승인된 Azure 전용 Ed25519 256-bit 공개키와 private 입력 일치. 현재 운영자 IPv4가 임시 /32 및 기존 Backend firewall 허용 IP와 일치 |
| Backend | 기존 State Blob 181 bytes, lease unlocked; Entra data-plane 읽기 성공. backend.hcl 0600·내용 보존; 재초기화·State 변경 없음 |
| CIDR | 현재 Arch routes 및 알려진 LAN/Docker/libvirt/tailnet/Pod/Service CIDR과 VNet 중복 없음; worker subnet 포함 관계 정상. 다른 노드의 전체 route는 재검증하지 않음 |
| Bootstrap | /32 임시 SSH→보호된 대화형 Tailscale 가입→새 tailnet SSH 및 Control Plane 경로 확인→별도 승인된 Plan/Apply로 공인 SSH 예외 제거 절차 준비. k3s pin `v1.35.9+k3s1`; 이번 설치/가입 없음 |
| 미래 Worker ACL | **미확인**. Host Tailscale Running은 미래 identity/tag의 TCP22/6443/10250·UDP8472 승인을 입증하지 않음. 정책 파일/identity·허용 경로 확인을 사용자에게 요청 |

Policy definition 첫 CLI 조회는 인자 문제로 실패했고 assignment가 참조한 definition의 읽기 전용 REST GET으로 해결했다. RBAC 오류나 Policy 거부로 잘못 분류하지 않았다. Azure catalogue·Quota·Policy 통과는 실제 allocation capacity 보장이 아니다.

### 비용과 종료 조건

03:36/04:02 KST의 공식 Retail 조회를 재사용했다. 원본 크기가 미확정이므로 아래 디스크 항목을 32/64 GiB 요율로 대체하지 않는다.

| 항목 | 요율 | 730시간 환산 |
| --- | --- | --- |
| B2pls_v2 Linux | US$0.0464/hour | US$33.87 |
| Standard static IPv4 | US$0.005/hour | US$3.65 |
| ACR Basic | US$0.1666/day | US$5.07 |
| OS disk Standard SSD LRS | 원본 capacity·billing tier 미확정 | 미확정 |
| 알려진 항목 소계 | OS disk·추가 사용·Backend 제외 | **US$42.59** |

48시간 알려진 소계는 **US$2.80 + 미확정 OS disk 요금**이다. 32 GiB E4 US$2.40/month와 64 GiB E6 US$4.80/month는 이전 비교 예시로만 남기며 원본 크기의 하한/상한이 아니다. Deallocate 후에도 월 환산 **US$8.72 + OS disk + Backend/추가 사용**이 남는다. Disk operations·ACR 초과 저장/Tasks·egress·세금·환율은 별도다.

최대 48시간·월 US$20 경고·SleepyMiny 종료/정리 책임·Backend 보존은 사용자 확정 조건이다. 배포 시 실험 시작/종료 시간을 기록하고 종료 때 별도 승인된 VM deallocate와 잔여 유료 리소스 점검/정리를 수행한다. 경고는 강제 차단이 아니고 Azure budget/자동 종료는 생성하지 않았다. US$100 credit은 사용자 확인, 실제 잔액·만료일은 미확인이다. 지속 사용의 알려진 소계부터 US$20 경고 기준을 넘으므로 장기 운영 승인으로 해석하지 않는다.

### 검증 결과와 남은 Gate

`terraform fmt -check`와 `terraform validate` PASS. Sandbox의 Provider socket/CLI log 제약은 실제 호스트에서 같은 검증을 재실행해 해결했다. 기존 25개 Python tests 및 신규 disk 회귀 7개, **총 32개 PASS**. 신규 테스트는 실제 variable block을 독립 로컬 root에 넣어 omitted/null, 정상 정수, 작은 값·소수·Provider 상한 초과·잘못된 타입을 검증한다. Scalar CLI `-var`는 null 문자열 처리 때문에 테스트를 HCL tfvars 입력으로 수정했으며 실제 private 파일 경로도 동일하게 사용했다. 실제 private 초안은 backend/provider 없는 독립 console에서 전체 변수 validation 및 region/size/image/disk/gate 조건 PASS였다. 이 검증은 P1 Terraform Plan이 아니다.

미래 Worker identity/tag의 현재 ACL 근거가 없어 **`deployment_prerequisites_confirmed=false`, P1 Plan 미실행, Plan 파일 미생성**을 유지한다. disk 크기 상속 계약은 확인 완료이며 명시적 32 GiB 증명은 현재 방식의 진입 조건이 아니다. 실제 디스크 과금 tier·총비용과 credit 잔액/만료는 Apply 승인 전 미확정 위험으로 남긴다. Allocation capacity와 실제 네트워크/MTU/양방향 통신은 실제 배포 후 검증 대상이다.

ACL 근거가 확보되어 모든 필수 조건이 충족되면 이미 승인된 범위의 private Plan을 준비한다. Apply/VM·네트워크·ACR 생성, Bootstrap/기존 Kubernetes join 및 종료/정리 실행은 각각 별도 승인이 필요하다. 이번 Git commit/push, Azure 리소스 생성, Backend·RBAC·Policy·기존 Kubernetes/Tailscale 변경은 없다. 기존 Observability values와 Provider lockfile은 그대로 보존했다.

## 8. Tailscale 일반 접근 증거와 Saved Plan

사용자는 Admin Console → General access rules의 활성 `{"src":["*"],"dst":["*"],"ip":["*"]}`를 확인해 전달했다. 사용자 화면 확인 증거로 기록하며 에이전트의 직접 콘솔/스크린샷 검증과 구분한다. [Visual editor 공식 계약](https://tailscale.com/docs/reference/visual-editor)에 따르면 이 규칙은 Grants 형식의 일반 네트워크 접근이다. 동일 Tailnet에 가입·승인된 미래 Worker의 TCP22/6443/10250·UDP8472는 이 규칙상 허용된다. 전체 policy, 미래 가입/승인, 실제 트래픽 성공을 입증하지 않는다.

Local Tailscale status는 Running/Online이다. 현재 환경변수와 OpsLoop private 경로에서 사용 가능한 관리자 API 인증/policy export를 찾지 못해 전체 관리자 정책 조회는 수행하지 못했다. Legacy ACL/다른 Grants·tagOwners·Tailscale SSH 정책·Device Approval 설정은 미확인이다. 이번 P1은 기존 사용자 소유 장치의 browser 로그인과 일반 OpenSSH를 권장하며 새 tag 정의나 Tailscale SSH 활성화를 요구하지 않는다. 장기 서버 identity인 `tag:opsloop-azure-worker`는 tagOwners·권한·key expiry 및 별도 정책 변경 승인을 검토한다. 포트별 판단·identity 비교·실제 승인 단계는 [runbook 5.3](../runbooks/azure-arm64-deployment.md#53-현재-tailscale-일반-접근과-장치-가입-설계)을 따른다.

04:51 KST의 선택 후보 한정 재조회에서 Students/Enabled 및 대상 일치, Registered Providers, East Asia Allowed Locations/실제 deny 정의, B2pls_v2 Arm64/2 vCPU/4 GiB/Gen2·accelerated networking·restrictions `[]`, Regional 0/6·Family 0/10, exact image Arm64/V2/plan null, ACR 이름 가용을 확인했다. 현재 public IPv4는 private /32와 기존 Backend firewall 허용 IP에 일치하며 data-plane 읽기에서 State 181 bytes/lease unlocked다. 기존 같은 구독 Owner 권한 증거를 재사용했다. 현재 Host routes와 고정 VNet/Pod/Service 및 알려진 LAN/VPN 범위의 중복 없음도 확인했다.

원본 OS disk null 상속·승인된 전용 Ed25519·48시간/월 US$20 경고/사용자 종료 책임은 기존 검증을 재사용한다. Terraform HCL은 이번 단계에서 수정하지 않았으며 기존 32개 테스트/validate 결과를 재사용하고 fmt/diff check만 재확인했다. 사용자 확인 general rule에 따른 **Plan 준비 검토**와 아직 실행하지 않은 device approval·bootstrap·실제 connectivity 검증을 분리한다. 비용 tier·credit 잔액/만료·실제 capacity는 계속 미확정 위험이다.

### Saved Plan 실행 및 보관

Plan 필수 조건 검토 후 Git 밖 `~/.config/opsloop/private/p1.tfvars`를 새로 준비했다. 기존 `p1.tfvars.draft`와 `backend.hcl`은 내용·0600 권한을 보존했다. 실제 Plan 입력에서만 `deployment_prerequisites_confirmed=true`로 설정하고 root 기본값/public example/보존된 draft의 false는 유지했다. 이 값은 Plan 준비의 operator attestation이며 Apply 승인이 아니다. 전체 Tailnet 정책이나 아직 없는 Worker의 실제 연결을 검증 완료로 바꾼 것은 아니다.

2026-10-13 04:58 KST 다음 실제 P1 Plan을 실행했다. 기존 Backend·CLI/Entra 인증과 Provider 자동 등록 금지를 유지했으며 backend init/reconfigure/migrate-state는 반복하지 않았다.

```bash
umask 077
terraform -chdir=infra/terraform/p1 plan -input=false -no-color -lock-timeout=5m -detailed-exitcode -var-file="$HOME/.config/opsloop/private/p1.tfvars" -out="$HOME/.config/opsloop/private/p1-20261012T195805Z.tfplan"
```

**성공: 9 add / 0 change / 0 destroy.** `-detailed-exitcode`의 exit **2**는 변경이 있는 성공이다. Plan 파일명 timestamp는 UTC다. Saved Plan과 stdout/stderr log, `p1-20261012T195805Z.review.json`은 Git 밖 private directory(0700), 운영자 소유 파일 0600으로 보관했다. Raw Plan/JSON/tfvars/공개키/인증정보를 터미널이나 저장소에 복사하지 않고 필터링한 결과만 보고했다. Plan SHA-256: `faf73f1f4053064d252851dc0c01f4f6fcf801b237bc0db4c1b19a5e68a2a063`.

| Terraform 생성 항목 | 검토 결과 |
| --- | --- |
| Resource Group | P1 전용 RG, East Asia; Backend RG 제외 |
| VNet / Subnet | 10.80.0.0/16 / 10.80.1.0/24; subnet default outbound access false |
| NSG | priority 100: 운영자 현재 단일 /32 → TCP22만 Allow; priority 200: 모든 나머지 inbound Deny |
| Subnet–NSG association | Worker subnet 연결; NIC가 association에 의존 |
| Public IP | Standard/Static/IPv4; NIC의 명시적 egress |
| NIC | Accelerated networking true, IP forwarding false |
| Linux VM | B2pls_v2/eastasia, exact Canonical Arm64 image; 승인된 Ed25519 공개키 일치, password auth off, custom-data/user-data 없음 |
| ACR | Basic, public endpoint on, admin/anonymous pull off; node pull 권한은 후속 준비 |

위 표의 VNet/Subnet은 각각 별도 Terraform 리소스이고 **총 9개**다. OS disk는 VM inline 소유이며 별도 managed_disk 리소스를 중복 생성하지 않는다. OS disk의 `disk_size_gb`는 Saved Plan JSON에서도 unknown-until-apply, type은 StandardSSD_LRS다. 원본 크기 상속은 유지되지만 capacity·billing tier·정확한 총비용을 Plan이 해결하지 않았다.

Structured Plan 검토에서 관리 포트 6443/10250/8472/NodePort에 대한 public Allow 규칙은 없고, TCP22 예외의 Source는 private 입력 및 현재 운영자 공인 IPv4 /32와 정확히 일치했다. 단일 /32 외 SSH 허용·비밀번호 인증·Bootstrap secret·provisioner·예상 외 자원 생성 계획은 없다. NSG 선언/계획 검증이며 실제 effective rules와 OpenSSH·Tailscale·Kubernetes 통신 검증은 배포 후 수행한다. ACR public endpoint와 광범위 tailnet Allow All은 별도의 보안 위험으로 남긴다.

### Plan 이후 State와 승인 경계

Plan 이후 `terraform state list`의 관리 리소스는 **0개**, Entra Blob metadata GET은 **181 bytes / lease unlocked**였다. 실제 Worker/네트워크/ACR 생성은 없다. Backend/draft 내용·Observability values·Provider lockfile은 보존했고 Git commit/push, Tailscale policy, Azure Backend/RBAC/Policy 및 기존 클러스터 변경은 없다. Plan warning은 없었다.

비용은 7절의 기존 공식 요율을 재사용한다: VM/PIP/ACR **730시간 US$42.59**, **48시간 US$2.80**에 미확정 OS disk·추가 사용·Backend가 더해진다. Deallocate 뒤에도 PIP/ACR 월 환산 US$8.72 + disk/Backend가 남는다. US$20은 경고이며 자동 제한이 아니다. 실제 원본 disk 과금 tier, Students 잔액·만료 및 종료 시각을 Apply 승인 전 검토한다. B-series CPU credit, allocation capacity, device approval, 실제 direct/relay·MTU·양방향 통신과 node pull 인증은 후속 위험이다.

다음 승인은 **이 Saved Plan을 대상으로 한 VM/VNet/Subnet/NSG/NIC/PIP/ACR Apply**, 별도의 guest Bootstrap·Tailscale 가입/필요한 장치 승인·k3s Worker join, 연결 검증 후 임시 public SSH 예외 제거 Plan/Apply 및 실험 종료 deallocate/정리 실행이다. 현재는 어느 Apply도 승인되지 않았다. 구독/정책/Quota/IP/State/입력이나 Terraform HCL이 바뀌면 Saved Plan을 재검토·재생성한다. `.tfplan` apply는 추가 interactive 확인이 없으므로 명시적 승인 전 실행하지 않는다. P1/P2 전체 완료로 처리하지 않는다.

## 9. Saved Plan 최종 읽기 전용 감사 — Apply 미승인

2026-10-13 05:14 KST 실제 Saved Plan을 읽어 내부 JSON에서 필요한 조건만 검사했다. Plan/전체 JSON/private 입력/키는 외부에 출력하거나 저장소에 복사하지 않았다. Terraform 변경/새 Plan 생성은 없으며 이전 성공한 fmt/init/validate 및 Python 32개를 불필요하게 반복하지 않았다.

| 감사 항목 | 실제 결과 |
| --- | --- |
| Plan 무결성 | SHA256 `faf73f1f4053064d252851dc0c01f4f6fcf801b237bc0db4c1b19a5e68a2a063` 일치 |
| 현재 코드 일치 | Plan에 포함된 backend.tf / variables.tf / versions.tf / main.tf가 현재 파일과 byte-for-byte 일치 |
| Provider lock | Plan의 `.terraform.lock.hcl`과 현재 lockfile 일치 |
| Private 입력 일치 | 실제 변수 선언을 provider/backend 없는 console에서 검증; default를 포함한 전체 입력값과 Plan 변수 일치 |
| 대상 | 활성 Students / Enabled, private Backend·입력·Plan subscription 일치; eastasia/B2pls_v2 및 exact Arm64 image 유지 |
| 변경 수 | 지정된 9개 주소 모두 create; update/delete/예상 외 리소스 없음 |
| 네트워크 참조 | VM→NIC→subnet/PIP, subnet→NSG association, NIC→association dependency 확인 |
| 공개 inbound | 현재 Arch public IPv4와 Source /32 일치, TCP22만 Allow; 다른 inbound priority 200 Deny; 6443/10250/UDP8472 public Allow 없음 |
| VM 인증/Bootstrap | 승인된 Azure 전용 Ed25519 public key 일치; password off, custom-data/user-data/provisioner 없음; token/auth key/private key pattern 없음 |
| ACR | Basic public endpoint on; admin/anonymous auth off; public endpoint 자체는 잔여 위험 |
| OS disk | StandardSSD_LRS, 입력 null; disk_size_gb computed/unknown. 실제 capacity·과금 tier 미확정 |
| Private 보호 | Plan/backend.hcl/p1.tfvars/p1.tfvars.draft 운영자 소유 0600, parent 0700; Git 밖 유지 |
| Backend | Entra GET 성공, State metadata 181 bytes/lease unlocked 유지; 관리 리소스 0개는 8절 실제 조회를 재사용 |
| Gate/승인 | private Plan true, draft/root/example false 유지; Apply/Bootstrap/SSH 제거/종료 변경은 미실행·미승인 |

### 생성 예정 9개 리소스

| 주소 | 이름/계획 |
| --- | --- |
| `azurerm_resource_group.testbed` | `opsloop-p1-rg` |
| `azurerm_virtual_network.testbed` | `opsloop-p1-vnet` |
| `azurerm_subnet.worker` | `opsloop-p1-worker` |
| `azurerm_network_security_group.worker` | `opsloop-p1-nsg` |
| `azurerm_subnet_network_security_group_association.worker` | subnet/NSG 연결 |
| `azurerm_public_ip.worker_egress` | `opsloop-p1-egress-pip`, Standard/Static/IPv4 |
| `azurerm_network_interface.worker` | `opsloop-p1-nic` |
| `azurerm_linux_virtual_machine.worker` | `opsloop-p1-arm-worker`, inline `opsloop-p1-osdisk` 포함 |
| `azurerm_container_registry.testbed` | private `acr_name`, Basic |

### 당시 Budget 부재와 운영 위험 — 역사적 기록

Subscription Budgets REST GET 및 현재 유일한 RG인 `opsloop-state-rg`의 RG-scope GET이 모두 빈 목록이었다. 읽기 실패를 부재로 해석한 것이 아니다. **US$20 Azure Budget Alert는 현재 없다.** Billing account의 별도 알림은 조사하지 않았다. [최소 Budget 구성/승인과 48시간 절차](../runbooks/azure-arm64-deployment.md#61-48시간-운영-시계와-비용-통제)에 청구 통화 확인, 실제 수신자·기간 승인, Monthly Cost Budget/알림 초안과 종료/deallocate 검증을 준비했다. Budget/RBAC/자동 종료는 생성하지 않았다.

48시간/월 US$20 경고/SleepyMiny 정리 책임은 확정 조건이며, 실제 배포 시작·종료 시각은 아직 없다. Apply 직전 UTC부터 최대 48시간으로 보수적으로 기록하고 24/36/47시간 수동 점검을 준비한다. Deallocate는 compute만 중지하고 disk/PIP/ACR/Backend는 남는다. 별도 destroy 승인은 testbed artifacts 삭제·소유 리소스 종료이며 Backend/State 이력 보존과 구분한다.

비용은 기존 공식 조회 재사용: VM/PIP/ACR **48시간 US$2.80 / 730시간 US$42.59**, deallocate 뒤 PIP/ACR만 월 환산 US$8.72. 실제 OS disk tier·operations/egress/Backend·세금 등은 별도다. 학생 credit US$100은 사용자 확인, 실제 잔액·만료일은 계속 미확인이다. Capacity/CPU credits 소진 성능·future device approval·OpenSSH/양방향 kubelet/VXLAN·MTU·ACR pull은 후속 수용시험이며 Plan이 보장하지 않는다.

### 실행 준비와 Git Checkpoint

[Bootstrap 실행 카드](../runbooks/azure-arm64-deployment.md#54-승인-후-사용할-bootstrap-실행-카드)는 /32 SSH→독립 host-key 검증→Tailscale 대화형 가입→새 Tailnet OpenSSH→SSH 예외 제거 별도 Plan/Apply→새 public 연결 실패/Tailnet 성공→agent token-file→exact k3s 가입→3노드/양방향 Pod-DNS/1 MiB payload 순서다. 실제 실행하지 않았다. 미래 Run Command host fingerprint 조회, guest 설치/가입, cluster 테스트 생성/삭제는 별도 승인이다. 후속 SSH 제거의 scalar `-var='...=null'` 문서 명령을 실제 null HCL override로 수정했고, 현재 private 입력은 바꾸지 않았다.

기존 8개 변경을 보존하고 이번 추가 편집은 이미 변경된 두 문서에만 반영했다. 다음은 **권장 checkpoint 대상**이며 stage/commit/push하지 않았다.

| 파일 | 보존할 변경 |
| --- | --- |
| `README.md` | 현재 P1-D/Plan 상태 |
| `docs/experiments/p1-d-azure-backend-initialization.md` | Backend/SSH/disk/Plan/최종 감사 증거 |
| `docs/runbooks/azure-arm64-deployment.md` | 배포/Bootstrap/SSH 제거/48시간 비용 통제 |
| `docs/runbooks/p1-platform-preparation.md` | 최신 P1-D 상태 연결 |
| `infra/terraform/p1/main.tf` | image disk 상속 설명 |
| `infra/terraform/p1/variables.tf` | Ed25519 지원·null-safe disk 계약 |
| `infra/terraform/p1/terraform.tfvars.example` | 공개 예제·gate false·disk null |
| `tests/test_terraform_disk_contract.py` | 추가 7개 회귀 테스트 |

권장 메시지: `Prepare P1 ARM64 saved-plan audit and bootstrap safeguards`.
실제 backend.hcl/tfvars/State/Plan/private key/인증정보 및 `.terraform`은 checkpoint 대상이 아니다. 기존 Observability values와 lockfile은 보존한다. 이번 문서의 새 bash block은 syntax만 검사하며 실제 명령을 실행한 것으로 기록하지 않는다.

최종 로컬 검증: 기존 8개 경로만 변경, staged 변경 없음, `git diff --check` PASS, 변경 파일의 credential pattern 검사 PASS, tracked runtime/State/Plan/private-key 경로 0개, 로컬 문서 링크 PASS. Runbook bash 18개 block의 `bash -n` 및 Budget JSON parse PASS이며 명령은 실행하지 않았다. `.terraform/terraform.tfstate`의 backend metadata도 현재 private 설정의 account/container/key/subscription/tenant와 일치하고 Azure CLI/Entra 인증을 유지한다. access key/SAS/client secret은 설정하지 않았다. 추가 Plan 필드에서 admin username, Standard Static IPv4, CIDR/NSG/NIC 옵션, 모든 지역 및 빈 prior state를 확인했다. Probe BusyBox OCI index는 실제 읽기 전용 manifest 조회에서 AMD64/ARM64를 확인해 실행 카드에 digest를 고정했다. Observability values는 기존 SHA256과 일치한다.

### 명시적 Apply 승인 이후의 정확한 명령

다음은 **현재 실행 금지**다. 해당 해시의 Plan, 9개 리소스 생성 비용, Budget 미구성 해소/수동 감시 위험 수용, 미확정 disk 비용, 만료·cleanup 조건을 사용자에게 승인받은 뒤 사용한다. 당일 확인 결과가 오래되거나 subscription/IP/Quota/Policy/State/입력/코드가 바뀌면 새 Plan으로 다시 검토한다.

```bash
terraform -chdir=/home/sleepyminy/Projects/OpsLoop/infra/terraform/p1 apply -input=false -lock-timeout=5m /home/sleepyminy/.config/opsloop/private/p1-20261012T195805Z.tfplan
```

Saved Plan apply는 입력 var-file을 다시 받거나 추가 승인 prompt를 띄우지 않는다. 따라서 단순 명령 실행을 승인 대신 사용하지 않는다. 이번 실제 인프라/정책/Backend/클러스터 변경, Apply/destroy 및 Git commit/push는 없다. 현재 판단은 **Saved Plan 무결성·구성 감사 PASS, 실제 Apply는 승인/운영 통제 완료 전 대기**다.

## 10. 승인된 Budget 사전 점검과 Git Checkpoint 검증

**역사적 기록:** 아래는 Budget 생성 전 사전 점검 당시 상태다. 현재 Budget 구성은 11절을 따른다.

사용자는 Subscription 전체 Monthly US$20 상당 Budget, Actual 80%/100% 및 Forecasted 100% 알림, 운영자 수신 이메일과 기존 8개 파일의 commit/push를 승인했다. **수신 이메일은 공개 Git 자료에 기록하지 않는다.** 이 승인은 Apply·VM 생성·Bootstrap·SSH 규칙 제거·종료 변경 승인이 아니다.

2026-10-13 **05:36 KST** 실제 활성 Azure for Students/Enabled 및 기존 private Backend의 target subscription 일치를 먼저 확인했다. 현재 subscription의 Billing Property GET이 가리키는 Billing Profile만 조회했다. Microsoft Customer Agreement이며 **`properties.currency=KRW`**다. 프로필의 currency는 해당 프로필의 실제 청구 통화다. 계정 ID·청구 주소·수신자 등 raw 결과는 출력하거나 저장소에 복사하지 않았다. [Billing Property GET](https://learn.microsoft.com/en-us/rest/api/billing/billing-property/get?view=rest-billing-2024-04-01), [Billing Profile GET 계약](https://learn.microsoft.com/en-us/rest/api/billing/billing-profiles/get?view=rest-billing-2024-04-01).

Subscription Budgets List는 성공했고 `value=[]`로 동일 이름/목적의 기존 Budget이 없다. **USD가 아닌 경우 임의 환산액으로 생성하지 말라는 사용자 조건에 따라 PUT을 실행하지 않았다.** Amount 20을 KRW 20으로 잘못 생성하거나 scope를 USD 기준 billing-account 범위로 바꾸지 않았다. 추가 RBAC/Provider 등록, Backend/Policy/인프라 변경도 없다.

### 환산 근거와 남은 입력

[공식 Azure Pricing FAQ](https://azure.microsoft.com/en-us/pricing/)는 MCA 사용료를 USD로 계산한 뒤 현지 통화로 환산하며, 이전 달 마지막 영업일 전 두 영업일 동안 포착한 London closing spot rate를 다음 달 거래에 적용한다고 설명한다. 이는 실시간 임의 환율·Retail USD 단가와 구분한다. 이번 달 이 구독의 실제 KRW/USD 적용 환율 숫자는 확인하지 못했다.

예산 후보는 `20 × 확인된 KRW/USD 월 적용 환율` 원이며 반올림 방식·정확한 원화 금액·다음 달 환율 변화에 따른 갱신 조건을 운영자가 확정해야 한다. 현재는 원화 금액 미확정이라 Budget 생성을 보류한다. 실제 Budget 통화/기간/지원 여부는 Subscription Cost Management 화면 및 생성 API에서 후속 확인한다. MCA billing-account scope의 USD 평가를 사용할 수 있다는 일반 안내는 **승인된 subscription scope를 바꿀 근거가 아니다**.

| 설정 | 현재 상태 |
| --- | --- |
| Scope / reset | Subscription 전체 / Monthly 승인; 리소스 미생성 |
| Amount / currency | US$20 상당 목표; 실제 청구 KRW, 승인된 원화 금액 미확정 |
| Actual 80% / Actual 100% | 승인된 요청, 아직 생성/GET 검증 안 됨 |
| Forecasted 100% | 승인된 요청, 이번 구독의 실제 생성 API 지원은 미검증; 무단 생략/대체 안 함 |
| Recipient | 운영자 확정; 공개 Git에 값 저장 안 함; 실제 Budget GET 대조는 미실행 |
| Spending control | Budget은 알림이며 자동 지출 차단/VM 종료 장치가 아님 |

### Checkpoint 검증

이번 요청에 따라 Python **32개 PASS**, `terraform fmt -check` PASS, `terraform validate` PASS를 재확인했다. Sandbox validate의 Provider RPC 실패는 실제 호스트에서 동일 정적 검증을 재실행해 해결했다. Backend init/새 Plan/State 변경은 수행하지 않았다. Plan SHA256은 8절의 값과 일치하고 embedded Terraform root 4개 파일 및 lockfile도 현재 파일과 byte-for-byte 일치한다. Private 파일은 Git 밖 0600으로 보존했다.

최초 SSH fetch는 현재 agent/키 인증 문제로 실패했다. 실제 호스트의 기존 GitHub CLI keyring 인증은 유효해 HTTPS URL을 직접 지정하고 기존 credential helper를 일회성 사용하여 fetch에 성공했다. 저장된 origin SSH URL·key·인증 설정은 바꾸지 않았다. 원격 main은 기준 `fbe6e1b`와 같아 충돌 없이 checkpoint를 준비했다. 승인된 stage/commit/push 대상은 9절의 8개 프로젝트 파일이며 이메일·runtime/private artifact는 제외한다. 결과 SHA와 원격 일치/clean tree는 최종 실행 결과로 보고하며, 자기 참조 commit SHA를 문서에 미리 기재하지 않는다.

## 11. P1 독립 감사 후 현재 상태

기준 commit **`b80a747`**, 수정일 2026-10-13. 독립 감사에서 Critical/High 확정 결함은 없고 Medium 1건·Low 3건·운영 확인 조건 2건이 보고됐다. 아래 Budget·Spending Limit 상태는 **이번 수정 요청의 사용자 제공 현재 정보**이며 Azure 외부 재조회·설정 변경은 하지 않았다. 1~10절의 당시 관측값과 실행 이력은 보존한다.

| 항목 | 현재 상태와 근거 |
| --- | --- |
| Subscription Budget | `opsloop-monthly-budget`, 월 **27,000 KRW** |
| 알림 | **Actual 80% / Actual 100% / Forecasted 100% 설정 완료**; 실제 이메일 전달 시험은 이번에 수행하지 않음 |
| Spending Limit | **On**; Budget/48시간 타이머와 별도 통제 |
| 공개 정보 경계 | 이메일·Subscription ID·청구 계정 원본·private 입력을 공개 문서에 추가하지 않음 |
| OS disk | `os_disk_size_gb=null`, 원본 이미지 크기 상속, StandardSSD_LRS; **실제 크기·과금 계층 미확정** |
| Saved Plan | **9 create / 0 update / 0 delete 생성 완료**; 8~10절의 파일을 보존 |
| Apply | **아직 실행하지 않았으며 미승인**; 코드 수정/테스트는 Apply 권한이 아님 |
| Gate | private Saved Plan 준비 true, root/example false 유지; Terraform 코드·lockfile 변경 없음 |
| 통신 보안 | 일반 Tailscale Allow All 하의 비노드 UDP8472 접근 가능성이 남음; 실제 접근 통제 검증 전 완료 주장 금지 |
| 종료 | 수동 24/36/47시간·최대 48시간 종료 점검 유지·보강; 자동 VM Deallocate는 별도 승인 후 구성할 설계 초안뿐 |

조사 CLI 기본 32 GiB 가정은 제거하고 옵션 생략 시 이미지 크기 상속으로 맞췄다. 명시적 `--disk-gib`만 이미지 minimum과 비교하며 외부 CLI의 양의 정수 옵션·상태 문자열·exit 2는 유지한다. 상속 JSON의 `disk_gib=null` 처리와 예전 32 GiB 동작을 원하는 사용자의 명시 옵션 필요성은 [scripts 계약](../../scripts/README.md)에 기록했다. Namespace 수용시험은 GET 오류와 성공한 빈 결과를 구분하고 오류/기존 Namespace에서는 생성 없이 중단한다. 임시 kubectl stub 테스트만 사용하며 실제 클러스터에 적용하지 않는다.

[배포 runbook](../runbooks/azure-arm64-deployment.md)의 UDP8472 가입 전 검증·최소 권한/롤백 초안 및 48시간 수동 점검·자동 Deallocate 설계를 따른다. 기존 노드/서비스 ACL·UFW·Kubernetes·Tailscale·Observability·Backend와 Azure 리소스/예약 작업은 변경하지 않는다. 이번 작업은 commit/push 전 멈춘다.

### 이번 수정의 로컬 검증 결과

| 검증 | 결과 |
| --- | --- |
| Python baseline / 수정 후 | 수정 전 기존 **32개 PASS**, 수정 후 기존 32개 + 신규 8개 = **40개 PASS**, skip 없음 |
| 신규 회귀 | 조사 CLI 5개: 상속/큰 이미지/크기 metadata 부재·이미지 조회 실패·명시 override·기존 옵션·잘못된 입력; Namespace 3개: 존재/부재/GET 실패. 실제 runbook Bash 블록을 임시 kubectl stub으로 실행 |
| Terraform fmt / validate | 둘 다 PASS. sandbox validate는 Provider RPC handshake 제한으로 실패했으며 허용된 실제 호스트에서 동일 정적 validate로 PASS; init/backend 연결·외부 Azure 인증 없음 |
| Saved Plan SHA256 | `faf73f1f4053064d252851dc0c01f4f6fcf801b237bc0db4c1b19a5e68a2a063` 일치, Plan 내용 출력/추출 없음 |
| Saved Plan 코드 / lockfile | embedded backend.tf/main.tf/variables.tf/versions.tf와 현재 파일, embedded lockfile과 현재 lockfile 각각 byte-for-byte 일치 |
| 구문 / 문서 / diff | 모든 저장소 Python AST·문서 Bash 30개 블록 `bash -n`·변경 문서 local file link·`git diff --check` PASS. 문서의 인프라 명령은 실행하지 않음 |
| 노출 검사 | tracked/unignored Secret/State/Plan/private tfvars 경로 0개, 추가 내용의 credential 패턴·이메일 검사 PASS. private 파일의 UUID/이메일과 공개 파일 비교에서 일치 없음; private 값 출력 없음 |
| 보존 범위 | baseline 대비 infra/bootstrap 변경 없음. Observability values·provider lockfile 동일; Terraform 코드·Backend·기존 Kubernetes/Tailscale/ACL/UFW 변경 없음 |

남은 위험은 실제 disk 크기/과금 계층, credit 잔액·만료, Allow All 아래 비노드 UDP8472 접근 및 미래 Worker의 실제 통신/MTU·수용시험, 수동 종료 담당자의 실행 준비다. 자동 Deallocate는 미구성 초안이며 Budget 설정을 종료 보장으로 해석하지 않는다. **Apply는 계속 미승인·미실행**이고 이번 검증은 이를 해제하지 않는다. Azure/예약 작업 생성·수정·삭제, Terraform Apply/Destroy, 실제 클러스터 테스트, commit/push는 수행하지 않았다.
