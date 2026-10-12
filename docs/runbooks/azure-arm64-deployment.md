# Azure ARM64 deployment / destroy runbook

기준: [Architecture v0.1](../architecture/architecture-v0.1.md), [ADR-005](../adr/ADR-005-hybrid-multi-architecture-testbed.md), [P1 준비 절차](p1-platform-preparation.md), [P1-A inventory](../experiments/p1-a-environment-inventory.md), [현재 Azure ARM64 조사](../experiments/p1-c-azure-arm64-candidates.md). 아래 배포·bootstrap·삭제 명령은 **후속 운영 절차**이며 이번 작업에서 실행하지 않았다. 로컬 구성 검증은 Azure 접근 권한·배포 승인·P1 완료를 대신하지 않는다.

## 1. 배포 계약과 현재 gate

Terraform root는 [`infra/terraform/p1`](../../infra/terraform/p1)다. Terraform **1.15.9**, AzureRM **4.64.0**과 기존 `.terraform.lock.hcl`을 유지한다. provider 등록은 `resource_provider_registrations="none"`, `resource_providers_to_register=[]`로 금지한다. version 업데이트 및 `init -upgrade`는 별도 검토한다.

| 항목 | 계약 |
| --- | --- |
| 기본 ARM 후보 | Korea Central / `Standard_D2pls_v6`, 2 vCPU / 4 GiB; family quota 확인 전 배포 불가 |
| 현재 quota-positive 대안 | Japan East / `Standard_B2pls_v2`(2 vCPU / 4 GiB); `Standard_B2ps_v2`(2 vCPU / 8 GiB)도 별도 비용·자원 검토 대안 |
| OS image | `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`; region에서 Arm64 / Gen2 / plan 없음 / 최소 disk 재확인 |
| Network | VNet `10.80.0.0/16`, worker subnet `10.80.1.0/24`; Terraform validation으로 고정 |
| Cluster | Pod `10.42.0.0/16`, Service `10.43.0.0/16`, DNS `10.43.0.10`; Tailscale underlay |
| Ingress / egress | Standard static NIC Public IP로 명시적 egress; NSG inbound deny-all. 필요시 운영자 public IPv4 `/32`의 임시 TCP 22만 허용 |
| VM / ACR | RSA 공개키 인증만, password off, 32 GiB Standard SSD 기본값(이미지 최소 크기 미검증); ACR Basic 기본, admin/anonymous pull off |
| State | 별도 소유 remote backend; testbed destroy에 포함하지 않음 |
| Secret 경계 | SSH private key, Tailscale auth key, k3s token, ACR pull credential, kubeconfig, Azure client secret을 Terraform·cloud-init·Git에 넣지 않음 |

2026-10-12 재조회 결과: `Microsoft.Compute`는 Registered, Network / Storage / ContainerRegistry는 NotRegistered다. Korea Central regional total은 0/6, Dplsv6 family는 0/0으로 **선호 SKU quota gate가 실패**한다. 과거 `list-usage=[]` 결과를 현재 quota로 해석하지 않는다. 최신 regional/family quota와 대안은 [Azure ARM64 조사](../experiments/p1-c-azure-arm64-candidates.md)의 최종 기록에 대조한다. backend storage/config/RBAC, ACR 이름·정책·비용 승인·cleanup 책임도 준비 확인이 필요하다. `deployment_prerequisites_confirmed=false`를 유지하며 이번 작업에서는 plan을 실행하지 않았다.

Japan East의 B2pls_v2/B2ps_v2는 SKU object에서 Arm64 / V2 / accelerated networking 지원 / restrictions 빈 목록이 확인됐고 regional cores 0/6, `standardBpsv2Family` 0/10이다. 같은 exact Ubuntu image도 확인했다. Terraform은 이 두 SKU를 허용하지만 기본 D2pls_v6 값과 닫힌 gate는 유지한다. 지역을 `japaneast`, size를 B 후보로 **함께** 변경하고 나머지 선행조건을 검토한다. B-series의 CPU credits/bursting은 D-series와 다르므로 Recovery Candidate의 지속 CPU 가용성을 동일하게 가정하지 않는다. Korea Central의 B SKU 조회 빈 목록은 실제 SKU 존재·가용성을 증명하지 않는다. 제한 없는 SKU/양수 quota도 capacity를 보장하지 않는다.

정확한 image 조회의 `osDiskImage`는 Linux 정보만 반환해 최소 disk size를 확인하지 못했다. **32 GiB 기본값의 이미지 호환성을 검증한 것으로 기록하지 않는다.** image minimum을 신뢰할 수 있는 metadata/공식 경로로 확인해 `os_disk_size_gb`가 그 이상인지 검토한 뒤 gate를 연다.

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

## 4. 계획 검토와 승인된 배포

gate를 모두 충족한 후 운영자가 private tfvars의 `deployment_prerequisites_confirmed=true`를 설정한다. 이는 확인 기록이며 배포 권한을 부여하지 않는다. public SSH가 필요하면 승인된 운영자 현재 public IPv4 `/32`만 넣는다. null이면 SSH 22도 닫힌다. tailnet 외 Kubernetes 포트를 추가하는 plan은 거부한다.

