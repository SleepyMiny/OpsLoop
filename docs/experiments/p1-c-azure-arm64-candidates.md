# P1-C — Azure ARM64 후보 조사

기준일: **2026-10-12**, 라이브 CLI 관측 17:22~17:24 KST 및 같은 날 공식 가격·사양 조회. 대상은 기존 기본 구독 **Azure for Students / Enabled**다. 구독 ID·tenant·토큰·자격 증명을 문서와 조사 캐시에 저장하지 않았다. Azure 리소스 생성·삭제·provider 등록·쿼터 요청은 수행하지 않았다.

## 결정에 대한 답

**2026-10-12 조사에서는 Japan East / Standard_B2pls_v2가 첫 후보였지만, 2026-10-13에는 구독의 활성 지역 deny 정책으로 배포 불가임을 확인했다.** SKU의 2 vCPU / 4 GiB / ARM64 / Gen2 / Accelerated Networking 지원, 빈 restrictions 및 이전 쿼터 여유는 정책 허용을 대신하지 않는다. 아래 Phase A/B/C 후속 기록에서 현재 제약을 확인한다. Network·Storage·ContainerRegistry 미등록, OS 이미지 최소 디스크 크기 미확인, 잔여 credit·backend 등도 남아 **배포 gate는 닫혀 있다**.

ADR-005의 선호 Standard_D2pls_v6는 유지되는 설계 기준이다. 현 구독에서는 조사한 세 지역 모두 해당 family limit=0이어서 즉시 사용할 후보로 추천할 수 없다. Bpsv2 선택은 burstable 성능을 수용하는 실험상의 대안이며 실제 native ARM Demo·부하·복구 검증 후 결정해야 한다. 이 조사로 architecture를 변경하거나 P1 완료를 선언하지 않는다.

## 확인된 구독·지역 결과

