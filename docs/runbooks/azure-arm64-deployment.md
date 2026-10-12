# Azure ARM64 deployment / destroy runbook

기준: [Architecture v0.1](../architecture/architecture-v0.1.md), [ADR-005](../adr/ADR-005-hybrid-multi-architecture-testbed.md), [P1 준비 절차](p1-platform-preparation.md), [P1-A inventory](../experiments/p1-a-environment-inventory.md), [기존 Azure 조사](../experiments/p1-c-azure-arm64-candidates.md), [최신 Backend 초기화 기록](../experiments/p1-d-azure-backend-initialization.md). 아래 VM 배포·bootstrap·삭제 명령은 **후속 운영 절차**이며 실행하지 않았다. 승인된 실제 Backend init과 연결 검증은 완료했다. 검증 통과는 VM 배포 승인·P1 완료를 대신하지 않는다.

## 1. 배포 계약과 현재 gate

Terraform root는 [`infra/terraform/p1`](../../infra/terraform/p1)다. Terraform **1.15.9**, AzureRM **4.64.0**과 기존 `.terraform.lock.hcl`을 유지한다. provider 등록은 `resource_provider_registrations="none"`, `resource_providers_to_register=[]`로 금지한다. version 업데이트 및 `init -upgrade`는 별도 검토한다.

| 항목 | 계약 |
| --- | --- |
| Architecture 선호 | `Standard_D2pls_v6`, 2 vCPU / 4 GiB; 현 family quota 0이며 Korea/Japan은 지역 정책 밖 |
| 이번 배포 후보 고정 | `eastasia` / `Standard_B2pls_v2` / 2 vCPU / 4 GiB; 다른 region/size는 새 검토 필요 |
| OS image | `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`; region에서 Arm64 / Gen2 / plan 없음 / 최소 disk 재확인 |
| Network | VNet `10.80.0.0/16`, worker subnet `10.80.1.0/24`; Terraform validation으로 고정 |
| Cluster | Pod `10.42.0.0/16`, Service `10.43.0.0/16`, DNS `10.43.0.10`; Tailscale underlay |
| Ingress / egress | Standard static NIC Public IP로 명시적 egress; NSG inbound deny-all. 필요시 운영자 public IPv4 `/32`의 임시 TCP 22만 허용 |
| VM / ACR | RSA 공개키 인증만, password off, 32 GiB Standard SSD 기본값(이미지 최소 크기 미검증); ACR Basic 기본, admin/anonymous pull off |
| State | 별도 소유 remote backend; testbed destroy에 포함하지 않음 |
| Secret 경계 | SSH private key, Tailscale auth key, k3s token, ACR pull credential, kubeconfig, Azure client secret을 Terraform·cloud-init·Git에 넣지 않음 |

다음 10월 12일 조사 및 초기 10월 13일 정책 차단 기록은 역사적 증거다. Provider 미등록·Backend 미준비 설명은 이후 등록·구축과 이번 연결 검증으로 해소되었으며, 최신 상태는 아래 현재 후보와 [Backend 연결 기록](../experiments/p1-d-azure-backend-initialization.md)을 따른다.

2026-10-12 재조회 당시 `Microsoft.Compute`는 Registered, Network / Storage / ContainerRegistry는 NotRegistered였다. Korea Central regional total은 0/6, Dplsv6 family는 0/0으로 **선호 SKU quota gate가 실패**했다. 과거 `list-usage=[]` 결과를 현재 quota로 해석하지 않는다.

과거 Japan East의 B2pls_v2/B2ps_v2는 SKU object에서 Arm64 / V2 / accelerated networking 지원 / restrictions 빈 목록, regional cores 0/6, `standardBpsv2Family` 0/10 및 exact Ubuntu image를 확인했으나 이후 지역 정책 차단을 확인했다. 이는 역사적 증거이며 현재 배포 입력으로 사용하지 않는다. Terraform의 기본 D2pls_v6 값은 architecture 선호로 보존하고, 이번 실제 입력은 East Asia/B2pls_v2로 명시한다. B-series의 CPU credits/bursting은 D-series와 다르므로 Recovery Candidate의 지속 CPU 가용성을 동일하게 가정하지 않는다. 제한 없는 SKU/양수 quota도 capacity를 보장하지 않는다.

정확한 image 조회의 `osDiskImage`는 Linux 정보만 반환해 최소 disk size를 확인하지 못했다. **32 GiB 기본값의 이미지 호환성을 검증한 것으로 기록하지 않는다.** image minimum을 신뢰할 수 있는 metadata/공식 경로로 확인해 `os_disk_size_gb`가 그 이상인지 검토한 뒤 gate를 연다.

