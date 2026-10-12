# P1-C — 학생 구독 허용 지역 ARM64 후보

기준 commit: `fd8034e`. 조회일 **2026-10-13**, SKU/usage/image 02:06~02:09 KST, 공식 Retail Prices 02:11 KST. 기존 Azure for Students 로그인으로 읽기 전용 조회했다. 범위는 `eastasia`, `indonesiacentral`, `malaysiawest`, `norwayeast`, `polandcentral`뿐이다. 이전 [일본/한국 조사와 정책 차단](p1-c-azure-arm64-candidates.md)은 역사적 증거로 보존한다. 리소스 생성/삭제, provider 등록, policy/RBAC 변경, quota 요청, Kubernetes 변경, commit/push를 수행하지 않았다.

## 추천과 확인 범위

**East Asia / Standard_B2pls_v2를 우선 후보로 선정한다.** ARM64 2 vCPU/4 GiB, Gen2 지원, accelerated networking True, restrictions `[]`, regional vCPU `0/6`, Bpsv2 family `0/10`, VM count `0/25000`, 정확한 Ubuntu ARM64 이미지가 함께 확인됐다. 해당 Allowed Locations 정책에도 포함된다. 이는 현재 알려진 catalogue/쿼터/지역 정책 조건을 충족하는 후보이며, **실제 VM 생성 성공이나 전체 배포 준비 완료의 증거는 아니다**.