아래 `current/limit`은 `az vm list-usage` 응답값이다. regional과 family 둘 다 필요하며 작은 값이 실효 한계다. 세 지역 모두 VM count `0/25000`을 반환했다. **카탈로그에 없는 SKU를 quota 양수라는 이유로 사용할 수 있다고 해석하지 않는다.** [Azure vCPU quotas](https://learn.microsoft.com/en-us/azure/virtual-machines/quotas).

| 지역 | Regional vCPU | Bpsv2 family | Dplsv6 / DPLSv5 / Dpldsv6 / Dpsv6 / DPSv5 family | B2pls_v2 / B2ps_v2 SKU 응답 | 판단 |
| --- | --- | --- | --- | --- | --- |
| Korea Central | 0/6 | 0/10 | 각각 0/0 | 둘 다 카탈로그 응답 없음 | 지리적 우선 지역이나 현재 4 GiB quota-positive ARM 후보 확인 못함 |
| Japan East | 0/6 | 0/10 | 각각 0/0 | 둘 다 존재, restrictions `[]`, catalog zones `[1]` | B2pls_v2 첫 후보; B2ps_v2는 8 GiB 대안 |
| Japan West | 0/6 | 0/10 | 각각 0/0 | 둘 다 카탈로그 응답 없음 | Japan East보다 낫다는 근거 없음 |

| 조회된 D 계열 | Korea Central | Japan East | Japan West |
| --- | --- | --- | --- |
| D2pls_v6 / D2plds_v6 / D2ps_v6 | catalog zones `[1,2,3]`; Zone restriction `[2]` | catalog zones `[1,2,3]`; restrictions `[]` | catalog zones `[1,2]`; Zone restriction `[1,3]` |
| D2pls_v5 / D2ps_v5 | SKU 응답 없음 | catalog zones `[1]`; restrictions `[]` | SKU 응답 없음 |

Zone restriction의 reason은 `NotAvailableForSubscription`이었다. Japan West는 catalog `[1,2]`와 restricted `[1,3]`이 동시에 반환됐으므로 원본 차이를 그대로 기록한다. zone을 선택한다면 현재 근거상 zone 2만 검토할 수 있으나 family quota=0이라 현재 후보가 되지 않는다. regional 배포에 Zone 제한을 Location 전체 제한으로 적용하지 않는다. `--all`로 얻은 SKU 존재나 빈 restrictions도 actual capacity 증거가 아니다. [Quota와 capacity 구분](https://learn.microsoft.com/en-us/azure/virtual-machines/quotas#quota-and-capacity).

기존 [P1-A inventory](p1-a-environment-inventory.md)의 provider 모두 NotRegistered·quota 빈 응답은 당시 snapshot이다. 이번 재조회는 Compute Registered와 실제 quota 값을 반환했다. 등록 상태 변경 경위나 이전 빈 응답의 원인을 이 조사로 단정하지 않는다.

| Provider | 최신 registrationState |
| --- | --- |
| Microsoft.Compute | Registered |
| Microsoft.Network | NotRegistered |
| Microsoft.Storage | NotRegistered |
| Microsoft.ContainerRegistry | NotRegistered |

추가 count-only 조회에서 현재 구독의 `az acr list`와 `az storage account list`는 각각 **0개**를 반환했다. 현재 구독 안에서 기존 ACR/Storage backend를 식별하지 못했다. 다른 구독의 backend·blob container·권한·ACR 이름 가용성은 조사하지 않았으므로 remote state나 ACR 준비 완료로 해석하지 않는다.

## 후보 사양·Ubuntu 이미지

Japan East B2pls_v2와 B2ps_v2의 API capabilities는 `CpuArchitectureType=Arm64`, `vCPUs=2`, `HyperVGenerations=V2`, `AcceleratedNetworkingEnabled=True`다. 메모리는 각각 4 GiB와 8 GiB다. 현재 Terraform의 accelerated NIC와 capability가 일치한다. Dplsv6는 공식 사양에서 Accelerated Networking을 필수로 명시한다. [Dplsv6 공식 사양](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dplsv6-series) (문서 갱신 2026-03-11, 조회 2026-10-12).

Bpsv2는 Ampere Altra 기반 burstable ARM이다. B2pls_v2는 VM 전체 기준 baseline CPU 30%, initial 60 credits, 36 credits/hour 적립, 최대 864 credits를 갖는다. B2ps_v2는 baseline 40%, initial 60, 48/hour, 최대 1152다. 크레딧 소진 시 지속 CPU 성능이 복구 지연에 영향을 줄 수 있으므로 `CPU Credits Remaining/Consumed`와 실제 요청·부하를 함께 측정해야 한다. 이 worker에는 k3s agent·exporter·Demo 중심으로 배치하고 Prometheus·Engineering build를 넣지 않는 architecture 계약을 유지한다. [Bpsv2 공식 사양](https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/bpsv2-series) (갱신 2026-03-11, 조회 2026-10-12).

| 이미지 항목 | 세 지역에서 확인한 결과 |
| --- | --- |
| Exact URN | `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040` |
| Architecture / Hyper-V | Arm64 / V2 |
| Marketplace plan | null |
| OS disk metadata | `osDiskImage={operatingSystem:Linux}`; sizeInGb 제공되지 않음 |
| 32 GiB OS disk 호환성 | **미확인**. image metadata에 최소 크기가 없어 Terraform 기본값만으로 PASS 처리하지 않음 |

정확한 버전의 존재를 확인했으며 `latest`로 대체하지 않았다. 세 지역 모두 동일한 결과였다. 배포 검토 직전 exact URN을 다시 조회하고 이미지의 최소 OS disk 요구를 공식 이미지 자료나 별도 읽기 전용 검증으로 해소해야 한다. 이미지 조회 성공은 VM allocation·guest 부팅·k3s 성공을 입증하지 않는다.

## 비용 비교와 사용 조건

**공개 USD retail, Linux 종량제, 할인·세금·학생 credit 차감 전**이다. Microsoft 공식 Retail Prices API를 2026-10-12에 직접 조회했고 `type=Consumption`, Linux `Virtual Machines …` product, 일반 SKU/meter를 선택했다. Spot·Low Priority·Windows·Cloud Services·예약 요금은 제외했다. API response의 `effectiveStartDate`는 요금 시작일이며 조회일과 다르다. USD 이외 환율과 실제 구독 청구액은 확인하지 않았다. [Retail Prices API 공식 설명](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices).

| 지역 / SKU | Compute USD/h | 730시간 compute USD | effectiveStartDate | 현재 배포 판단 |
| --- | ---: | ---: | --- | --- |
| Japan East B2pls_v2 | 0.0432 | 31.54 | 2023-11-01 | 첫 후보; 공통 gate 미해결 |
| Japan East B2ps_v2 | 0.0864 | 63.07 | 2023-11-01 | 메모리 8 GiB 대안; 같은 burst family |
| Japan East D2pls_v6 | 0.0798 | 58.25 | 2024-10-01 | family quota 0으로 차단 |
| Japan East D2pls_v5 | 0.0856 | 62.49 | 2023-07-01 | family quota 0으로 차단 |
| Korea Central D2pls_v6 | 0.0810 | 59.13 | 2025-09-01 | family quota 0으로 차단 |
| Japan West D2pls_v6 | 0.0800 | 58.40 | 2025-07-01 | family quota 0으로 차단 |

[공식 VM 가격 API 응답](https://prices.azure.com/api/retail/prices?api-version=2023-01-01-preview&currencyCode=USD&%24filter=serviceName+eq+%27Virtual+Machines%27+and+priceType+eq+%27Consumption%27+and+%28armRegionName+eq+%27koreacentral%27+or+armRegionName+eq+%27japaneast%27+or+armRegionName+eq+%27japanwest%27%29+and+%28armSkuName+eq+%27Standard_B2pls_v2%27+or+armSkuName+eq+%27Standard_B2ps_v2%27+or+armSkuName+eq+%27Standard_D2pls_v6%27+or+armSkuName+eq+%27Standard_D2pls_v5%27%29)을 근거로 위 단가를 기록했다. Korea Central·Japan West B2pls/B2ps의 일반 Linux VM 가격 응답도 없었다. 비용 숫자를 다른 region에서 가져와 채우지 않았다. 730시간은 비교용 가정이며 실제 월 시간·실행 기간에 따라 달라진다.

| 추가 항목 | 세 조사 지역의 공식 retail 결과 / 비용 적용 범위 |
| --- | --- |
| OS Standard SSD LRS 32 GiB, E4 LRS Disk | US$2.40/month (effective 2019-01-01); disk operations 별도 US$0.002/10K (2018-11-01) |
| NIC Standard IPv4 Static Public IP | US$0.005/hour (2018-06-01); 730시간 US$3.65 |
| Basic ACR, Basic Registry Unit | US$0.1666/day (2018-01-01); 저장 초과·transfer·Tasks 등 별도 |
| Internet egress, Asia / Microsoft premium network | 공식 페이지 first 100 GB/month 무료, 다음 10 TB/month US$0.12/GB; 실제 aggregate 사용량·청구 범위 확인 필요 |
| Remote state Storage / blob·transactions | backend 구성·사용량 미정으로 금액 미산정 |

[Japan East 추가 meter 공식 API 응답](https://prices.azure.com/api/retail/prices?api-version=2023-01-01-preview&currencyCode=USD&%24filter=armRegionName+eq+%27japaneast%27+and+%28productName+eq+%27Standard+SSD+Managed+Disks%27+or+productName+eq+%27IP+Addresses%27+or+productName+eq+%27Container+Registry%27%29+and+%28skuName+eq+%27E4+LRS%27+or+skuName+eq+%27Standard%27+or+skuName+eq+%27Basic%27%29)에서 product·sku·meter·단위를 선택했다. **B2pls_v2 compute + E4 base + PIP의 730시간 비교값은 US$37.59**다. 이는 전체 월 비용이 아니며 disk operations·egress·ACR·backend·세금·환율·credit·운영 기간을 제외한다. E4의 Disk Mount meter도 API에서 반환되었으나 공유 디스크 추가 mount용이므로 일반 inline VM OS disk 비용에 가산하지 않았다. [Managed Disks 가격 범위](https://azure.microsoft.com/en-us/pricing/details/managed-disks/), [Public IP 가격](https://azure.microsoft.com/en-us/pricing/details/ip-addresses/), [Bandwidth 가격](https://azure.microsoft.com/en-us/pricing/details/bandwidth/).

가격 재현용 공식 GET endpoint는 `https://prices.azure.com/api/retail/prices`이며 `api-version=2023-01-01-preview`, `currencyCode=USD`를 사용했다. 지역은 각 `armRegionName`, compute는 `serviceName='Virtual Machines'`, 정확한 `armSkuName`, `priceType='Consumption'`으로 필터했다. 추가 meter는 `Standard SSD Managed Disks / E4 LRS / E4 LRS Disk`, `IP Addresses / Standard / Standard IPv4 Static Public IP`, `Container Registry / Basic / Basic Registry Unit`의 이름·단위·type을 확인했다. 결과를 임의 가격표로 추정하지 않았고 원본의 선택된 공개 필드는 `/tmp/opsloop-azure-prices.json`, `/tmp/opsloop-azure-acr-prices.json`에만 보관했다.

[Students 공식 안내](https://azure.microsoft.com/en-us/free/students/)의 일반 credit 조건은 이 구독의 잔액·만료 증거가 아니다. 최초 조사에서는 잔여 credit, 만료일, spending limit, cost ceiling을 확인하지 못했다. **아래 2단계에서 spending limit은 On으로 확인했으며 잔여 credit·만료일·운영 비용 상한은 여전히 미확정**이다. VM stop/deallocate만으로 유지 중인 disk/PIP/ACR/backend 비용이 사라진다고 가정하지 않는다.

## 재사용 가능한 읽기 전용 survey / preflight

[스크립트](../../scripts/azure-arm64-survey.py)는 Python 표준 라이브러리와 기존 `az` 로그인만 사용한다. Azure command allowlist는 account show, provider list, vm list-usage/list-skus/image show다. `az account set`이나 provider register를 호출하지 않는다. 현재 구독 ID는 호출 범위 고정과 cache namespace hash에만 메모리에서 사용한다. credential/token을 조회하지 않으며 CLI stderr의 식별자도 문서/JSON에 복사하지 않는다.

```bash
python3 scripts/azure-arm64-survey.py --refresh --output .opsloop/azure-survey/latest.json
python3 scripts/azure-arm64-survey.py --region japaneast --zone 1 --refresh
python3 -m unittest discover -s tests -p 'test_azure_arm64_survey.py' -v
```

기본 범위는 Korea Central, Japan East, Japan West다. `--region`을 반복해 범위를 좁힐 수 있다. `--subscription`은 명시 구독을 조회하되 CLI default를 변경하지 않는다. exact image version을 요구하며 `latest` 입력은 거부한다. `--zone`을 생략하면 regional 검토이고 명시 시 SKU catalog와 zone restrictions를 모두 비교한다.

각 region의 quota/SKU/image를 한 번씩 조회하고 선택된 필드만 `.opsloop/azure-survey/`의 구독 hash별 cache에 저장한다. default TTL 900초, `--refresh`로 우회하며 cached 응답도 원래 `observed_at`을 유지한다. cache 위치는 git ignored이고 tracked source에 Azure API dump를 추가하지 않는다. account identity는 매 실행 새로 확인한다. Azure CLI 자체는 기존 인증을 사용하며 정상 동작 중 로컬 session metadata를 갱신할 수 있어 이번 환경에서는 샌드박스 밖 읽기 전용 명령 실행이 필요했다.

출력은 quota missing/zero, Location/Zone 제한, 2 vCPU/4 GiB minimum, ARM64/Gen2, accelerated NIC capability, image metadata·plan, disk minimum, provider 상태를 `pass/blocked/unknown`으로 구분한다. SKU 응답 부재는 `missing_skus`에 기록한다. unknown을 0 또는 무제한·성공으로 바꾸지 않는다. **`deployment_gate=false`, exit code 2는 알려진 차단 또는 미검증 항목이 남는다는 뜻**이다. catalog 검사 PASS만으로 actual capacity·policy·신용 잔액·backend 등을 검증할 수 없으므로 현 스크립트는 배포 성공 exit 0을 제공하지 않는다. 이를 테스트 실패나 Azure 인증 실패로 단정하지 않는다.

## 실제 검증과 남은 결정

- 라이브: 기존 Enabled/default 구독, 네 provider 상태, 세 지역 usage/SKU/exact image를 조회했다. 최초 `--refresh`와 캐시 재평가 모두 exit 2였고 위 차단·unknown 값이 기록됐다. 수정된 minimum_resources/accelerated_networking 검사는 캐시 재평가로 반영했다.
- 공식 자료: Microsoft Learn 사양·quota·Retail API 설명, Microsoft Azure 가격·Students 안내를 같은 날 열람했고 공개 Retail API 응답을 조회했다. SDK·패키지를 설치하지 않았다.
- 오프라인: [fixture tests](../../tests/test_azure_arm64_survey.py) 13개 통과. zero/missing quota, family case 비교, Zone/Location 제한 구분, image plan/arch/Gen2, disk unknown/too small, minimum memory, accelerated NIC, 미등록 provider, mutation allowlist 거부, cache 재사용/refresh, CLI 오류 비노출, 구독 ID 비저장과 계속 닫힌 gate를 확인했다.
- 남음: provider 등록 별도 승인, image 최소 disk 해소, actual subscription credit·expiry·cost ceiling, backend·ACR·OIDC/RBAC/policy·network/CIDR 확인, 별도 배포 승인과 native ARM 실험. Japan East에서 조회 근거를 얻었으므로 추가 region API를 확장하지 않았다.

배포 준비 계약은 [Azure ARM64 runbook](../runbooks/azure-arm64-deployment.md)에 연결한다. 본 보고서와 스크립트는 Terraform state/plan, cluster·Helm·Tailscale 또는 사용자 observability 값을 변경하지 않았다.

## 통합 작업 2단계 — 미해결 배포 조건만 확인

2026-10-12 **21:29 KST**의 구독·provider·권한·예산 조회와 후속 청구 속성 GET 결과다. 앞선 SKU/쿼터/이미지/가격 및 클러스터 관측을 재사용하고 해당 대량 조회는 반복하지 않았다. 읽기 전용 명령 실행에는 기존 Azure CLI의 로컬 session metadata 접근을 위한 sandbox 외 실행 승인을 사용했다. Azure 리소스 변경 권한이나 배포 승인을 요청·사용한 것은 아니다.

| 조건 | 실제 조회 결과 | 남은 조건 |
| --- | --- | --- |
| 학생 구독 | Enabled; offer `AzureForStudents_2018-01-01`; `spendingLimit=On` | 잔여 credit·만료일 미확인; On은 사용 가능 잔액을 뜻하지 않음 |
| Provider | Network / Storage / ContainerRegistry 모두 NotRegistered | 별도 명시적 등록 승인 필요; 자동 등록 금지 유지 |
| Subscription budgets | Budgets List 응답 `value=[]`, next page 없음 | 설정된 budget 없음; 잔여 credit/사용액 0으로 해석하지 않음 |
| ARM 권한 | subscription Permissions GET의 grant가 RG·VM·VNet·NIC·NSG·PIP·Storage·ACR 생성 및 role assignment action을 허용 | 실제 쓰기 시험 없음; deny assignment·조건·Azure Policy·backend data-plane 미검증 |
| 청구 연결 | Subscription Billing Property GET 성공; 연결된 Microsoft Customer Agreement billing profile 확인 | Available Balance GET에 `properties.amount` 없음; 0으로 대체하지 않으며 학생 credit 잔액·만료 미확인 |
| Backend | partial `azurerm` 선언·Entra/CLI 인증 example·runbook 존재; 이전 Storage count=0 근거 재사용 | 계정/container 실값, 네트워크 접근, Blob Data Contributor, 잠금·복구 절차 미준비/미검증 |
| Bootstrap | Terraform에 auth key/join token/custom-data 없음; agent 예제는 `token-file` 사용 | 초기 VM 접속 경로, Tailscale device 승인/ACL, 보호된 token 전달, ARM64 installer/checksum 및 설치 절차 실행 승인 필요 |

권한 조회는 read-only로 반환된 `actions/notActions`만 비교했다. subscription scope의 management-plane grant를 backend Blob 접근권이나 실제 resource 생성 성공으로 확대하지 않는다. 청구 account 목록의 다른 profile을 임의로 조회하지 않고 현재 구독의 Billing Property가 연결한 profile만 조회했다. 실 구독·tenant·billing identifiers 및 CLI raw stderr는 출력하거나 Git에 저장하지 않았다. 요약 JSON은 ignored `.opsloop/azure-survey/prerequisites-latest.json`(0600)에만 저장했으며 인증정보는 포함하지 않는다.

[Subscription policies GET](https://learn.microsoft.com/en-us/rest/api/resources/subscriptions/get?view=rest-resources-2022-12-01), [Subscription budgets GET](https://learn.microsoft.com/en-us/rest/api/consumption/budgets/list?view=rest-consumption-2024-08-01), [Subscription Billing Property GET](https://learn.microsoft.com/en-us/rest/api/billing/billing-property/get?view=rest-billing-2024-04-01), [Billing-profile Available Balance GET](https://learn.microsoft.com/en-us/rest/api/billing/available-balances/get-by-billing-profile?view=rest-billing-2024-04-01)을 사용했다. Available Balance는 MCA 청구서 정산용 credit/payment balance API이므로 값이 있더라도 학생 구독의 남은 프로모션 credit로 자동 해석할 수 없다.

### 정확한 이미지와 OS disk의 확인 한계

기존 exact URN `Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040`의 Arm64/V2/plan-null 결과를 유지한다. [Canonical 공식 mapping](https://ubuntu.com/azure/docs/azure-how-to/instances/find-ubuntu-images/#ubuntu-24-04-lts-noble-numbat)도 Ubuntu 24.04 Server ARM64 Gen2에 `server-arm64`를 사용한다. [Microsoft VM Images GET schema](https://learn.microsoft.com/en-us/rest/api/compute/virtual-machine-images/get?view=rest-compute-2025-04-01#osdiskimage)의 `OSDiskImage`는 `operatingSystem`만 정의하므로 같은 CLI 명령 반복으로 이 exact build의 최소 disk 크기를 얻을 수 없다.

[Microsoft 일반 Ubuntu Server 이미지 예시](https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks#size-requirements)는 약 30 GiB OS image를 설명하지만 해당 ARM64 exact build의 증거는 아니다. **32 GiB Standard SSD는 계획 입력이며 exact image 최소 요구를 검증한 PASS가 아니다.** 공식 이미지의 해당 build 크기 근거를 확보하거나, 이후 별도 승인된 검증에서 해소하기 전 gate를 열지 않는다. 현재 작업에서 image용 VM/disk를 생성하지 않았다.

### Japan East 계획 입력·비용·결론

이번 배포 검토 후보는 `location=japaneast`, `vm_size=Standard_B2pls_v2`, 위 exact image, `os_disk_size_gb=32`(호환성 미확인), `acr_sku=Basic`다. VNet/Subnet 및 Tailscale/k3s 책임 경계를 유지한다. Terraform 기본 D2pls_v6를 변경하지 않았으므로 [runbook의 plan 명령](../runbooks/azure-arm64-deployment.md#4-계획-검토와-승인된-배포)에 Japan East/B2pls pair를 명시했다. `deployment_prerequisites_confirmed=false`를 유지했다.

기존 같은 날 공식 단가를 재사용한 **비교 가정**은 compute 730시간 US$31.536 + E4 기본 US$2.40 + PIP 730시간 US$3.65 + ACR 30일 US$4.998 = **US$42.58**다. 730시간과 30일을 조합한 비교값이며 달력 월 청구 총액이 아니다. backend·disk operations·egress·추가 저장·세금·환율은 제외한다. 24시간씩 30일 유지하는 동일 기본 항목은 compute US$31.104 + E4 US$2.40 + PIP US$3.60 + ACR US$4.998 = **US$42.10/30일**(약 US$1.40/일)이며 추가 사용료가 늘 수 있다. 실험 시간 단축과 승인된 cleanup 시점을 비용 계획에 반영한다.

[Students 일반 조건](https://learn.microsoft.com/en-us/azure/cost-management-billing/manage/azurestudents-subscription-disabled)은 US$100/12개월이며 이는 현재 잔액이 아니다. 실제 사용 가능한 잔액을 `R` USD로 확인한 후 위 30일 비교값 기준 유지 가능 기간을 `30 × R / 42.102`일 이하로 추산하되, credit 만료일과 다른 사용료를 함께 적용한다. 실제 잔액·만료 확인 전 지속 운영을 승인할 비용 근거가 부족하다. credit/기간 소진 시 구독이 비활성화될 수 있어 recovery candidate 가용성에도 영향을 준다.

**현재 Azure 배포 불가: 조건부 후보를 준비했으나 provider·exact image disk·backend·credit/budget·bootstrap/정책 조건이 남는다.** 실제 capacity는 읽기 전용 조회로 보장할 수 없으며 이후 승인된 allocation 시 결과를 분리 기록한다. 이번 단계에는 backend init 연결, Terraform plan/apply/destroy, provider 등록 및 VM/network/storage/ACR 생성·변경을 실행하지 않았다. P1/P2 전체 완료 상태를 변경하지 않았다.

## 2026-10-13 Phase A/B/C 후속 확인

Azure snapshot은 **2026-10-13 01:19 KST**(2026-10-12 16:19 UTC), 이후 정책 조건을 추가 검토했다. 대상 commit은 `4a4abd9`, `f26c125`다. 기존 다중 지역 SKU/쿼터/가격 조사는 반복하지 않고 현재 Japan East SKU restrictions·exact image·provider·spending limit·budget·정책·deny 및 backend에 필요한 기존 Storage 유무를 확인했다.

### 새로 확정한 지역 정책 차단

subscription scope에 적용되는 정책 1개가 `enforcementMode=Default`, `effect=deny`, `listOfAllowedLocations`를 사용한다. assignment의 실 파라미터와 definition 조건을 읽고 Japan East VM에 적용됨을 확인했다. 허용 지역은 다음뿐이다.

```text
indonesiacentral
polandcentral
eastasia
norwayeast
malaysiawest
```

definition의 `allOf`는 location이 허용 목록 밖이며 global이 아니고, type이 `Microsoft.AzureActiveDirectory/b2cDirectories`가 아닌 경우를 검사한다. VM은 type 예외가 아니다. 따라서 **Japan East B2pls_v2 VM은 활성 정책에 의해 차단**된다. Korea Central과 Japan West도 허용 목록 밖이다. 현재 구독 scope의 deny assignments 응답은 0개였지만 Azure Policy deny는 별개이며 이를 서로 상쇄하지 않는다. 정책/RBAC를 변경하거나 우회하지 않았다.

Japan East SKU 조회는 ARM64, 2 vCPU, 4 GiB, Hyper-V `V1,V2`, accelerated networking True, restrictions `[]`, zone `[1]`를 반환했다. exact Ubuntu image는 Arm64/V2/Linux/plan null로 계속 조회됐다. 이 두 조회 성공은 **정책상 배포 가능성이나 capacity를 입증하지 않는다**. 기존 쿼터 수치(2026-10-12 regional 0/6, Bpsv2 family 0/10)를 재사용했으며 최신 실측으로 날짜를 바꾸지 않았다.

### 나머지 준비 상태 및 검증

- Provider: Compute Registered; Network/Storage/ContainerRegistry NotRegistered. 필요한 별도 승인 목록은 Network(VNet/subnet/NSG/NIC/PIP), Storage(remote backend), ContainerRegistry(기존 root의 Basic ACR)다. 자동 등록은 계속 금지한다.
- 구독: Azure for Students Enabled, spendingLimit On, configured budgets 0개. 이전 청구 API로 확인되지 않은 학생 credit 잔액·만료일을 성공으로 바꾸지 않고 동일한 조회를 반복하지 않았다.
- Backend: 현 구독 Storage account 0개, ACR 0개. 예상 private 경로 `~/.config/opsloop/private/`의 backend.hcl/p1.tfvars가 없고 활성 `.terraform/terraform.tfstate` backend metadata도 없다. 운영자가 별도로 지정한 다른 구독 backend는 확인되지 않았다. Entra/CLI 인증과 container 범위 Storage Blob Data Contributor, 접근 제한·blob 잠금·복구 준비가 필요하다.
- Image/disk: 고정 version 재확인, 최소 OS disk 요구는 여전히 unknown. API schema의 크기 필드 부재와 일반 Ubuntu 30 GiB 설명을 exact ARM build 증거로 확대하지 않는다. 32 GiB E4는 조건부 비용 가정이다.
- Security: 기존 Terraform의 NSG deny-all(priority 200), optional 단일 public IPv4 /32 SSH(priority 100), password off, ACR admin/anonymous off를 유지했다. 6443/10250/8472/NodePort를 인터넷에 허용하는 rule은 없다. 실제 VM/NSG가 없으므로 배포된 effective rules를 검증했다고 주장하지 않는다.
- Bootstrap: Terraform에 개인키/auth key/join token을 넣지 않는다. 보호된 SSH key 파일, 별도 Tailscale 승인, root 전용 k3s token-file 및 ACL 전달 절차가 필요하다. 승인 없이 설치/인증/방화벽 변경을 실행하지 않았다.
- Local validation: Terraform 1.15.9 / AzureRM 4.64.0의 fmt, init -backend=false -input=false -lockfile=readonly, validate 통과. init/validate sandbox 제한은 승인된 외부 실행으로 해소했다. 기존 source/23개 테스트의 변경이 없으므로 테스트를 반복하지 않았다. remote backend init과 plan은 실행하지 않았고 plan 파일도 만들지 않았다.
- Costs: 2026-10-12 공식 단가와 계산을 그대로 재사용했다. 30일 720h 기준 VM US$31.104, E4 US$2.40, PIP US$3.60, Basic ACR US$4.998, 합계 US$42.102. backend/operations/egress/세금/환율/추가 사용료 제외이며 지금 Japan East에 배포할 수 있다는 뜻이 아니다. 허용 지역으로 변경하면 가격/SKU/쿼터/이미지를 해당 지역에서 별도로 확인해야 한다.

GitHub 공개 HTTPS 조회의 remote main은 `52e1c47368a1b748219b1c1196ab4a2dbffff182`, 로컬 HEAD는 `f26c1258f4e2b278cbf75f43290d6e2e680afd9d`다. 두 commit이 fast-forward 가능한 차이이며 push 대상 27파일의 두 commit 버전(54개 blob)을 점검해 credential/state를 발견하지 않았고 사용자 values hash도 일치했다. 최종 검증 뒤 승인된 `git push origin main`을 실행했으나 exit 128, `Permission denied (publickey)`로 실패했다. SSH agent가 현재 세션에 없고 ssh-askpass도 없으며 gh는 미로그인이다. remote/auth 설정은 바꾸지 않았다. 이 후속 문서 변경은 아직 commit/push하지 않았다.

다음 순서는 GitHub 인증 준비 → 기존 두 commit push와 remote SHA 확인, 별도로 **허용 지역 ARM64 후보 선택/검증 또는 승인된 정책 허용 경로 확보** → provider 등록 승인 → backend 구성/RBAC 승인 → private 입력·비용/credit·bootstrap 확인 → 조건부 plan 검토다. 기존 Japan East plan 명령을 그대로 실행하지 않는다.