통합 2단계에서는 미등록 세 provider를 재확인했고 Students 구독 spending limit은 On, 설정된 budget은 없었다. 구독 management-plane grant는 확인했지만 policy/deny/backend data-plane은 미검증이다. 구독에 연결된 MCA billing profile 조회에서도 학생 credit 잔액·만료를 확인하지 못했다. SKU/쿼터/가격 조사는 반복하지 않았다. 자세한 근거와 지속 운영 비용은 [2단계 결과](../experiments/p1-c-azure-arm64-candidates.md#통합-작업-2단계--미해결-배포-조건만-확인)에 기록했다. backend 실값·권한 및 최초 bootstrap 관리 경로가 준비되지 않아 현재 plan을 실행하지 않는다.

**2026-10-13 추가 차단 확인:** 활성 subscription Allowed Locations deny 정책은 `indonesiacentral`, `polandcentral`, `eastasia`, `norwayeast`, `malaysiawest`만 허용하며 Japan East VM은 예외가 아니다. 이전 SKU/쿼터 결과와 독립적인 차단이다. 정책을 변경/우회하지 않고 [최신 Phase A/B/C 기록](../experiments/p1-c-azure-arm64-candidates.md#2026-10-13-phase-abc-후속-확인)에 근거를 보존했다. 허용 지역의 ARM64 후보를 새로 검토하거나 적법한 정책 허용 경로를 확보하기 전 Japan East plan을 실행하거나 gate를 열지 않는다. Network/Storage/ContainerRegistry는 여전히 미등록이고 backend도 미준비다.

**현재 후보 확정 (2026-10-13):** East Asia/B2pls_v2의 regional 0/6, Bpsv2 family 0/10, restrictions 빈 목록, ARM64/Gen2/NIC 및 exact Ubuntu image를 재확인했다. East Asia는 subscription Allowed Locations 정책 안에 있다. 세 Provider Registered, 기존 Backend의 CLI/Entra 인증·container scope RBAC·공인 IP 접근·실제 init/validate는 통과했다. 초기화 전 없던 State Blob이 초기화 후 존재하며 default workspace의 관리 리소스는 0개다. 사용자가 US$100 credit을 확인했지만 만료일·비용 상한·cleanup 책임은 미확정이다. image minimum disk·RSA 공개키·최초 bootstrap 경로/ACL·capacity는 미검증이고 gate는 false다. Plan은 실행하지 않았다. [상세 기록](../experiments/p1-d-azure-backend-initialization.md).

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

설정과 산출물은 source tree 밖 운영자 전용 디렉터리(0700), 파일 0600으로 보관한다. 예를 들어 승인된 운영자 경로를 `OPSLOOP_PRIVATE_DIR`로 지정한다. [`backend.hcl.example`](../../infra/terraform/p1/backend.hcl.example)에는 account/container/key/tenant/subscription 식별자만, [`terraform.tfvars.example`](../../infra/terraform/p1/terraform.tfvars.example)에는 nonsecret 입력과 SSH **공개키**만 있다. 운영자는 placeholder를 교체하고 공개키 RSA 2048-bit 이상 여부를 `ssh-keygen -lf /path/to/approved-public-key.pub`로 확인한다. backend와 provider의 target subscription을 각각 확인하며 credential/env 전체 덤프는 하지 않는다.

새 배포이며 기존 local/remote state가 없음을 확인한 이후에만 후속 운영자가 다음을 실행한다. backend 인증·잠금/복구 준비가 실패하면 중단한다.

```bash
umask 077
export OPSLOOP_PRIVATE_DIR=/path/to/operator-private-opsloop
terraform -chdir=infra/terraform/p1 init -input=false -lockfile=readonly -backend-config="$OPSLOOP_PRIVATE_DIR/backend.hcl"
terraform -chdir=infra/terraform/p1 workspace show
```

`default` workspace와 독립 P1 key를 사용한다. backend identity/key를 바꾸는 `-reconfigure`나 기존 state를 이동하는 `-migrate-state`는 신규 배포 명령에 추가하지 않는다. 기존 state가 발견되면 writer를 중단하고 보호된 backup·lineage/serial·리소스 소유권·복구 계획을 별도로 검토한다. `.terraform/`도 backend metadata를 포함하므로 source와 분리된 runtime 자료로 취급하고 Git에 올리지 않는다.

### 3.4 32 GiB OS disk 검증 방법과 plan 진입 조건

현재 exact URN은 `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`다. 읽기 전용 `az vm image show --location eastasia --urn ...`는 Arm64/Gen2를 확인했지만 응답의 `osDiskImage`에 disk size가 없다. 같은 조회를 반복해도 32 GiB 호환성 증거가 되지 않는다. 현재 **image minimum unknown / 32 GiB compatibility unknown**이다.

검증 경로는 다음과 같이 구분한다.

1. **우선 읽기 전용 근거:** Canonical의 해당 Azure Marketplace exact build에 대한 원본 가상 OS disk 크기나 지원 확인을 확보한다. 정확히 같은 artifact임을 publisher/build/checksum으로 입증할 수 있는 공식 VHD가 있다면 `qemu-img info --output=json /path/to/verified-exact-image.vhd`의 `virtual-size <= 34359738368`(32 × 1024³ bytes)인지 확인한다. 압축 파일 크기, root filesystem 사용량, 다른 Ubuntu cloud image 또는 일반적인 30 GiB 안내는 대체 증거가 아니다. 현재 그 artifact/확인 자료는 확보하지 못했다.
2. **provider preflight:** provider 등록 및 승인된 기존 RG/권한이 준비된 후, 같은 URN/ARM64 size/`storageProfile.osDisk.diskSizeGB=32`를 가진 검토된 ARM template로 `az deployment group validate --validation-level Provider`를 수행하는 방법이 있다. 별도 배포 프로젝트로 전환하지 않고 검증용 template만 사용한다. CLI 지원 여부와 Azure Policy/RBAC를 먼저 확인한다. provider의 disk-too-small 오류는 FAIL 근거지만, 성공 결과에 disk 호환성 검사가 명시되지 않으면 정확한 minimum 확인으로 간주하지 않는다. Terraform validate/plan이나 ARM의 Template-only 검증은 source image 크기 검증을 대신하지 않는다. 이번에는 template 생성이나 remote validation도 실행하지 않았다. [Provider validation CLI](https://learn.microsoft.com/en-us/cli/azure/deployment/group?view=azure-cli-latest#az-deployment-group-validate).
3. **실제 생성 검증:** 위 경로가 충분한 근거를 제공하지 못하면 비용·리소스 생성의 별도 승인 후 실제 OS disk provisioning 및 VM boot로 확인해야 한다. 이는 현재 금지 범위이며, 지금 호환성 PASS로 기록하지 않는다. 64 GiB로 임의 증설해도 image minimum 증거가 생기지 않고 비용 tier가 바뀐다.

Backend init은 승인된 기존 account/container의 인증·접근·state 충돌 여부를 먼저 확인하고 수행한다. VM 입력·디스크·bootstrap gate와 구분하며 현재 init은 완료했다. **VM Plan**에 진입하기 전 다음을 모두 기록해야 한다. 부족한 항목은 승인이 있어도 검증 완료로 바꾸지 않는다.

- 세 provider Registered; 승인된 구독/관리-plane 권한/전체 적용 Policy와 ACR global name 확인.
- backend account/container, container Blob Data Contributor 및 firewall 접근, lease 잠금·복구/소유권 준비; 다른 state와 key 충돌 없음.
- private tfvars의 East Asia/B2pls_v2/exact image/RSA 공개키와 32 GiB 호환성 근거; 실제 SSH 개인키·토큰은 포함하지 않음.
- Azure for Students **US$100 credit은 사용자 확인, 만료일은 미확인**이다. 소비 이후 실잔액과 비용 상한·종료 시점/cleanup 책임을 확인한다. spending limit On과 budget 없음은 충분한 운영 예산을 증명하지 않는다.
- 최초 관리 경로 승인(필요시 운영자 public /32 TCP 22만), 보호된 Tailscale 등록 및 k3s token-file 전달 절차; join은 `100.73.4.76:6443`, version은 `v1.35.9+k3s1`. Kubernetes 관리 포트는 인터넷에 열지 않음.
- plan 직전 변화 가능한 quota/restrictions/policy/provider 상태만 재확인; 실제 allocation capacity는 apply 전 확정할 수 없다고 기록.

**현재 결과:** 이번 25개 Python 테스트와 `fmt -check` / 실제 Azure Backend `init` / `validate` / `workspace show` / `state list`는 통과했다. 사용자가 만든 Observability values는 변경하지 않았다. image disk·RSA 공개키·최초 bootstrap/ACL·비용 및 종료 조건이 남아 `deployment_prerequisites_confirmed=false`이며 Plan/apply/destroy는 실행하지 않았다. 조건 충족 후 Plan은 이번 승인 범위다. 실제 worker 생성과 Tailscale/k3s bootstrap은 별도 후속 승인이다.

## 4. 계획 검토와 승인된 배포

gate를 모두 충족한 후 운영자가 private tfvars의 `deployment_prerequisites_confirmed=true`를 설정한다. 이는 확인 기록이며 배포 권한을 부여하지 않는다. `p1.tfvars`의 placeholder subscription/ACR 이름/RSA 공개키를 승인된 실값으로 교체하고 exact image를 유지한다. public SSH가 필요하면 승인된 운영자 현재 public IPv4 `/32`만 넣는다. null이면 SSH 22도 닫힌다. 새 VM에는 Terraform이 Tailscale을 설치하지 않으므로 최초 bootstrap에는 이 임시 예외 또는 별도 승인된 관리 경로가 있어야 한다. tailnet 외 Kubernetes 포트를 추가하는 plan은 거부한다.

다음 명령은 현재 검토 후보 **East Asia/B2pls_v2**를 명시한다. **현재는 image disk·RSA 공개키·최초 bootstrap/ACL·비용 및 종료 조건이 남아 실행하지 않는다.** 모든 차단 조건 해소 후에만 실행한다. private 입력의 region/size를 같은 pair로 유지한다. `p1.tfvars.draft`는 미완성 초안이며 `p1.tfvars`로 사용하지 않는다. Korea/Japan으로 되돌리거나 zone을 선택하면 정책/SKU 계약을 새로 검토한다. Provider는 이미 Registered이며 Terraform 자동 등록은 금지 상태를 유지한다.

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

확인된 cluster pin은 **k3s `v1.35.9+k3s1`**이다. Laptop VM server / Physical agent / Azure ARM64 agent를 같은 exact version으로 유지한다. 변경 승인 전 이미 설치된 노드의 version·역할·설정을 먼저 확인한다. Host를 node로 등록하지 않는다. [K3s release](https://github.com/k3s-io/k3s/releases/tag/v1.35.9%2Bk3s1).

새 노드 설치가 별도 승인되면 release의 해당 architecture binary와 checksum, version에 대응하는 installer를 다운로드해 검토하고 local cache에서 실행한다. installer 입력은 `INSTALL_K3S_VERSION='v1.35.9+k3s1'`로 고정한다. Azure/Physical worker에는 `INSTALL_K3S_EXEC='agent'`, Laptop control plane에는 `INSTALL_K3S_EXEC='server'`를 명시한다. agent YAML의 `server:`는 join 주소이며 installer의 service-mode 선택을 대신하지 않는다. `curl | sh`, `latest`나 channel 기본값에 의존하지 않는다. 재실행은 기존 config/token/service와 바이너리 version을 먼저 비교한 뒤 필요한 변경만 한다. 설치·재설치는 서비스를 시작/재시작할 수 있으므로 현재 read-only 승인으로 실행하지 않는다. [K3s configuration](https://docs.k3s.io/installation/configuration).

[`server`](../../bootstrap/nodes/k3s-server.yaml.example) / [`agent`](../../bootstrap/nodes/k3s-agent.yaml.example) 예제를 노드별 unique name·실제 tailnet IPv4로 채운다. `/etc/rancher/k3s/config.yaml`은 root 전용으로 관리한다. server는 `advertise-address`, `node-ip`, TLS SAN을 tailnet IPv4로 명시하며 SQLite 단일 server, Traefik/ServiceLB disabled다. agent join token은 승인된 별도 전달로 `/etc/rancher/k3s/agent-token`(0600)에 배치한다. YAML은 version 설치를 대신하지 않는다. secret content는 Terraform/cloud-init/CLI argv에 전달하지 않는다. SSH 개인키는 Arch Host의 운영자 전용 0600 파일에 유지하고 VM에 복사하거나 agent forwarding으로 전달하지 않는다. Tailscale 인증정보와 k3s token은 승인된 보호 경로의 root 전용 파일로 전달하며 shell history, installer 로그, Terraform state에 남기지 않는다.

Tailscale 승인·ACL과 OS firewall에서 노드 간 TCP 6443(server), TCP 10250, UDP 8472(VXLAN)를 역할별 제한한다. server/agent 모두 `flannel-iface=tailscale0`, node IP를 tailnet 주소로 사용한다. Pod/Service subnet route는 Tailscale에 광고하지 않는다. SSH tailnet 관리 경로를 확인한 후 별도 승인된 Terraform plan으로 임시 public SSH 예외를 제거한다.

검증은 세 Node Ready·architecture·exact k3s version·hostname/role, SQLite 상태, Traefik/ServiceLB 비활성화, 양방향 Pod/DNS 통신, direct/relay와 RTT, `tailscale0`/Flannel 실제 MTU·큰 payload, Host tailnet NodePort HTTP, public 포트 비노출, ARM native image 및 repository-scoped read-only ACR pull로 이어진다. 미등록 Azure worker를 완료로 기록하지 않는다. 현재 host kubectl 1.37.x는 server 1.35.x와 두 minor 차이이므로 허용된 ±1 minor 범위의 kubectl 또는 server의 `k3s kubectl`을 사용한다. [Kubernetes version skew](https://kubernetes.io/releases/version-skew-policy/).

## 6. 실패 시 복구와 삭제

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