Hong Kong인 East Asia는 허용된 후보 지역 중 한국과 지리적으로 가깝다. 다른 네 지역에서 조건을 충족하는 ARM 후보도 확인되지 않아 우선순위가 명확하다. 물리적 위치는 지연의 대용 지표일 뿐이며 WAN 라우팅·Tailscale direct/DERP relay·실제 RTT·MTU·큰 payload는 VM 생성 없이 검증하지 못했다. [공식 Azure 지역 목록](https://learn.microsoft.com/en-us/azure/reliability/regions-list).

## 지역별 실제 CLI 결과

`current/limit` 표기다. 241개 minimum-resource/Gen2 ARM SKU를 평가했다. 고정 7개 이름에 한정하지 않고 모든 `CpuArchitectureType=Arm64` SKU를 조회했으며 모든 quota family를 한 번씩 수집했다. 후보는 2 vCPU/4 GiB 이상이며 region/각 family/VM-count headroom, restrictions, image와 NIC 호환성을 구분해 평가했다.

| 지역 | ARM 후보 수 | Regional / Bpsv2 / Dplsv6 quota | 최소 크기 ARM 후보 및 restrictions | 현재 판단 |
| --- | ---: | --- | --- | --- |
| eastasia | 96 | 0/6 / 0/10 / 0/0 | B2pls_v2·B2ps_v2·B4pls_v2·B4ps_v2 restrictions `[]`, catalog zone `[3]`; D v5 일부 restrictions `[]`이나 family 0; D v6 Location+Zone 제한 | B 후보 4개만 catalogue/쿼터/이미지/NIC 조건 통과 |
| indonesiacentral | 49 | 0/6 / 0/10 / 0/0 | B ARM SKU 없음; D2pls/plds/ps/pds_v6 및 E2ps/pds_v6 Location `NotAvailableForSubscription`; catalog zone `[3]`, restricted `[1]` | SKU 제한 및 family 0으로 차단 |
| malaysiawest | 0 | 0/6 / 0/10 / 0/0 | 이 구독의 ARM64 SKU 조회 응답 없음 | 조건에 맞는 catalogue 후보 없음; 글로벌 제공 여부로 확대하지 않음 |
| norwayeast | 48 | 0/6 / 0/10 / 0/0 | B ARM SKU 없음; D/E v6 Location 제한; catalog zone `[2]`, restricted `[1,2,3]` | SKU 제한 및 family 0으로 차단 |
| polandcentral | 48 | 0/6 / 0/10 / 0/0 | B ARM SKU 없음; D/E v6 Location 제한; catalog zone `[1]`, restricted `[1,2,3]` | SKU 제한 및 family 0으로 차단 |

모든 지역의 VM count quota는 `0/25000`, image 조회는 성공했다. Bpsv2 quota가 10이어도 B SKU가 없는 지역에 VM을 생성할 수 있다는 뜻은 아니다. 조회된 ARM D/E family는 모두 limit=0이며, Indonesia의 ARM GPU 계열도 quota 0이다. East Asia D2pls_v5/D2plds_v5/D2ps_v5/D2pds_v5 및 E v5의 빈 restrictions를 family quota 통과로 해석하지 않는다. Location 제한과 Zone 제한이 동시에 있으면 zone 선택으로 Location 제한을 우회할 수 없다. zone catalog와 restrictions가 불일치한 응답은 그대로 보존했다. 현재 Terraform은 zone을 강제하지 않는다.

### Azure Policy 실제 적용

assignment는 subscription scope, enforcementMode `Default`, effect `deny`, parameter `listOfAllowedLocations`다. definition의 allOf는 location이 허용 목록 밖이며 `global`이 아니고, resource type이 `Microsoft.AzureActiveDirectory/b2cDirectories`가 아닌 경우를 검사한다. 5개 조사 지역은 모두 허용 목록에 포함되고 VM은 type 예외가 아니다. VM·VNet·NSG·NIC·PIP·ACR 등 지역 자원에는 검토한 region을 일관되게 사용한다. 이 지역 정책의 통과를 전체 Azure Policy/RBAC/data-plane 통과로 확대하지 않는다. actual allocation capacity는 여전히 미검증이다.

### 이미지와 OS disk

5개 지역 모두 다음 exact URN을 반환했다.

```text
Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040
architecture=Arm64, hyperVGeneration=V2, plan=null
```

`osDiskImage.sizeInGb`는 null이며 **exact build 최소 OS disk 크기는 unknown**이다. [VM Image GET schema](https://learn.microsoft.com/en-us/rest/api/compute/virtual-machine-images/get?view=rest-compute-2025-04-01#osdiskimage)에 크기 필드가 없으므로 같은 조회를 반복하거나 fixture의 30 GiB를 실측으로 사용하지 않았다. Terraform의 32 GiB StandardSSD_LRS는 조건부 입력/비용 가정이다. 정확한 이미지 크기 근거를 확보하기 전 호환성 PASS 또는 전체 deployment gate를 선언하지 않는다. disk/VHD/VM을 만들어 확인하지 않았다.

## 비용 및 B/D 성능 구분

[공식 Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices)를 USD, Consumption, 일반 Linux VM으로 조회했다. Windows/Spot/Low Priority/Cloud Services/예약은 제외했다. GET endpoint `https://prices.azure.com/api/retail/prices`, api-version `2023-01-01-preview`, region filter는 위 5개 `armRegionName`, compute는 정확한 B2pls/B2ps/B4pls/B4ps_v2/D2pls_v5/D2pls_v6 `armSkuName`이다. 단위·meter·product·effectiveStartDate를 확인하고 페이지를 모두 읽었다. 조회일과 가격 적용 시작일은 다르다.

| East Asia 후보 | vCPU / RAM | VM USD/h | VM 730h USD | 선정 근거 |
| --- | --- | ---: | ---: | --- |
| Standard_B2pls_v2 | 2 / 4 GiB | 0.0464 | 33.872 | 1순위; 요구 최소 자원, 가장 낮은 VM 비용 |
| Standard_B2ps_v2 | 2 / 8 GiB | 0.0928 | 67.744 | 메모리 여유 대안; VM 비용 2배 |
| Standard_B4pls_v2 | 4 / 8 GiB | 0.1640 | 119.720 | quota 통과하지만 자원/비용 증가; 기본 P1에는 불필요 |
| Standard_B4ps_v2 | 4 / 16 GiB | 0.1860 | 135.780 | quota 통과하지만 비용/메모리 과대; 기본 P1에는 불필요 |

B VM 가격 effectiveStartDate는 `2023-09-01`이다. 위 4개 후보는 accelerated networking을 지원하고 동일 Bpsv2 family quota를 사용한다. B4 후보는 조사 결과에 포함하되 최소 변경 원칙에 따라 현재 Terraform allowlist에 추가하지 않았다.

| 비교용 D 후보 — 배포 차단 상태 | USD/h | VM 730h USD | 차단 |
| --- | ---: | ---: | --- |
| East Asia D2pls_v5 | 0.0860 | 62.780 | family 0/0 |
| East Asia D2pls_v6 | 0.0854 | 62.342 | family 0/0 + Location 제한 |
| Indonesia Central D2pls_v6 | 0.0725 | 52.925 | family 0/0 + Location 제한 |
| Norway East D2pls_v6 | 0.0888 | 64.824 | family 0/0 + Location 제한 |
| Poland Central D2pls_v6 | 0.0742 | 54.166 | family 0/0 + Location 제한 |

Malaysia West에서 해당 비교 SKU의 가격 응답도 없었다. 미제공 B/D 가격을 다른 지역에서 가져와 채우지 않는다. D v5 effectiveStartDate `2022-09-01`, D v6는 East Asia `2025-07-01`, Indonesia `2026-08-01`, Norway/Poland `2025-09-01`이다.

Bpsv2는 Ampere Altra burstable CPU다. B2pls_v2의 VM 기준 baseline 30%, initial 60 credits, 적립 36/h, 최대 864다. B2ps_v2는 baseline 40%, 적립 48/h, 최대 1152다. credit 소진 시 지속 CPU 성능과 복구 시간이 달라질 수 있으므로 runtime CPU/memory 지표와 Azure CPU Credits Remaining/Consumed를 함께 평가하는 후속 실험이 필요하다. [Bpsv2 공식 사양](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/bpsv2-series). 일반 D 계열은 이 B CPU credit 모델을 쓰지 않으며 Dplsv6는 Cobalt 100 물리 core/vCPU를 제공한다. [Dplsv6 공식 사양](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dplsv6-series). family quota=0인 D를 지속 CPU 대안으로 지금 선택할 수 없다.

ARM worker는 k3s agent, Node Exporter, native ARM Demo recovery candidate 중심으로 사용하고 Prometheus/Grafana·Engineering builds는 기존 control/Arch host 책임을 유지한다. B-series 초기 가용성을 일반 D-series의 지속 성능으로 가정하지 않는다.

East Asia 부대 meter는 E4 LRS Disk US$2.40/month, E4 operations US$0.002/10K, Standard IPv4 Static Public IP US$0.005/h, Basic Registry Unit US$0.1666/day다. Disk Mount meter는 shared disk 추가 mount이므로 일반 OS disk 기본 비용에 더하지 않는다. **B2pls_v2의 30일 720h 비교값은 VM 33.408 + SSD 2.40 + PIP 3.60 + ACR 4.998 = US$44.406**이다. 730h 및 같은 기간 ACR(730/24일) 비교값은 약 US$44.99이다. 달력 월/실제 청구 총액이 아니며 backend storage, transactions, egress, 추가 ACR 저장/Tasks, 세금·환율·학생 credit 차감을 제외한다. 잔여 credit·만료일이 확인되지 않았으므로 지속 운영 기간을 확정하지 않는다.

## Terraform 준비와 검증

기존 root [infra/terraform/p1](../../infra/terraform/p1)을 유지한다. [tfvars example](../../infra/terraform/p1/terraform.tfvars.example)의 location/size를 `eastasia`/`Standard_B2pls_v2`로 바꾸고 runbook의 plan/destroy 입력 pair를 맞춘다. exact image, 32 GiB 조건부 disk, VNet `10.80.0.0/16`, subnet `10.80.1.0/24`, Pod/Service CIDR, Tailscale underlay 및 secret 분리를 유지한다. 기본 D2pls_v6 변수는 architecture 선호로 보존하고 이미 허용된 B2 SKU를 재사용한다. 새 root/resource를 만들지 않는다.

`deployment_prerequisites_confirmed=false`, provider 자동 등록 none/empty list를 유지한다. Terraform 1.15.9 / AzureRM 4.64.0의 fmt, init -backend=false -input=false -lockfile=readonly, validate 통과. 예제 HCL은 stdin fmt로 확인하고 발견된 정렬 차이를 수정했다. init/validate는 알려진 sandbox DNS/plugin 제한 때문에 승인된 외부 실행을 사용했으며 Azure backend에 연결하지 않았다. plan은 backend·실입력·image disk·배포 전 조건이 미준비여서 실행하지 않는다. 로컬 테스트는 기존 23개와 broad discovery 2개로 25개 통과했다. 실 조회 exit 2는 provider/disk 및 나머지 조건 때문에 gate가 닫혀 있음을 뜻하며 SKU 조회 실패를 뜻하지 않는다.

## 남은 조건과 승인

- Network/Storage/ContainerRegistry는 NotRegistered, Compute는 Registered다. 등록은 이번 작업에서 금지하며 별도 명시적 승인이 필요하다.
- remote backend account/container, Blob Data Contributor, 접근 제한·잠금·복구, private tfvars 실입력·RSA 공개키 및 ACR 이름 가용성이 미준비/미검증이다. 실제 credentials/state/plan을 출력하지 않는다.
- exact image 최소 disk, 잔여 Students credit/만료일, 비용 상한·cleanup 시점, 초기 SSH /32 또는 승인된 관리 경로, Tailscale ACL/device 승인과 보호된 k3s token-file 전달이 필요하다. bootstrap의 join 대상은 기존 control plane `100.73.4.76:6443`, k3s pin `v1.35.9+k3s1`이다.
- quota/SKU/policy/image의 현재 관측은 allocation capacity를 보장하지 않는다. 현재 단계에서 capacity를 증명하려고 리소스를 생성하지 않는다.

선택한 후보는 정책 내 B2pls_v2로 진행할 수 있는 읽기 전용 근거를 얻었다. provider/backend/RBAC/비용·bootstrap을 승인·준비한 뒤 보호된 경로의 plan을 검토해야 한다. 실제 VM 생성은 별도 작업이며 이번 범위에 포함하지 않는다. 원본 응답 전체가 아닌 filtered cache/report만 ignored `.opsloop/azure-survey/`에 보관한다.

## 후속 checkpoint와 승인 준비

위 조사 결과를 재사용해 이번 배포 후보를 **eastasia / Standard_B2pls_v2 / Ubuntu 24.04 ARM64 exact build**로 고정했다. 후속 로컬 검증에서 Python 25개와 Terraform fmt/init(-backend=false)/validate가 다시 통과했다. [배포 runbook](../runbooks/azure-arm64-deployment.md)에 세 provider의 승인 후 등록 순서, 별도 최소 Blob backend/RBAC/bootstrap, 공식 backend 단가와 32 GiB 검증 방법·한계를 보완했다. 이 후속 작업에서도 provider 등록, backend/Azure 리소스 생성, 실제 backend init/plan/apply/destroy 및 Kubernetes 변경은 실행하지 않았다. 학생 구독 잔액·만료일과 exact image minimum disk는 계속 미확인이며 gate는 false다. 이 checkpoint의 Git commit/push 결과는 작업 완료 보고와 Git 이력으로 확인한다.
