# P1-C — Azure ARM64 후보 조사

기준일: **2026-10-12**, 라이브 CLI 관측 17:22~17:24 KST 및 같은 날 공식 가격·사양 조회. 대상은 기존 기본 구독 **Azure for Students / Enabled**다. 구독 ID·tenant·토큰·자격 증명을 문서와 조사 캐시에 저장하지 않았다. Azure 리소스 생성·삭제·provider 등록·쿼터 요청은 수행하지 않았다.

## 결정에 대한 답

**현재 읽기 전용 근거로는 Japan East / Standard_B2pls_v2가 첫 후보**다. 2 vCPU / 4 GiB / ARM64 / Gen2 / Accelerated Networking 지원, 지역·family 쿼터 여유와 빈 SKU restrictions를 함께 확인했다. 이는 배포 허가나 allocation 성공 보장이 아니다. Network·Storage·ContainerRegistry 미등록, OS 이미지 최소 디스크 크기 미확인, 실제 capacity·RBAC/policy·잔여 credit·backend·비용 상한이 남아 **배포 gate는 닫혀 있다**.

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

[Students 공식 안내](https://azure.microsoft.com/en-us/free/students/)의 일반 credit 조건은 이 구독의 잔액·만료 증거가 아니다. **잔여 credit, 만료일, spending limit, cost ceiling은 미조회/미확정**이다. portal에서 해당 구독의 실제 값을 확인하고 실험 유지 시간·비용 상한·cleanup 담당자를 정해야 한다. VM stop/deallocate만으로 유지 중인 disk/PIP/ACR/backend 비용이 사라진다고 가정하지 않는다.

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
