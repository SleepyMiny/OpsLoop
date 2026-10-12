# Azure ARM64 deployment / destroy runbook

기준: [Architecture v0.1](../architecture/architecture-v0.1.md), [ADR-005](../adr/ADR-005-hybrid-multi-architecture-testbed.md), [P1 준비 절차](p1-platform-preparation.md), [P1-A inventory](../experiments/p1-a-environment-inventory.md), [기존 Azure 조사](../experiments/p1-c-azure-arm64-candidates.md), [최신 Backend 초기화 기록](../experiments/p1-d-azure-backend-initialization.md). 아래 VM 배포·bootstrap·삭제 명령은 **후속 운영 절차**이며 실행하지 않았다. 승인된 실제 Backend init과 연결 검증은 완료했다. 검증 통과는 VM 배포 승인·P1 완료를 대신하지 않는다.

## 1. 배포 계약과 현재 gate

Terraform root는 [`infra/terraform/p1`](../../infra/terraform/p1)다. Terraform **1.15.9**, AzureRM **4.64.0**과 기존 `.terraform.lock.hcl`을 유지한다. provider 등록은 `resource_provider_registrations="none"`, `resource_providers_to_register=[]`로 금지한다. version 업데이트 및 `init -upgrade`는 별도 검토한다.

| 항목 | 계약 |
| --- | --- |
| Architecture 선호 | `Standard_D2pls_v6`, 2 vCPU / 4 GiB; 현 family quota 0이며 Korea/Japan은 지역 정책 밖 |
| 이번 배포 후보 고정 | `eastasia` / `Standard_B2pls_v2` / 2 vCPU / 4 GiB; 다른 region/size는 새 검토 필요 |
| OS image | `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`; region에서 Arm64 / Gen2 / plan 없음; OS disk 크기는 image 상속 |
| Network | VNet `10.80.0.0/16`, worker subnet `10.80.1.0/24`; Terraform validation으로 고정 |
| Cluster | Pod `10.42.0.0/16`, Service `10.43.0.0/16`, DNS `10.43.0.10`; Tailscale underlay |
| Ingress / egress | Standard static NIC Public IP로 명시적 egress; NSG inbound deny-all. 필요시 운영자 public IPv4 `/32`의 임시 TCP 22만 허용 |
| VM / ACR | RSA/Ed25519 공개키 인증만, password off, 크기 null(image 상속), Standard SSD LRS; 정확한 과금 tier 미확정; ACR Basic 기본, admin/anonymous pull off |
| State | 별도 소유 remote backend; testbed destroy에 포함하지 않음 |
| Secret 경계 | SSH private key, Tailscale auth key, k3s token, ACR pull credential, kubeconfig, Azure client secret을 Terraform·cloud-init·Git에 넣지 않음 |

다음 10월 12일 조사 및 초기 10월 13일 정책 차단 기록은 역사적 증거다. Provider 미등록·Backend 미준비 설명은 이후 등록·구축과 이번 연결 검증으로 해소되었으며, 최신 상태는 아래 현재 후보와 [Backend 연결 기록](../experiments/p1-d-azure-backend-initialization.md)을 따른다.

2026-10-12 재조회 당시 `Microsoft.Compute`는 Registered, Network / Storage / ContainerRegistry는 NotRegistered였다. Korea Central regional total은 0/6, Dplsv6 family는 0/0으로 **선호 SKU quota gate가 실패**했다. 과거 `list-usage=[]` 결과를 현재 quota로 해석하지 않는다.

과거 Japan East의 B2pls_v2/B2ps_v2는 SKU object에서 Arm64 / V2 / accelerated networking 지원 / restrictions 빈 목록, regional cores 0/6, `standardBpsv2Family` 0/10 및 exact Ubuntu image를 확인했으나 이후 지역 정책 차단을 확인했다. 이는 역사적 증거이며 현재 배포 입력으로 사용하지 않는다. Terraform의 기본 D2pls_v6 값은 architecture 선호로 보존하고, 이번 실제 입력은 East Asia/B2pls_v2로 명시한다. B-series의 CPU credits/bursting은 D-series와 다르므로 Recovery Candidate의 지속 CPU 가용성을 동일하게 가정하지 않는다. 제한 없는 SKU/양수 quota도 capacity를 보장하지 않는다.

정확한 image 조회의 `osDiskImage`는 Linux 정보만 반환해 원본 disk size를 확인하지 못했다. 현재 `os_disk_size_gb=null`로 원본 이미지 크기를 상속한다. AzureRM 4.64.0의 Optional/Computed 계약과 size override 생략 경로를 확인했다. 32/64 GiB 호환성을 증명한 것은 아니며, 명시적 증설을 선택할 때만 image minimum 검증이 필요하다. 정확한 Standard SSD 과금 tier와 총비용은 아직 미확정이다.