```bash
terraform -chdir=infra/terraform/p1 plan -input=false -lock-timeout=5m -var-file="$OPSLOOP_PRIVATE_DIR/p1.tfvars" -out="$OPSLOOP_PRIVATE_DIR/p1.tfplan"
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

[`server`](../../bootstrap/nodes/k3s-server.yaml.example) / [`agent`](../../bootstrap/nodes/k3s-agent.yaml.example) 예제를 노드별 unique name·실제 tailnet IPv4로 채운다. `/etc/rancher/k3s/config.yaml`은 root 전용으로 관리한다. server는 `advertise-address`, `node-ip`, TLS SAN을 tailnet IPv4로 명시하며 SQLite 단일 server, Traefik/ServiceLB disabled다. agent join token은 승인된 별도 전달로 `/etc/rancher/k3s/agent-token`(0600)에 배치한다. YAML은 version 설치를 대신하지 않는다. secret content는 Terraform/cloud-init/CLI argv에 전달하지 않는다.

Tailscale 승인·ACL과 OS firewall에서 노드 간 TCP 6443(server), TCP 10250, UDP 8472(VXLAN)를 역할별 제한한다. server/agent 모두 `flannel-iface=tailscale0`, node IP를 tailnet 주소로 사용한다. Pod/Service subnet route는 Tailscale에 광고하지 않는다. SSH tailnet 관리 경로를 확인한 후 별도 승인된 Terraform plan으로 임시 public SSH 예외를 제거한다.

검증은 세 Node Ready·architecture·exact k3s version·hostname/role, SQLite 상태, Traefik/ServiceLB 비활성화, 양방향 Pod/DNS 통신, direct/relay와 RTT, `tailscale0`/Flannel 실제 MTU·큰 payload, Host tailnet NodePort HTTP, public 포트 비노출, ARM native image 및 repository-scoped read-only ACR pull로 이어진다. 미등록 Azure worker를 완료로 기록하지 않는다. 현재 host kubectl 1.37.x는 server 1.35.x와 두 minor 차이이므로 허용된 ±1 minor 범위의 kubectl 또는 server의 `k3s kubectl`을 사용한다. [Kubernetes version skew](https://kubernetes.io/releases/version-skew-policy/).

## 6. 실패 시 복구와 삭제

quota/restriction/policy/capacity/provider 미등록·backend 인증 실패 시 중단하고 오류 종류와 nonsecret resource identifiers만 기록한다. provider 자동 등록, quota 요청, region/image 교체 또는 광범위 port 개방으로 우회하지 않는다. Azure 조회 결과와 state ownership을 대조한 후 후속 변경 승인을 받는다.

부분 apply 실패는 backend/state를 보존하고 실제 생성된 resources와 새 plan을 보호된 터미널에서 대조한다. 재시도 전에 동일 subscription/key/lock 및 resource IDs를 확인한다. local state 삭제, `state rm`, 전체 RG 삭제, `-target`/`-lock=false` 우회로 처리하지 않는다. stale lock은 다른 writer 종료와 lock owner를 확인한 경우에만 별도 승인 후 해제한다. state 장애는 writer 중단→승인된 backend version/backup 복구→lineage/serial 및 Azure resources 대조→새 plan 검토 순서로 처리한다. state/backup 내용을 evidence에 출력하지 않는다.

SQLite datastore와 server token은 Terraform state와 별개로 함께 보호·복구해야 한다. cluster backup/restore는 해당 datastore 절차에 따라 별도 검증한다. 실패한 worker bootstrap을 반복하기 전에 version/config/token-file 권한·tailnet 경로를 확인한다. VM 재생성은 local application/data 복구를 대신하지 않는다.

삭제 담당자는 workload 철수·실험 종료와 backend 보존을 확인하고, Tailscale device·ACR node pull token·후속 OIDC/RBAC 정리를 각 소유자에게 요청한다. 이 인증 자원은 현재 testbed root가 관리하지 않는다. 승인된 testbed state와 같은 inputs를 사용해 삭제 계획을 먼저 만든다.

```bash
terraform -chdir=infra/terraform/p1 plan -destroy -input=false -lock-timeout=5m -var-file="$OPSLOOP_PRIVATE_DIR/p1.tfvars" -out="$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
terraform -chdir=infra/terraform/p1 show -no-color "$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
```

testbed RG·VM/inline OS disk·NIC·PIP·NSG·subnet/VNet·ACR만 삭제되고 backend account/container/RBAC가 포함되지 않는지 확인한다. ACR images도 제거되므로 필요한 artifact 보존을 먼저 끝낸다. **해당 destroy plan에 대한 별도 사용자 승인 후** 실행한다.

```bash
terraform -chdir=infra/terraform/p1 apply -input=false -lock-timeout=5m "$OPSLOOP_PRIVATE_DIR/p1-destroy.tfplan"
```

이후 Azure resource 목록·청구 대상 잔존 여부를 read-only 확인하고 remote state의 이력/backup을 보존 정책에 따라 유지한다. backend 자체 cleanup은 별도 승인·별도 root 책임이다. VM 정지/deallocate만으로 disk/PIP/ACR/backend 비용이 끝나지 않는다.