통합 2단계에서는 미등록 세 provider를 재확인했고 Students 구독 spending limit은 On, 설정된 budget은 없었다. 구독 management-plane grant는 확인했지만 policy/deny/backend data-plane은 미검증이다. 구독에 연결된 MCA billing profile 조회에서도 학생 credit 잔액·만료를 확인하지 못했다. SKU/쿼터/가격 조사는 반복하지 않았다. 자세한 근거와 지속 운영 비용은 [2단계 결과](../experiments/p1-c-azure-arm64-candidates.md#통합-작업-2단계--미해결-배포-조건만-확인)에 기록했다. backend 실값·권한 및 최초 bootstrap 관리 경로가 준비되지 않아 현재 plan을 실행하지 않는다.

**2026-10-13 추가 차단 확인:** 활성 subscription Allowed Locations deny 정책은 `indonesiacentral`, `polandcentral`, `eastasia`, `norwayeast`, `malaysiawest`만 허용하며 Japan East VM은 예외가 아니다. 이전 SKU/쿼터 결과와 독립적인 차단이다. 정책을 변경/우회하지 않고 [최신 Phase A/B/C 기록](../experiments/p1-c-azure-arm64-candidates.md#2026-10-13-phase-abc-후속-확인)에 근거를 보존했다. 허용 지역의 ARM64 후보를 새로 검토하거나 적법한 정책 허용 경로를 확보하기 전 Japan East plan을 실행하거나 gate를 열지 않는다. Network/Storage/ContainerRegistry는 여전히 미등록이고 backend도 미준비다.

**현재 후보 확정 (2026-10-13):** East Asia/B2pls_v2의 regional 0/6, Bpsv2 family 0/10, restrictions 빈 목록, ARM64/Gen2/NIC 및 exact Ubuntu image를 재확인했다. East Asia는 subscription Allowed Locations 정책 안에 있다. 세 Provider Registered, 기존 Backend의 CLI/Entra 인증·container scope RBAC·공인 IP 접근·실제 init/validate는 통과했다. 초기화 전 없던 State Blob이 초기화 후 존재하며 default workspace의 관리 리소스는 0개다. 사용자가 US$100 credit을 확인했으며 만료일은 미확인이다. 후속 확인에서 최대 48시간, 월 US$20 경고, 사용자 본인 종료 책임과 임시 /32 SSH 절차 준비를 확정했다. image 크기 상속 계약은 확인했으며, 사용자 확인 general Allow All 규칙으로 Plan 조건부 접근 검토를 완료했다. Saved Plan은 9 create/0 update/0 delete로 성공했으며 private Plan 준비 gate는 true, Apply는 미승인이다. 전체 Tailnet 정책·미래 가입/연결·실제 disk 과금 tier·capacity는 미검증이다. 전용 Ed25519 공개키와 private 입력 준비는 후속 기록을 따른다. [상세 기록](../experiments/p1-d-azure-backend-initialization.md).

배포 전 운영자가 승인된 subscription UUID, region과 ARM SKU, regional total 및 해당 family 각각의 `limit-currentValue >= 2`, SKU Location/Zone restrictions, exact image/minimum OS disk, 실제 가격·잔여 credit·종료 시점, RG/network/VM/ACR 권한·정책, 세 노드 route/CIDR, backend/RBAC를 다시 확인한다. positive quota나 SKU catalogue가 실제 allocation capacity를 보장하지 않는다. Kubernetes 포트 6443 / 10250 / UDP 8472 / NodePort는 public NSG에 열지 않는다.

## 2. 로컬 재현 검증 — Azure 변경 없음

저장소 root에서 다음만 실행한다. `init -backend=false`는 provider download 및 lockfile 검증이며 backend에 접속하지 않는다. `validate`는 provider schema와 구성 정합성을 확인하며 실제 Azure quota/route/인증은 확인하지 않는다.

```bash
terraform -chdir=infra/terraform/p1 version
terraform -chdir=infra/terraform/p1 fmt -check -diff
terraform -chdir=infra/terraform/p1 init -backend=false -input=false -lockfile=readonly
terraform -chdir=infra/terraform/p1 validate
```

2026-10-12 위 fmt / init / validate 통과. sandbox init은 registry DNS 제한, sandbox validate는 provider plugin handshake 실패로 중단했고 승인된 외부 실행으로 같은 로컬 명령을 재검증했다. backend 연결, plan, apply, destroy 및 Azure 변경은 실행하지 않았다. state/plan/token/private key 내용을 출력하지 않았다.

## 3. Backend를 먼저 독립적으로 준비

### 3.1 Provider 등록 — 별도 승인 후 실행할 명령

현재 Compute 및 다음 세 Provider는 이미 Registered다. 아래는 이전 등록 준비 절차로 보존하며 **지금 다시 실행하지 않는다**. 향후 미등록 상태가 발견되면 해당 namespace의 별도 승인을 받은 뒤 진행한다. 등록 성공은 정책/쿼터/capacity 통과를 뜻하지 않는다. [공식 CLI](https://learn.microsoft.com/en-us/cli/azure/provider?view=azure-cli-latest#az-provider-register).

```bash
set -euo pipefail
OPSLOOP_SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
az account show --query '{name:name,state:state}' -o table
# 위 구독을 운영자가 확인한 후, 별도 등록 승인 범위에서만 진행한다.
for OPSLOOP_NAMESPACE in Microsoft.Network Microsoft.Storage Microsoft.ContainerRegistry; do
  az provider register --subscription "$OPSLOOP_SUBSCRIPTION_ID" --namespace "$OPSLOOP_NAMESPACE" --wait --only-show-errors -o none
  OPSLOOP_PROVIDER_STATE="$(az provider show --subscription "$OPSLOOP_SUBSCRIPTION_ID" --namespace "$OPSLOOP_NAMESPACE" --query registrationState -o tsv)"
  test "$OPSLOOP_PROVIDER_STATE" = Registered
  printf '%s: %s\n' "$OPSLOOP_NAMESPACE" "$OPSLOOP_PROVIDER_STATE"
done
```

Network는 VNet/subnet/NSG/NIC/PIP, Storage는 별도 state backend, ContainerRegistry는 기존 P1의 Basic ACR에 필요하다. 등록 담당자에게 subscription scope의 해당 `/register/action` 권한이 필요하다. 이미 Registered인 Compute를 다시 등록하거나 Terraform 자동 등록을 활성화하지 않는다.

### 3.2 최소 backend 구성과 최초 bootstrap

Backend는 이미 별도 승인으로 구축되었고 이번에 실제 Terraform 연결을 확인했다. 다음 생성 절차는 재현성 기록이며 기존 Backend에는 재실행하지 않는다. 실값은 Git 밖 private 설정·운영 기록에만 보관한다.

제안은 별도 `opsloop-state-rg`(eastasia), globally unique Storage account 하나, private `tfstate` container 하나다. `StorageV2`, `Standard_LRS`, Hot tier, HTTPS only, TLS ≥1.2, Azure 기본 암호화, blob anonymous access 및 Shared Key 금지로 구성한다. HNS/SFTP/NFS/Private Endpoint/NAT/PIP/VM은 backend 최소 구성에 포함하지 않는다. 공용 Blob endpoint는 활성화하되 default Deny + 운영자의 승인된 실제 public IPv4 한 개만 허용한다. Tailscale `100.x`는 public source IP가 아니며 동적 공인 IP 변경 시 승인된 규칙 갱신이 필요하다. 정책상 public endpoint가 금지되면 이 구성은 차단되고 Private Endpoint/접속 경로와 추가 비용을 별도 검토한다.

최초 bootstrap 담당자는 RG/Storage 관리 권한(예: 승인된 범위의 Contributor)과 별도의 role assignment 권한(Owner/User Access Administrator 또는 적절한 RBAC 역할)이 필요하다. **관리-plane Contributor/Owner가 Blob data-plane 권한을 자동 부여하지 않는다.** container는 `az storage container-rm create`로 관리-plane에서 만들고, 이후 Terraform 실행 주체에 container scope의 `Storage Blob Data Contributor`를 부여한다. 기본 Blob endpoint를 사용하므로 `lookup_blob_endpoint`를 켜지 않으며 backend용 account Reader는 필수가 아니다. [HashiCorp 최소 권한](https://developer.hashicorp.com/terraform/language/backend/azurerm#storage-account-required-role-assignments).

새 RG 생성에는 subscription 범위의 `Microsoft.Resources/subscriptions/resourceGroups/write`도 필요하다. 접근 범위를 넓혀 해결하지 않고 기존 grant와 승인된 역할 범위를 먼저 대조한다.

다음은 **backend 생성·네트워크/RBAC 변경의 별도 승인 후 실행할 준비 절차**다. placeholder를 실값으로 교체하고 global name 가용성과 Policy/RBAC를 확인한 뒤 실행한다. P1 Terraform에 backend를 넣어 순환 의존성을 만들지 않는다. bootstrap inventory는 private 운영 기록으로 보존하고 향후 별도 소유 state로 관리/import할 경우에도 P1 key와 분리한다.

```bash
# 앞 절에서 확인한 OPSLOOP_SUBSCRIPTION_ID를 사용한다. 현재 실행 금지.
OPSLOOP_STATE_RG=opsloop-state-rg
OPSLOOP_STATE_ACCOUNT=REPLACE_GLOBAL_UNIQUE_LOWERCASE_NAME
OPSLOOP_OPERATOR_PUBLIC_IPV4=REPLACE_APPROVED_PUBLIC_IPV4
OPSLOOP_OPERATOR_OBJECT_ID=REPLACE_APPROVED_ENTRA_USER_OBJECT_ID
az group create --subscription "$OPSLOOP_SUBSCRIPTION_ID" --name "$OPSLOOP_STATE_RG" --location eastasia -o none
az storage account create --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group "$OPSLOOP_STATE_RG" --name "$OPSLOOP_STATE_ACCOUNT" --location eastasia --kind StorageV2 --sku Standard_LRS --access-tier Hot --https-only true --min-tls-version TLS1_2 --allow-blob-public-access false --allow-shared-key-access false --public-network-access Enabled --default-action Deny --bypass None -o none
az storage account network-rule add --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group "$OPSLOOP_STATE_RG" --account-name "$OPSLOOP_STATE_ACCOUNT" --ip-address "$OPSLOOP_OPERATOR_PUBLIC_IPV4" -o none
az storage account blob-service-properties update --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group "$OPSLOOP_STATE_RG" --account-name "$OPSLOOP_STATE_ACCOUNT" --enable-versioning true --enable-delete-retention true --delete-retention-days 7 --enable-container-delete-retention true --container-delete-retention-days 7 -o none
az storage container-rm create --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group "$OPSLOOP_STATE_RG" --storage-account "$OPSLOOP_STATE_ACCOUNT" --name tfstate --public-access off -o none
OPSLOOP_STATE_SCOPE="/subscriptions/$OPSLOOP_SUBSCRIPTION_ID/resourceGroups/$OPSLOOP_STATE_RG/providers/Microsoft.Storage/storageAccounts/$OPSLOOP_STATE_ACCOUNT/blobServices/default/containers/tfstate"
az role assignment create --subscription "$OPSLOOP_SUBSCRIPTION_ID" --assignee-object-id "$OPSLOOP_OPERATOR_OBJECT_ID" --assignee-principal-type User --role 'Storage Blob Data Contributor' --scope "$OPSLOOP_STATE_SCOPE" -o none
```

CLI User용 제안이며 다른 identity에 임의로 권한을 부여하지 않는다. RBAC/firewall 반영 후 `az storage container show --account-name "$OPSLOOP_STATE_ACCOUNT" --name tfstate --auth-mode login --query name -o tsv`로 data-plane 도달을 확인한다. account key/SAS/token 취득과 `--debug`는 필요 없다. versioning/7일 soft-delete에 더해 이전 version 보관 용량·cleanup 담당자·state 복구 절차를 정한다. soft-delete가 account/RG 삭제까지 보호한다고 간주하지 않는다. [Storage account CLI](https://learn.microsoft.com/en-us/cli/azure/storage/account?view=azure-cli-latest#az-storage-account-create), [container 관리-plane CLI](https://learn.microsoft.com/en-us/cli/azure/storage/container-rm?view=azure-cli-latest#az-storage-container-rm-create), [versioning/retention CLI](https://learn.microsoft.com/en-us/cli/azure/storage/account/blob-service-properties?view=azure-cli-latest#az-storage-account-blob-service-properties-update).

**backend 비용:** 2026-10-13 02:33 KST의 공식 Retail GET에서 East Asia/Consumption/USD/Hot LRS의 소용량 tier는 Data Stored **US$0.024/GB-month**, Write 및 List/Create Container **US$0.05/10K**, Read/Other **US$0.004/10K**였다. 예를 들어 version/soft-delete분을 포함한 과금 용량 1 GB, 월 1,000 Write + 1,000 Read이면 `0.024 + 0.005 + 0.0004 = US$0.0294/월`이다. 실측 용량·상한·청구 보장이 아니며 List/lease 등 추가 작업, egress, 세금·환율을 제외한다. account/RG/RBAC 자체에 VM 같은 고정 시간 요금은 없고, 이전 version 증가는 추가 용량 비용이다. [Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices).

### 3.3 private backend 설정과 초기 접속

[`backend.tf`](../../infra/terraform/p1/backend.tf)는 partial `azurerm` backend를 선언한다. 선언만으로 storage가 생성되지는 않는다. **별도 backend 담당자**가 별도 root/state로 Storage account/container, 접근 제한, 암호화, blob versioning/soft delete·보존·복구 절차를 준비한다. P1 resource group과 분리하고 state key `opsloop/p1/testbed.tfstate`를 backend bootstrap state에 재사용하지 않는다. 표준 Blob endpoint를 전제로 한다.

인증은 기존 운영자 Azure CLI session과 Entra data-plane 인증(`use_cli=true`, `use_azuread_auth=true`)이다. container에 최소 `Storage Blob Data Contributor`를 부여하고 네트워크 접근을 확인한다. access key lookup, SAS 및 client secret은 사용하지 않는다. Azure Blob lease 기반 잠금을 사용하며 `-lock=false`로 우회하지 않는다. [HashiCorp azurerm backend](https://developer.hashicorp.com/terraform/language/backend/azurerm).

설정과 산출물은 source tree 밖 운영자 전용 디렉터리(0700), 파일 0600으로 보관한다. 예를 들어 승인된 운영자 경로를 `OPSLOOP_PRIVATE_DIR`로 지정한다. [`backend.hcl.example`](../../infra/terraform/p1/backend.hcl.example)에는 account/container/key/tenant/subscription 식별자만, [`terraform.tfvars.example`](../../infra/terraform/p1/terraform.tfvars.example)에는 nonsecret 입력과 SSH **공개키**만 있다. 운영자는 placeholder를 교체하고 공개키 RSA 2048-bit 이상 또는 Ed25519 256-bit 여부를 `ssh-keygen -lf /path/to/approved-public-key.pub`로 확인한다. backend와 provider의 target subscription을 각각 확인하며 credential/env 전체 덤프는 하지 않는다.

새 배포이며 기존 local/remote state가 없음을 확인한 이후에만 후속 운영자가 다음을 실행한다. backend 인증·잠금/복구 준비가 실패하면 중단한다.

```bash
umask 077
export OPSLOOP_PRIVATE_DIR=/path/to/operator-private-opsloop
terraform -chdir=infra/terraform/p1 init -input=false -lockfile=readonly -backend-config="$OPSLOOP_PRIVATE_DIR/backend.hcl"
terraform -chdir=infra/terraform/p1 workspace show
```

`default` workspace와 독립 P1 key를 사용한다. backend identity/key를 바꾸는 `-reconfigure`나 기존 state를 이동하는 `-migrate-state`는 신규 배포 명령에 추가하지 않는다. 기존 state가 발견되면 writer를 중단하고 보호된 backup·lineage/serial·리소스 소유권·복구 계획을 별도로 검토한다. `.terraform/`도 backend metadata를 포함하므로 source와 분리된 runtime 자료로 취급하고 Git에 올리지 않는다.

### 3.4 32 GiB OS disk 검증 방법과 plan 진입 조건

현재 exact URN은 `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`다. 읽기 전용 `az vm image show --location eastasia --urn ...`는 Arm64/Gen2를 확인했지만 응답의 `osDiskImage`에 disk size가 없다. 같은 조회를 반복해도 32 GiB 호환성 증거가 되지 않는다. 현재 **image capacity unknown / 32·64 GiB override compatibility unknown**이다.

**현재 선택은 `os_disk_size_gb=null`이다.** [Terraform null 계약](https://developer.hashicorp.com/terraform/language/expressions/types#types)에 따라 resource 인자에서 null은 생략과 같다. [고정 Provider 문서](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/website/docs/r/linux_virtual_machine.html.markdown#arguments-reference)는 image 크기를 변경할 때만 `disk_size_gb`를 지정하는 Optional 인자라고 정의한다. [AzureRM 4.64.0 소스](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/internal/services/compute/virtual_machine.go#L212-L218)의 Optional/Computed schema와 [expand 함수](https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/internal/services/compute/virtual_machine.go#L264-L298)를 확인했다: `FromImage`를 사용하고 크기가 양수일 때만 `DiskSizeGB` 포인터를 설정한다. null은 크기 override를 보내지 않으므로 원본 이미지 크기를 사용한다. VM을 실제 생성하여 시험한 것은 아니다.

따라서 임의의 32/64 GiB를 강제하던 undersize 위험을 제거한다. 실제 용량과 Standard SSD LRS의 정확한 과금 tier는 아직 알 수 없으며, Plan에서도 apply 후 computed 값으로 남을 수 있다. 아래 minimum 검증 절차는 **향후 명시적 override를 선택하는 경우**에 적용한다.

검증 경로는 다음과 같이 구분한다.

1. **우선 읽기 전용 근거:** Canonical의 해당 Azure Marketplace exact build에 대한 원본 가상 OS disk 크기나 지원 확인을 확보한다. 정확히 같은 artifact임을 publisher/build/checksum으로 입증할 수 있는 공식 VHD가 있다면 `qemu-img info --output=json /path/to/verified-exact-image.vhd`의 `virtual-size <= 34359738368`(32 × 1024³ bytes)인지 확인한다. 압축 파일 크기, root filesystem 사용량, 다른 Ubuntu cloud image 또는 일반적인 30 GiB 안내는 대체 증거가 아니다. 현재 그 artifact/확인 자료는 확보하지 못했다.
2. **provider preflight:** provider 등록 및 승인된 기존 RG/권한이 준비된 후, 같은 URN/ARM64 size/`storageProfile.osDisk.diskSizeGB=32`를 가진 검토된 ARM template로 `az deployment group validate --validation-level Provider`를 수행하는 방법이 있다. 별도 배포 프로젝트로 전환하지 않고 검증용 template만 사용한다. CLI 지원 여부와 Azure Policy/RBAC를 먼저 확인한다. provider의 disk-too-small 오류는 FAIL 근거지만, 성공 결과에 disk 호환성 검사가 명시되지 않으면 정확한 minimum 확인으로 간주하지 않는다. Terraform validate/plan이나 ARM의 Template-only 검증은 source image 크기 검증을 대신하지 않는다. 이번에는 template 생성이나 remote validation도 실행하지 않았다. [Provider validation CLI](https://learn.microsoft.com/en-us/cli/azure/deployment/group?view=azure-cli-latest#az-deployment-group-validate).
3. **실제 생성 검증:** 위 경로가 충분한 근거를 제공하지 못하면 비용·리소스 생성의 별도 승인 후 실제 OS disk provisioning 및 VM boot로 확인해야 한다. 이는 현재 금지 범위이며, 지금 호환성 PASS로 기록하지 않는다. 64 GiB로 임의 증설해도 image minimum 증거가 생기지 않고 비용 tier가 바뀐다.

Backend init은 승인된 기존 account/container의 인증·접근·state 충돌 여부를 먼저 확인하고 수행한다. VM 입력·디스크·bootstrap gate와 구분하며 현재 init은 완료했다. **VM Plan**에 진입하기 전 다음을 모두 기록해야 한다. 부족한 항목은 승인이 있어도 검증 완료로 바꾸지 않는다.

- 세 provider Registered; 승인된 구독/관리-plane 권한/전체 적용 Policy와 ACR global name 확인.
- backend account/container, container Blob Data Contributor 및 firewall 접근, lease 잠금·복구/소유권 준비; 다른 state와 key 충돌 없음.
- private tfvars의 East Asia/B2pls_v2/exact image/승인된 전용 Ed25519 공개키, disk null과 image 상속 계약 확인; 명시적 disk override 시 별도의 minimum 근거 필요. 실제 SSH 개인키·토큰은 포함하지 않음.
- Azure for Students **US$100 credit은 사용자 확인, 만료일은 미확인**이다. 소비 이후 실잔액과 비용 상한·종료 시점/cleanup 책임을 확인한다. spending limit On과 budget 없음은 충분한 운영 예산을 증명하지 않는다.
- 최초 관리 경로 승인(필요시 운영자 public /32 TCP 22만), 보호된 Tailscale 등록 및 k3s token-file 전달 절차; join은 `100.73.4.76:6443`, version은 `v1.35.9+k3s1`. Kubernetes 관리 포트는 인터넷에 열지 않음.
- plan 직전 변화 가능한 quota/restrictions/policy/provider 상태만 재확인; 실제 allocation capacity는 apply 전 확정할 수 없다고 기록.

**검증 재사용:** 기존 25개 Python 테스트와 신규 disk 회귀 7개, 총 32개 및 `fmt -check` / `validate`는 통과했다. 실제 Azure Backend `init` / `workspace show` / `state list`는 이전 성공 기록을 재사용하고 재초기화하지 않았다. State metadata의 읽기 접근은 다시 확인했다. 사용자가 만든 Observability values는 변경하지 않았다. 이후 사용자가 일반 접근 Allow All 규칙을 확인해 전달했으며 Plan 준비와 실제 Bootstrap 검증을 구분한다. 최신 Saved Plan 결과는 [P1-D 8절](../experiments/p1-d-azure-backend-initialization.md#8-tailscale-일반-접근-증거와-saved-plan)을 따른다. 실제 worker 생성과 Tailscale/k3s bootstrap은 별도 후속 승인이다.

원본 disk 크기를 상속하는 현재 구성은 정확한 disk 과금 tier가 미확정이다. 03:36 KST Retail 기준 VM/PIP/ACR만 730시간 US$42.59, 48시간 US$2.80이며 **OS disk + 추가 사용 + Backend는 별도**다. 기존 32/64 GiB의 US$44.99/47.39 총액은 해당 크기를 가정한 예시이며 현재 확정 견적이 아니다. Deallocate 후에도 월 환산 PIP/ACR US$8.72 + disk/Backend 비용이 남는다. 최대 48시간·월 US$20 경고·SleepyMiny 정리 책임과 Backend 보존을 지키며 actual disk 과금/credit 잔액·만료는 Apply 승인 전 위험 검토에 포함한다.

## 4. 계획 검토와 승인된 배포

gate를 모두 충족한 후 운영자가 private tfvars의 `deployment_prerequisites_confirmed=true`를 설정한다. 이는 확인 기록이며 배포 권한을 부여하지 않는다. `p1.tfvars`의 placeholder subscription/ACR 이름/RSA/Ed25519 공개키를 승인된 실값으로 교체하고 exact image를 유지한다. public SSH가 필요하면 승인된 운영자 현재 public IPv4 `/32`만 넣는다. null이면 SSH 22도 닫힌다. 새 VM에는 Terraform이 Tailscale을 설치하지 않으므로 최초 bootstrap에는 이 임시 예외 또는 별도 승인된 관리 경로가 있어야 한다. tailnet 외 Kubernetes 포트를 추가하는 plan은 거부한다.

다음 명령은 현재 검토 후보 **East Asia/B2pls_v2**를 명시한다. Plan 필수 조건을 확인한 후에만 실행한다. private 입력의 region/size를 같은 pair로 유지한다. `p1.tfvars.draft`는 보존된 gate false 초안이며, 완료한 검토 결과를 반영한 별도 private `p1.tfvars`를 사용한다. Korea/Japan으로 되돌리거나 zone을 선택하면 정책/SKU 계약을 새로 검토한다. Provider는 이미 Registered이며 Terraform 자동 등록은 금지 상태를 유지한다.

```bash
terraform -chdir=infra/terraform/p1 plan -input=false -lock-timeout=5m -var-file="$OPSLOOP_PRIVATE_DIR/p1.tfvars" -var='location=eastasia' -var='vm_size=Standard_B2pls_v2' -out="$OPSLOOP_PRIVATE_DIR/p1.tfplan"
terraform -chdir=infra/terraform/p1 show -no-color "$OPSLOOP_PRIVATE_DIR/p1.tfplan"
```

plan은 보호된 운영자 터미널에서만 검토한다. 현재 root는 token/credential 리소스가 없지만 이후 computed 민감 attribute가 들어갈 수 있으므로 state/plan은 민감 자료로 취급한다. 터미널·CI 로그에 state, plan JSON 또는 credentials를 복사하지 않는다. `show`의 텍스트도 공개 evidence에 붙이지 않는다. RG/VNet/subnet/NSG/PIP/NIC/VM inline disk/ACR만 생성하는지, 등록/RBAC/backend 변경이 없는지, resource ownership과 가격을 검토하고 **해당 saved plan에 대한 사용자 승인 후** 아래 명령을 실행한다.

```bash
terraform -chdir=infra/terraform/p1 apply -input=false -lock-timeout=5m "$OPSLOOP_PRIVATE_DIR/p1.tfplan"
```

saved plan apply는 추가 interactive prompt가 없으므로 승인을 먼저 기록한다. 설정·subscription·state·비용/정책이 바뀌면 기존 plan을 재사용하지 않고 새 계획을 검토한다. apply 후 Azure provisioning status, NSG effective rules, VM outbound HTTPS/ACR 연결을 승인된 관리 경로에서 확인한다. SSH private key를 파일 인수로만 지정하고 host key 검증을 유지한다. Terraform 성공은 node bootstrap 성공을 뜻하지 않는다.

## 5. Bootstrap과 세 노드 검증

### 5.1 최초 관리 접속 — A 선택, 실행은 별도 승인

2026-10-13 후속 확인에서 사용자는 **A 절차 준비**와 전용 Ed25519 공개키 사용을 선택했다. NSG의 default deny-all과 Terraform의 SSH null 기본값은 보존한다. 실제 예외 적용·VM 생성·Bootstrap은 수행하지 않았다.

| 항목 | A: 운영자 public /32의 임시 SSH | B: public SSH 없는 Azure Run Command |
| --- | --- | --- |
| 접근·보안 | 키 인증과 단일 /32, 종료 조건 및 제거 검증 필요 | public SSH 비노출; Azure 관리-plane root 실행 권한·VM Agent에 의존 |
| 복잡도 | SSH로 대화형 로그인·보호 파일 전달 가능; NSG 제거 Plan 추가 | 비대화형 스크립트 실행 후 별도 Tailscale browser 로그인 설계 필요 |
| 민감정보 | SSH 개인키는 Host만; token은 SSH stdin/보호 파일, argv/log 제외 | script/parameters/output은 Azure 및 guest에 기록될 수 있어 token 전달 금지; 로그인 URL도 민감하게 취급 |
| 비용·자원 | 현재 구성의 VM/PIP 비용 안에서 사용; 별도 Bastion/Key Vault 불필요 | VM/PIP 유지; VM Agent extension 운영 필요, 별도 Bastion/Key Vault/로그 Blob을 자동 추가하지 않음; Run Command 별도 단가 미확인 |
| 장애 대응 | Tailnet 접속이 입증되기 전 예외를 제거하지 않음 | Agent/HTTPS egress 장애 시 경로 사용 불가; 공개 SSH 대체도 별도 승인 |

단기 수동 실험에서는 **A를 권장**한다. 기존 SSH·tailnet 운영 방식으로 비밀을 Azure 명령 로그에 전달하지 않으면서 가입·복구를 확인할 수 있다. B는 [Ubuntu 24.04 ARM64 지원과 비대화형 제한](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/run-command)이 확인된 대안이다. token/auth key를 Terraform/cloud-init 또는 Run Command scripts/일반 parameters에 넣지 않는다. Managed Run Command의 ProtectedParameter는 별도 보호 수단이지만 guest 실행·출력 관리가 필요하며 현재 구성에는 추가하지 않았다. B의 Tailscale 로그인 URL은 auth key와 다르지만 민감한 가입 자료이며 공용 로그에 출력할 수 없다. 보호 경로가 승인되지 않으면 B 가입도 준비 완료로 처리하지 않는다.

**A의 실행 및 제거 절차 — VM 배포 이후 별도 승인 범위:**

1. 운영자는 실행 직전 Arch의 public IPv4 및 승인된 /32를 확인한다. 최초 Plan에서 `AllowOperatorBootstrapSSH`가 그 주소의 TCP22만 허용하고 priority 200 deny-all·관리 포트 비노출이 유지되는지 검토한다. 사설/tailnet IP나 넓은 CIDR로 대체하지 않는다.
2. saved Plan Apply는 별도 승인 후 수행한다. SSH host fingerprint를 Azure 관리 경로 등 독립 근거로 확인하고 `StrictHostKeyChecking=yes`, `IdentitiesOnly=yes`, `ForwardAgent=no`로 접속한다. `ssh -i`의 개인키는 Host 파일 경로만 사용한다.
3. 서명/출처를 확인한 Tailscale 설치 후 운영자 브라우저의 대화형 로그인 및 device/tag/ACL 승인을 수행한다. auth key를 명령 인수로 넣지 않는다. k3s token은 tailnet SSH가 검증된 뒤 암호화된 stdin으로 root-only 0600 token-file에 전달하며 stdout·shell history·`set -x`를 사용하지 않는다. k3s 설치/가입은 별도 승인이다.
4. Host→Worker tailnet IP의 **새 SSH 세션**이 인증되는지 확인한다. `tailscale ping`으로 direct/relay 경로를 기록하고 Worker→Control Plane 100.73.4.76:6443 TCP/TLS 경로와 hostname/host key를 확인한다. 이것만으로 미래 노드의 양방향 kubelet/VXLAN ACL을 통과로 기록하지 않는다.
5. private 입력을 `bootstrap_ssh_source_cidr=null`로 바꾸고 같은 backend/state로 다음 제거 Plan을 검토한다. gate의 기존 확인 조건은 유지하며 VM 재생성·추가 자원이 나타나면 중단한다. 기대 변경은 NSG에서 SSH 예외 제거뿐이다. 해당 saved Plan의 Apply 승인은 별도다.

```bash
# 지금 실행하지 않음: 실제 VM 구축·tailnet 검증 이후의 제거 Plan이다.
umask 077
printf 'bootstrap_ssh_source_cidr = null\n' > "$OPSLOOP_PRIVATE_DIR/close-public-ssh.tfvars"
chmod 600 "$OPSLOOP_PRIVATE_DIR/close-public-ssh.tfvars"
terraform -chdir=infra/terraform/p1 plan -input=false -lock-timeout=5m -var-file="$OPSLOOP_PRIVATE_DIR/p1.tfvars" -var-file="$OPSLOOP_PRIVATE_DIR/close-public-ssh.tfvars" -out="$OPSLOOP_PRIVATE_DIR/p1-close-public-ssh.tfplan"
```

Scalar CLI `-var='bootstrap_ssh_source_cidr=null'`은 문자열 `"null"`로 해석될 수 있으므로 사용하지 않는다. HCL var-file의 실제 null로 override한다. 제거 Apply 이후 운영자가 canonical private `p1.tfvars`도 null로 갱신해야 다음 Plan이 예외를 다시 열지 않는다. 그 전까지 모든 후속 Plan에 제거 override를 마지막 var-file로 사용한다. 최초 9-create Saved Plan은 이미 소비된 계획이며 제거 이후 재사용하지 않는다.

6. 승인된 제거 Apply 후 Azure NIC effective NSG와 NSG 규칙에서 예외 부재를 확인한다. `ssh -o ControlMaster=no -o ControlPath=none ...`으로 새 tailnet 세션은 성공해야 한다. 운영자 public 경로의 **새 TCP22 연결**(`nc -vz -w 5 PUBLIC_IP 22`)은 거부/timeout이어야 한다. 예전 SSH 세션의 생존 여부는 차단 검증이 아니다. [NSG는 연결 상태를 유지하므로 변경은 새 연결에 적용](https://learn.microsoft.com/en-us/azure/virtual-network/network-security-groups-overview)된다. k3s 관리 포트 비노출도 재확인한다.

**실패 시 복구:** tailnet 새 세션이나 Control Plane 경로가 실패하면 제거 Apply를 진행하지 않고 /32 예외의 승인된 만료 시점 안에서 DNS/egress/ACL/daemon을 점검한다. 이미 제거한 뒤 tailnet이 끊기면 승인된 Azure Run Command로 비밀 없는 상태 조회·복구를 검토한다. 필요시 같은 운영자 /32만 여는 새 Terraform Plan을 작성하여 별도 승인을 받는다. UFW/NSG 전체 비활성화·광범위 포트 개방·개인키 복사·임의 State 변경으로 복구하지 않는다. 시간 제한을 넘기면 사용자 승인 범위에 따라 VM 할당 해제/정리를 검토한다.

### 5.2 k3s Worker 준비

확인된 cluster pin은 **k3s `v1.35.9+k3s1`**이다. Laptop VM server / Physical agent / Azure ARM64 agent를 같은 exact version으로 유지한다. 변경 승인 전 이미 설치된 노드의 version·역할·설정을 먼저 확인한다. Host를 node로 등록하지 않는다. [K3s release](https://github.com/k3s-io/k3s/releases/tag/v1.35.9%2Bk3s1).

새 노드 설치가 별도 승인되면 release의 해당 architecture binary와 checksum, version에 대응하는 installer를 다운로드해 검토하고 local cache에서 실행한다. installer 입력은 `INSTALL_K3S_VERSION='v1.35.9+k3s1'`로 고정한다. Azure/Physical worker에는 `INSTALL_K3S_EXEC='agent'`, Laptop control plane에는 `INSTALL_K3S_EXEC='server'`를 명시한다. agent YAML의 `server:`는 join 주소이며 installer의 service-mode 선택을 대신하지 않는다. `curl | sh`, `latest`나 channel 기본값에 의존하지 않는다. 재실행은 기존 config/token/service와 바이너리 version을 먼저 비교한 뒤 필요한 변경만 한다. 설치·재설치는 서비스를 시작/재시작할 수 있으므로 현재 read-only 승인으로 실행하지 않는다. [K3s configuration](https://docs.k3s.io/installation/configuration).

[`server`](../../bootstrap/nodes/k3s-server.yaml.example) / [`agent`](../../bootstrap/nodes/k3s-agent.yaml.example) 예제를 노드별 unique name·실제 tailnet IPv4로 채운다. `/etc/rancher/k3s/config.yaml`은 root 전용으로 관리한다. server는 `advertise-address`, `node-ip`, TLS SAN을 tailnet IPv4로 명시하며 SQLite 단일 server, Traefik/ServiceLB disabled다. agent join token은 승인된 별도 전달로 `/etc/rancher/k3s/agent-token`(0600)에 배치한다. YAML은 version 설치를 대신하지 않는다. secret content는 Terraform/cloud-init/CLI argv에 전달하지 않는다. SSH 개인키는 Arch Host의 운영자 전용 0600 파일에 유지하고 VM에 복사하거나 agent forwarding으로 전달하지 않는다. Tailscale 인증정보와 k3s token은 승인된 보호 경로의 root 전용 파일로 전달하며 shell history, installer 로그, Terraform state에 남기지 않는다.

Tailscale 승인·ACL과 OS firewall에서 노드 간 TCP 6443(server), TCP 10250, UDP 8472(VXLAN)를 역할별 제한한다. server/agent 모두 `flannel-iface=tailscale0`, node IP를 tailnet 주소로 사용한다. Pod/Service subnet route는 Tailscale에 광고하지 않는다. SSH tailnet 관리 경로를 확인한 후 별도 승인된 Terraform plan으로 임시 public SSH 예외를 제거한다.

검증은 세 Node Ready·architecture·exact k3s version·hostname/role, SQLite 상태, Traefik/ServiceLB 비활성화, 양방향 Pod/DNS 통신, direct/relay와 RTT, `tailscale0`/Flannel 실제 MTU·큰 payload, Host tailnet NodePort HTTP, public 포트 비노출, ARM native image 및 repository-scoped read-only ACR pull로 이어진다. 미등록 Azure worker를 완료로 기록하지 않는다. 현재 host kubectl 1.37.x는 server 1.35.x와 두 minor 차이이므로 허용된 ±1 minor 범위의 kubectl 또는 server의 `k3s kubectl`을 사용한다. [Kubernetes version skew](https://kubernetes.io/releases/version-skew-policy/).

### 5.3 현재 Tailscale 일반 접근과 장치 가입 설계

2026-10-13 사용자가 Admin Console의 General access rules에서 활성 규칙 `{"src":["*"],"dst":["*"],"ip":["*"]}`를 확인해 전달했다. **사용자의 화면 확인 증거**이며 에이전트가 콘솔이나 스크린샷 파일을 직접 검증한 것은 아니다. [Visual editor](https://tailscale.com/docs/reference/visual-editor)의 일반 규칙은 Grants 형식이며 [Grants](https://tailscale.com/docs/reference/syntax/grants)는 네트워크 접근을 허용한다. 동일 Tailnet에 정상 가입·승인된 장치 사이에서 다음 트래픽은 이 일반 규칙상 허용된다.

| 트래픽 | 일반 규칙상 판단 | 실제 확인 시점 |
| --- | --- | --- |
| Arch → Azure Worker TCP22 | 조건부 허용 | Bootstrap 후 새 OpenSSH 세션·host key·전용 공개키 인증 |
| Azure Worker → Control Plane TCP6443 | 조건부 허용 | Bootstrap 후 TCP/TLS API 경로 |
| Kubernetes 노드 간 TCP10250 | 조건부 허용 | 승인된 가입 이후 kubelet 경로·인증 |
| Kubernetes 노드 간 UDP8472 | 조건부 허용 | 가입 이후 VXLAN·양방향 Pod 통신; TCP probe로 대체하지 않음 |

현재 CLI의 Host Running/Online은 전체 관리자 정책을 제공하지 않는다. 현재 환경변수와 OpsLoop private 경로에서 사용 가능한 관리자 API 인증/정책 export를 찾지 못해 full policy를 읽지 못했다. 기존 legacy `acls`, 다른 `grants`, `tagOwners`, `ssh`, Device Approval 설정은 각각 미확인이다. 이 결과를 전체 정책/미래 장치 승인/실제 연결 성공으로 기록하지 않는다. [Device Approval](https://tailscale.com/docs/features/access-control/device-management/device-approval)이 활성화된 경우 새 장치는 관리자 승인 전 통신할 수 없으므로 Bootstrap 담당자가 확인·승인한다.

| Identity | 장점 | 제약·필요한 후속 검토 |
| --- | --- | --- |
| 운영자 사용자 소유 장치 — 이번 P1 권장 | 기존 사용자로 browser 로그인, 새로운 tagOwners나 auth key 없이 기존 규칙 활용 | 계정 상태·장치 승인·key expiry에 의존; 장기 서비스 identity와 운영자 계정이 결합 |
| `tag:opsloop-azure-worker` — 장기 운영 검토안 | 역할별 서버 identity·최소 권한 Grants 설계에 적합 | 실제 tagOwners/운영자 권한 확인 필요; 정의가 없으면 별도 정책 변경 승인. 태그가 사용자 identity를 대체하고 key expiry 기본값도 검토해야 함 |

[Tags](https://tailscale.com/docs/features/tags)는 서비스 장치용 identity다. 이번에는 `--advertise-tags`를 사용하거나 tagOwners를 추가하지 않는다. 일반 OpenSSH는 Azure에 전달한 Ed25519 공개키와 guest sshd로 인증하며 tailnet IP를 전송 경로로 쓴다. [Tailscale SSH](https://tailscale.com/docs/features/tailscale-ssh)는 별도 활성화와 `ssh` 정책이 필요한 기능이므로 이번 설계에서 활성화하지 않는다. 일반 Allow All이 Tailscale SSH 사용자 인증까지 승인한 것은 아니다.

장기 최소 권한안은 Arch→Worker TCP22, Worker→Control Plane TCP6443, 필요한 노드 identity 사이 TCP10250/UDP8472를 역할·방향별로 제한하고 정책 tests를 마련하는 것이다. Grants는 합산되므로 좁은 규칙을 추가해도 기존 Allow All을 제한하지 않는다. 전체 Tailnet 영향 검토와 별도 승인 후 변경해야 하며 이번에는 draft 적용·정책 변경이 없다. 실제 join·MTU/direct/relay·서비스 인증 성공은 Apply/Bootstrap 이후 검증한다.

### 5.4 승인 후 사용할 Bootstrap 실행 카드

아래 명령은 **준비만 했으며 실행하지 않았다**. Saved Plan Apply, guest 설치/Tailscale 가입, SSH 예외 제거 Apply, 기존 cluster에 agent 가입/테스트는 각각 승인 범위를 확인하고 순서대로 실행한다. 실제 Azure IP와 host key가 생기기 전의 placeholder는 성공 증거가 아니다. 현재 사용자 소유 장치로 가입하며 Tailscale SSH 기능은 켜지 않는다.

**① Arch에서 접속 변수와 host key 확인.** 아래 subscription은 현재 활성 구독 ID와 private Backend/Plan의 대상 ID가 같은 경우에만 사용한다. `opsloop`은 검토한 VM admin username이다. 개인키는 Host에만 보관하고 파일 내용은 출력하지 않는다.

```bash
set -euo pipefail
set +x
umask 077
OPSLOOP_PRIVATE_DIR=/home/sleepyminy/.config/opsloop/private
OPSLOOP_TF_ROOT=/home/sleepyminy/Projects/OpsLoop/infra/terraform/p1
OPSLOOP_SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
OPSLOOP_SSH_KEY=/home/sleepyminy/.ssh/id_ed25519_opsloop_azure
OPSLOOP_PUBLIC_IP="$(az network public-ip show --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-egress-pip --query ipAddress -o tsv)"
test -n "$OPSLOOP_PUBLIC_IP"
ssh-keyscan -T 10 -t ed25519 "$OPSLOOP_PUBLIC_IP" > "$OPSLOOP_PRIVATE_DIR/worker-hostkey.candidate"
ssh-keygen -lf "$OPSLOOP_PRIVATE_DIR/worker-hostkey.candidate"
```

`ssh-keyscan`은 신원 검증이 아니다. 다음 Azure Run Command는 **별도 승인된 관리-plane 실행 후** guest의 공개 host key fingerprint만 조회하는 방법이다. 비밀/가입 URL/token을 이 기능으로 전달하지 않는다. 이 읽기 목적의 guest 명령도 현재 실행하지 않았다. [Azure Run Command](https://learn.microsoft.com/en-us/azure/virtual-machines/linux/run-command).

```bash
az vm run-command invoke --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-arm-worker --command-id RunShellScript --scripts 'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub' --query 'value[].message' -o tsv
# 두 fingerprint를 운영자가 대조한 뒤에만 등록한다.
awk '$1 !~ /^#/ && NF == 3 {print "opsloop-azure-worker", $2, $3}' "$OPSLOOP_PRIVATE_DIR/worker-hostkey.candidate" > "$OPSLOOP_PRIVATE_DIR/worker-known-hosts"
test -s "$OPSLOOP_PRIVATE_DIR/worker-known-hosts"
chmod 600 "$OPSLOOP_PRIVATE_DIR/worker-known-hosts"
OPSLOOP_SSH_OPTS=(-i "$OPSLOOP_SSH_KEY" -o IdentitiesOnly=yes -o ForwardAgent=no -o StrictHostKeyChecking=yes -o HostKeyAlias=opsloop-azure-worker -o "UserKnownHostsFile=$OPSLOOP_PRIVATE_DIR/worker-known-hosts" -o ControlMaster=no -o ControlPath=none)
ssh "${OPSLOOP_SSH_OPTS[@]}" "opsloop@$OPSLOOP_PUBLIC_IP"
```

대조 실패 시 중단한다. `StrictHostKeyChecking=no`/`accept-new`로 우회하지 않는다. Azure 검증 경로가 준비되지 않으면 독립적으로 신뢰할 수 있는 host key 확인부터 준비한다.

**② 새 Worker의 승인된 guest 세션에서 Tailscale 설치/대화형 가입.** OS `VERSION_CODENAME=noble`, `dpkg --print-architecture=arm64`를 먼저 확인한다. 공식 서명 repo를 사용하고 root 설치 파일을 검토한다. 패키지 정확한 버전은 설치 시 기록한다. [Ubuntu Noble 공식 설치](https://pkgs.tailscale.com/stable/#ubuntu-noble).

```bash
set -euo pipefail
set +x
. /etc/os-release
test "$VERSION_CODENAME" = noble
test "$(dpkg --print-architecture)" = arm64
curl --fail --silent --show-error --location https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg -o /tmp/opsloop-tailscale-keyring.gpg
curl --fail --silent --show-error --location https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list -o /tmp/opsloop-tailscale.list
sudo install -m 0644 /tmp/opsloop-tailscale-keyring.gpg /usr/share/keyrings/tailscale-archive-keyring.gpg
sudo install -m 0644 /tmp/opsloop-tailscale.list /etc/apt/sources.list.d/tailscale.list
sudo apt-get update
sudo apt-get install --yes tailscale
tailscale version
sudo tailscale up --hostname=opsloop-azure-worker
tailscale ip -4
tailscale ping --c 3 100.73.4.76
timeout 10 openssl s_client -connect 100.73.4.76:6443 -brief </dev/null
```

로그인 URL은 보호된 운영자 터미널/브라우저에서만 사용한다. `--auth-key`, `--advertise-tags`, `--ssh`를 추가하지 않는다. 필요시 동일 Tailnet 관리자에게 Device Approval을 요청한다. TLS probe는 TCP/TLS 경로 확인이며 Kubernetes 사용자 인증/서버 신뢰 검증을 대신하지 않는다. 새 장치 승인 전 실패는 ACL 차단/egress 실패와 구분한다.

**③ Arch의 새 Tailnet OpenSSH 세션과 SSH 예외 제거.** 실제 Worker `tailscale ip -4` 결과를 입력한다. 승인된 정책/장치 승인 후 새 세션이 성공하기 전에는 공개 예외를 제거하지 않는다.

```bash
read -r -p '승인된 Azure Worker Tailscale IPv4: ' OPSLOOP_WORKER_TS_IP
tailscale ping --c 3 "$OPSLOOP_WORKER_TS_IP"
ssh "${OPSLOOP_SSH_OPTS[@]}" "opsloop@$OPSLOOP_WORKER_TS_IP" 'hostname; uname -m; tailscale status --self'
# 5.1의 close-public-ssh.tfvars 및 제거 Saved Plan을 생성·검토한다.
# NSG SSH Allow 제거만 기대한다. VM 교체/추가 생성/Backend 변경이면 중단한다.
# 해당 제거 Plan의 명시적 Apply 승인 뒤에만 실행한다.
terraform -chdir="$OPSLOOP_TF_ROOT" apply -input=false -lock-timeout=5m "$OPSLOOP_PRIVATE_DIR/p1-close-public-ssh.tfplan"
az network nsg show --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-nsg --query 'securityRules[].{name:name,access:access,source:sourceAddressPrefix,port:destinationPortRange}' -o table
az network nic list-effective-nsg --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-nic -o json > "$OPSLOOP_PRIVATE_DIR/worker-effective-nsg.json"
ssh "${OPSLOOP_SSH_OPTS[@]}" "opsloop@$OPSLOOP_WORKER_TS_IP" 'true'
if nc -z -w 5 "$OPSLOOP_PUBLIC_IP" 22; then
  printf '%s\n' 'FAIL: 새 공용 TCP22 연결이 허용됨' >&2
  exit 1
fi
```

허용 Source/발신 인터넷 경로가 처음과 같고 VM이 정상인 상태에서 검사한다. 공용 연결 실패 하나만으로 NSG 설정을 증명하지 않으며 effective rules·Tailnet 새 세션 성공과 함께 확인한다. 정상 차단 후 canonical `p1.tfvars`의 SSH source도 null로 갱신한다. 기존 공개 SSH 연결이 살아 있는 것은 stateful NSG 특성이며 새로운 연결 검증을 생략할 이유가 아니다.

**④ 공용 SSH 제거 후 k3s 가입 — 별도 승인.** ARM64 release binary, `sha256sum-arm64.txt`, 같은 tag의 `install.sh`를 검토한 뒤 사용한다. installer가 token 내용을 stdout에 기록하지 않도록 shell trace/CI log를 끈다. 정확한 [release](https://github.com/k3s-io/k3s/releases/tag/v1.35.9%2Bk3s1)와 [agent token-file 계약](https://docs.k3s.io/cli/agent)을 사용한다.

승인된 k3s agent join token은 사전 승인된 보호 경로로 Arch의 `~/.config/opsloop/private/k3s-agent-token`에 준비한다. Control Plane token을 터미널에서 출력하거나 환경변수/CLI 인수/cloud-init에 넣지 않는다. 파일은 운영자 소유·0600이어야 한다. 다음은 Host의 stdin→암호화된 SSH→guest root-only 파일 전달이며 개인키는 전달하지 않는다. `sudo -n` 실패 시 권한을 넓히지 않고 승인된 대화형 root 전달 절차로 돌아간다.

```bash
test -s "$OPSLOOP_PRIVATE_DIR/k3s-agent-token"
test "$(stat -c %a "$OPSLOOP_PRIVATE_DIR/k3s-agent-token")" = 600
test "$(stat -c %u "$OPSLOOP_PRIVATE_DIR/k3s-agent-token")" = "$(id -u)"
ssh -T "${OPSLOOP_SSH_OPTS[@]}" "opsloop@$OPSLOOP_WORKER_TS_IP" 'sudo -n install -d -m 0700 /etc/rancher/k3s && sudo -n sh -c '\''umask 077; cat > /etc/rancher/k3s/agent-token'\''' < "$OPSLOOP_PRIVATE_DIR/k3s-agent-token"
```

이후 Worker guest 세션에서 실행한다. 기존 binary/config/service가 있으면 덮어쓰지 않고 먼저 원인을 조사한다. 새 Worker라는 조건에서만 아래를 사용한다.

```bash
set -euo pipefail
set +x
umask 077
test ! -e /usr/local/bin/k3s
test ! -e /etc/rancher/k3s/config.yaml
OPSLOOP_K3S_STAGE="$(mktemp -d)"
cd "$OPSLOOP_K3S_STAGE"
OPSLOOP_K3S_RELEASE=https://github.com/k3s-io/k3s/releases/download/v1.35.9%2Bk3s1
curl --fail --silent --show-error --location "$OPSLOOP_K3S_RELEASE/k3s-arm64" -o k3s-arm64
curl --fail --silent --show-error --location "$OPSLOOP_K3S_RELEASE/sha256sum-arm64.txt" -o sha256sum-arm64.txt
awk '$2 == "k3s-arm64" {print}' sha256sum-arm64.txt > selected.sha256
test "$(wc -l < selected.sha256)" -eq 1
sha256sum --check selected.sha256
curl --fail --silent --show-error --location https://raw.githubusercontent.com/k3s-io/k3s/v1.35.9%2Bk3s1/install.sh -o install.sh
# 운영자가 install.sh를 검토하고 승인한 뒤 진행한다.
sudo install -m 0755 k3s-arm64 /usr/local/bin/k3s
OPSLOOP_WORKER_TS_IP="$(tailscale ip -4)"
sudo sh -c 'umask 077; cat > /etc/rancher/k3s/config.yaml' <<EOF
node-name: opsloop-azure-worker
node-ip: $OPSLOOP_WORKER_TS_IP
server: https://100.73.4.76:6443
token-file: /etc/rancher/k3s/agent-token
flannel-iface: tailscale0
EOF
sudo test -s /etc/rancher/k3s/agent-token
sudo chmod 0600 /etc/rancher/k3s/agent-token /etc/rancher/k3s/config.yaml
sudo env INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_VERSION=v1.35.9+k3s1 INSTALL_K3S_EXEC=agent sh ./install.sh
sudo systemctl is-active k3s-agent
k3s --version
ip -d link show tailscale0
ip -d link show flannel.1
```

token이 포함될 수 있는 로그는 evidence에 복사하지 않는다. journal은 필요시 보호된 터미널에서 최소한으로 확인하고 외부 보고는 실패 이유만 요약한다. 전체 노드의 `Ready`/exact version/architecture를 Arch의 기존 전용 kubectl로 확인한다. 기존 `verify-platform.py`는 **2노드 baseline 전용**이므로 3노드 수용시험에 사용하지 않는다.

```bash
OPSLOOP_KUBECTL=(/home/sleepyminy/.local/bin/kubectl-opsloop --kubeconfig /home/sleepyminy/.kube/opsloop.yaml)
"${OPSLOOP_KUBECTL[@]}" wait --for=condition=Ready node/opsloop-control node/sarmubuntuserver node/opsloop-azure-worker --timeout=180s
"${OPSLOOP_KUBECTL[@]}" get nodes -o custom-columns=NAME:.metadata.name,ARCH:.status.nodeInfo.architecture,K3S:.status.nodeInfo.kubeletVersion,IP:.status.addresses[0].address
```

3노드, AMD64/AMD64/ARM64, 각 `v1.35.9+k3s1` 및 Tailscale InternalIP를 확인한다. 네트워크 수용시험은 검증한 multi-arch probe image로 고유한 test Namespace에 노드별 Pod/ClusterIP Service를 배치한다. **각 Pod→다른 두 Pod의 직접 IP 및 Service DNS/HTTP를 모든 방향**으로 검증하고, 1 MiB payload·MTU·direct/relay 경로를 기록한다. TCP probe만으로 UDP8472를 검증 완료로 처리하지 않고 Pod 통신/Flannel 관측을 함께 판단한다. 노드 장애 복구, CPU credits 소진 상태 성능, ACR pull 권한은 별도 수용시험이다. 테스트용 Kubernetes 생성/삭제도 별도 승인이 필요하다.

다음 probe image는 이번에 Docker Hub manifest만 읽어 linux/amd64와 linux/arm64/v8을 확인했다. OCI index digest를 고정하며, 실제 cluster pull/실행 성공을 주장하지 않는다. 후속 시험은 승인된 새 Namespace에서만 수행한다. 기존 앱/namespace를 수정하지 않는다. 아래 HTTP 크기 검증은 DNS 응답만 받은 것을 Pod HTTP 성공으로 잘못 기록하지 않게 한다.

```bash
# 승인된 3노드 네트워크 수용시험에서만 실행. 앞의 OPSLOOP_KUBECTL 배열 재사용.
set -euo pipefail
OPSLOOP_PROBE_IMAGE=docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
OPSLOOP_TEST_NS="opsloop-arm64-nettest-$(date -u +%Y%m%d%H%M%S)"
# 같은 이름이 있거나 get 자체가 실패하면 중단하고 원인을 조사한다.
test -z "$("${OPSLOOP_KUBECTL[@]}" get namespace "$OPSLOOP_TEST_NS" --ignore-not-found -o name)"
"${OPSLOOP_KUBECTL[@]}" create namespace "$OPSLOOP_TEST_NS"
OPSLOOP_PROBE_NODES=(opsloop-control sarmubuntuserver opsloop-azure-worker)
for OPSLOOP_TEST_NODE in "${OPSLOOP_PROBE_NODES[@]}"; do
  "${OPSLOOP_KUBECTL[@]}" -n "$OPSLOOP_TEST_NS" run "probe-$OPSLOOP_TEST_NODE" --image="$OPSLOOP_PROBE_IMAGE" --restart=Never --overrides="{\"spec\":{\"nodeName\":\"$OPSLOOP_TEST_NODE\",\"tolerations\":[{\"operator\":\"Exists\",\"effect\":\"NoSchedule\"}],\"containers\":[{\"name\":\"probe\",\"image\":\"$OPSLOOP_PROBE_IMAGE\",\"command\":[\"sh\",\"-c\",\"mkdir -p /www; dd if=/dev/zero of=/www/payload.bin bs=1024 count=1024 2>/dev/null; exec httpd -f -p 8080 -h /www\"],\"resources\":{\"requests\":{\"cpu\":\"10m\",\"memory\":\"8Mi\"},\"limits\":{\"cpu\":\"100m\",\"memory\":\"32Mi\"}}}]}}"
  "${OPSLOOP_KUBECTL[@]}" -n "$OPSLOOP_TEST_NS" expose pod "probe-$OPSLOOP_TEST_NODE" --port=8080 --target-port=8080
done
"${OPSLOOP_KUBECTL[@]}" -n "$OPSLOOP_TEST_NS" wait --for=condition=Ready pod --all --timeout=180s
for OPSLOOP_SRC_NODE in "${OPSLOOP_PROBE_NODES[@]}"; do
  for OPSLOOP_DST_NODE in "${OPSLOOP_PROBE_NODES[@]}"; do
    test "$OPSLOOP_SRC_NODE" != "$OPSLOOP_DST_NODE" || continue
    OPSLOOP_DST_POD_IP="$("${OPSLOOP_KUBECTL[@]}" -n "$OPSLOOP_TEST_NS" get pod "probe-$OPSLOOP_DST_NODE" -o jsonpath='{.status.podIP}')"
    for OPSLOOP_HTTP_HOST in "$OPSLOOP_DST_POD_IP" "probe-$OPSLOOP_DST_NODE.$OPSLOOP_TEST_NS.svc.cluster.local"; do
      "${OPSLOOP_KUBECTL[@]}" -n "$OPSLOOP_TEST_NS" exec "probe-$OPSLOOP_SRC_NODE" -- sh -c 'set -e; wget -T 10 -q -O /tmp/payload.bin "$1"; test "$(wc -c < /tmp/payload.bin)" -eq 1048576' sh "http://$OPSLOOP_HTTP_HOST:8080/payload.bin"
      printf 'PASS %s -> %s\n' "$OPSLOOP_SRC_NODE" "$OPSLOOP_HTTP_HOST"
    done
  done
done
# 성공/실패의 Pod placement·오류 이유·MTU/direct/relay 기록 후, 이 시험에서 만든 namespace만 정리한다.
"${OPSLOOP_KUBECTL[@]}" delete namespace "$OPSLOOP_TEST_NS" --wait=true
```

`nodeName`은 네트워크 검증용 probe의 배치 통제다. OpsLoop workload는 기존 기본 Kubernetes Scheduler를 사용하는 책임 경계를 유지한다. `set -e`로 중간 실패하면 namespace가 보존되므로 담당자는 증거 확인 후 위 마지막 cleanup만 별도로 수행한다. 임시 probe의 pull/app 시작 오류를 Flannel 실패로 오분류하지 않는다.

**실패 시 복구:** 최초 SSH 실패는 IP/host key/NSG/key authentication을 구분해 확인한다. Tailscale 실패는 device approval/identity/egress/DNS/daemon을 조사하고 성공 전에 공개 SSH를 닫지 않는다. 닫은 뒤의 실패는 승인된 Run Command 상태 확인 또는 운영자 /32만 다시 여는 별도 Plan/승인으로 복구한다. k3s 실패는 token-file 권한·exact version·Control Plane TLS/도달성·VXLAN/MTU를 확인한다. 기존 2노드 재시작·전체 방화벽 해제·cluster reset으로 처리하지 않는다. 기한에 도달하면 SleepyMiny가 별도 승인된 deallocate를 수행하고 부분 구축 상태도 State를 보존한다.

## 6. 실패 시 복구와 삭제

### 6.1 48시간 운영 시계와 비용 통제

최종 감사에서 subscription 및 현재 유일한 Backend RG의 Budgets REST GET(`2024-08-01`)은 모두 빈 목록이었다. **월 US$20 Azure Budget Alert는 구성되지 않았다.** Billing account의 별도 알림까지 조사한 것은 아니다. 예산 약속과 실제 Budget 생성은 구분한다.

**후속 승인 및 통화 확인:** 사용자는 Budget 생성과 운영자 이메일 수신을 승인했다. 2026-10-13 05:36 KST 해당 subscription의 연결된 MCA Billing Profile GET에서 실제 청구 통화 **KRW**를 확인했다. 아래 amount 20 USD 가정 초안은 그대로 실행할 수 없다. 이번 달 적용 환율과 정확한 KRW 금액을 사용자에게 확인한 뒤 생성하며, 임의 환산·scope 변경·Forecast 알림 생략은 금지한다. Subscription Budget은 계속 0개다. 이메일은 Git 밖 private 요청에서만 취급하고 공개 문서에는 복사하지 않는다. [최신 검증 기록](../experiments/p1-d-azure-backend-initialization.md#10-승인된-budget-사전-점검과-git-checkpoint-검증).

최소 제안은 subscription scope의 `opsloop-monthly-warning`, category Cost / timeGrain Monthly / amount 20, Actual 80%·100% 및 Forecasted 100% 알림이다. 원래 청구 통화가 USD인지 Cost Management에서 확인한 뒤 금액을 설정한다. Retail USD 가격/학생 credit 표기만으로 Budget 통화를 단정하지 않는다. 시작일은 해당 월 1일, 종료일과 실제 운영자 알림 수신자를 승인받는다. 기존 Owner 권한을 재사용하고 추가 RBAC·Action Group·Automation은 최소안에 포함하지 않는다. 사용자가 승인한 실제 수신자 주소는 Git 밖 요청 파일에만 둔다. 아래 REST 설정안은 **생성하지 않았으며**, Budget 생성·알림 수신자·기간의 별도 승인이 필요하다. [Budget API 계약](https://learn.microsoft.com/en-us/rest/api/consumption/budgets/create-or-update?view=rest-consumption-2024-08-01).

```json
{
  "properties": {
    "category": "Cost",
    "amount": 20,
    "timeGrain": "Monthly",
    "timePeriod": {
      "startDate": "2026-10-01T00:00:00Z",
      "endDate": "2026-12-31T00:00:00Z"
    },
    "notifications": {
      "Actual80": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": 80, "thresholdType": "Actual", "contactEmails": ["REPLACE_APPROVED_OPERATOR_EMAIL"]},
      "Actual100": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": 100, "thresholdType": "Actual", "contactEmails": ["REPLACE_APPROVED_OPERATOR_EMAIL"]},
      "Forecast100": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": 100, "thresholdType": "Forecasted", "contactEmails": ["REPLACE_APPROVED_OPERATOR_EMAIL"]}
    }
  }
}
```

승인된 KRW 금액 확정 후 private `budget.request.json`(0600)의 금액/수신자/기간/통화를 검토하고 `az rest --method put --url "https://management.azure.com/subscriptions/$OPSLOOP_SUBSCRIPTION_ID/providers/Microsoft.Consumption/budgets/opsloop-monthly-warning?api-version=2024-08-01" --body "@$OPSLOOP_PRIVATE_DIR/budget.request.json" -o none`로 생성한다. 생성 전 같은 이름 존재 여부를 GET으로 확인해 기존 Budget을 덮어쓰지 않는다. 이후 GET으로 amount/notification enabled/recipient/기간을 확인하고 운영자가 수신 설정을 점검한다. Forecast API 오류 시 다른 조건으로 바꾸거나 임의 재시도하지 않고 오류를 분류해 보고한다. Azure Budget은 리소스를 정지시키지 않으며 비용 수집/평가가 지연되어 48시간 종료 타이머를 대신하지 못한다. [공식 Budget 운영 특성](https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-acm-create-budgets).

**시간 기준:** Apply 직전 UTC를 시작 시각으로 기록해 provisioning/부분 실패 중 과금 시간도 보수적으로 포함한다. SleepyMiny가 시작+24/36/47시간의 수동 점검 알림과 최대 48시간 종료 확인을 등록한다. 알림·자동 종료 리소스는 이번 작업에서 생성하지 않았다. 아래는 Apply 승인 이후 준비할 기록 명령이다.

```bash
umask 077
OPSLOOP_START_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OPSLOOP_STOP_UTC="$(date -u -d "$OPSLOOP_START_UTC +48 hours" +%Y-%m-%dT%H:%M:%SZ)"
printf 'owner=SleepyMiny\nstart_utc=%s\nstop_by_utc=%s\n' "$OPSLOOP_START_UTC" "$OPSLOOP_STOP_UTC" > "$OPSLOOP_PRIVATE_DIR/p1-experiment-window.txt"
# 종료/deallocate 승인 후, 48시간 이전에 실행한다. 단순 stop이나 guest shutdown으로 대체하지 않는다.
az vm deallocate --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-arm-worker --only-show-errors -o none
OPSLOOP_POWER_STATE="$(az vm get-instance-view --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-arm-worker --query "instanceView.statuses[?starts_with(code, 'PowerState/')].code | [0]" -o tsv)"
test "$OPSLOOP_POWER_STATE" = PowerState/deallocated
az disk show --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --name opsloop-p1-osdisk --query '{capacityGiB:diskSizeGb,sku:sku.name,tier:tier}' -o table
az resource list --subscription "$OPSLOOP_SUBSCRIPTION_ID" --resource-group opsloop-p1-rg --query '[].{name:name,type:type}' -o table
```

종료 확인 기록에는 실제 deallocated 관측 시각·남은 disk/PIP/ACR·별도 Backend 보존을 남긴다. 원본 OS disk의 크기는 VM 생성 후 `az disk show`로 확인하고 Standard SSD billing tier와 사용량을 공식 가격/Cost Analysis에 대조한다. `tier`가 비어 있으면 무상/미과금으로 판단하지 않는다. [VM 상태와 과금](https://learn.microsoft.com/en-us/azure/virtual-machines/states-billing), [deallocate CLI](https://learn.microsoft.com/en-us/cli/azure/vm?view=azure-cli-latest#az-vm-deallocate).

| 상태/작업 | Compute | 계속 남는 과금/주의 |
| --- | --- | --- |
| Running 또는 stopped/allocated | VM compute 과금 | disk/PIP/ACR/Backend도 유지 |
| VM deallocated 확인 | VM compute 중지 | disk/PIP/ACR/Backend 및 관련 사용량 유지; 노드는 NotReady가 될 수 있음 |
| 별도 승인된 testbed destroy | 소유 VM/disk/PIP/ACR 등 삭제 후 종료 | ACR artifacts도 삭제. Backend와 State 이력은 별도 보존; 최종 잔존/청구 확인 필요 |

기존 당일 공식 Retail 조회를 재사용한다. VM US$0.0464/h, PIP US$0.005/h, ACR Basic US$0.1666/day이며 합계 **48시간 US$2.80 / 730시간 US$42.59**에 미확정 OS disk·operations·network egress·Backend·세금 등이 더해진다. Deallocate 이후도 PIP/ACR만 월 환산 US$8.72 + disk/Backend가 남는다. 학생 US$100은 사용자 확인이며 실제 잔액/만료일은 미확인이다. 48시간 실험 승인은 지속 월간 운영이나 정리 실행 승인을 대신하지 않는다.

Budget 미구성은 이번 Saved Plan의 무결성 실패가 아니라 **Apply 전 운영 통제의 미완료 항목**이다. 알림 생성 승인을 받아 구성하거나, 지연/미구성 위험을 명시적으로 수용하고 수동 감시 절차를 승인받기 전에는 Apply 준비 완료로 선언하지 않는다.

### 6.2 부분 실패와 별도 승인된 정리

quota/restriction/policy/capacity/provider 미등록·backend 인증 실패 시 중단하고 오류 종류와 nonsecret resource identifiers만 기록한다. provider 자동 등록, quota 요청, region/image 교체 또는 광범위 port 개방으로 우회하지 않는다. Azure 조회 결과와 state ownership을 대조한 후 후속 변경 승인을 받는다.

부분 apply 실패는 backend/state를 보존하고 실제 생성된 resources와 새 plan을 보호된 터미널에서 대조한다. 재시도 전에 동일 subscription/key/lock 및 resource IDs를 확인한다. local state 삭제, `state rm`, 전체 RG 삭제, `-target`/`-lock=false` 우회로 처리하지 않는다. stale lock은 다른 writer 종료와 lock owner를 확인한 경우에만 별도 승인 후 해제한다. state 장애는 writer 중단→승인된 backend version/backup 복구→lineage/serial 및 Azure resources 대조→새 plan 검토 순서로 처리한다. state/backup 내용을 evidence에 출력하지 않는다.

SQLite datastore와 server token은 Terraform state와 별개로 함께 보호·복구해야 한다. cluster backup/restore는 해당 datastore 절차에 따라 별도 검증한다. 실패한 worker bootstrap을 반복하기 전에 version/config/token-file 권한·tailnet 경로를 확인한다. VM 재생성은 local application/data 복구를 대신하지 않는다.

삭제 담당자는 workload 철수·실험 종료와 backend 보존을 확인하고, Tailscale device·ACR node pull token·후속 OIDC/RBAC 정리를 각 소유자에게 요청한다. 이 인증 자원은 현재 testbed root가 관리하지 않는다. 승인된 testbed state와 같은 inputs를 사용해 삭제 계획을 먼저 만든다.

```bash
terraform -chdir=infra/terraform/p1 plan -destroy -input=false -lock-timeout=5m -var-file="$OPSLOOP_PRIVATE_DIR/p1.tfvars" -var='location=eastasia' -var='vm_size=Standard_B2pls_v2' -out="$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
terraform -chdir=infra/terraform/p1 show -no-color "$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
```

testbed RG·VM/inline OS disk·NIC·PIP·NSG·subnet/VNet·ACR만 삭제되고 backend account/container/RBAC가 포함되지 않는지 확인한다. ACR images도 제거되므로 필요한 artifact 보존을 먼저 끝낸다. **해당 destroy plan에 대한 별도 사용자 승인 후** 실행한다.

```bash
terraform -chdir=infra/terraform/p1 apply -input=false -lock-timeout=5m "$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
```

이후 Azure resource 목록·청구 대상 잔존 여부를 read-only 확인하고 remote state의 이력/backup을 보존 정책에 따라 유지한다. backend 자체 cleanup은 별도 승인·별도 root 책임이다. VM 정지/deallocate만으로 disk/PIP/ACR/backend 비용이 끝나지 않는다.
