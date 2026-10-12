# P1 platform preparation runbook

기준: [Architecture v0.1](../architecture/architecture-v0.1.md). 환경 근거: 과거 [P1-A Inventory](../experiments/p1-a-environment-inventory.md), 최신 [P1-B 로컬 검증](../experiments/p1-b-local-platform.md), [관측 검증](../experiments/p2-observability-verification.md), [Azure 후보](../experiments/p1-c-azure-arm64-candidates.md).

## 현재 상태 / 실행 경계

2026-10-12 인계 기준 로컬 VM Control Plane과 Physical Worker는 이미 구축되었고 관측 스택도 실행 중이다. 이번 읽기 전용 조회에서 두 노드 Ready·AMD64·k3s `v1.35.9+k3s1`·tailnet IP를 재확인했다. **Azure ARM64 추가 배포 gate는 닫혀 있고 P1 플랫폼 DoD는 미완료다.** provider 등록·quota 최신 상태는 별도 Azure 조사 기록을 따르며 remote backend·ARM worker·ACR/OIDC·전체 네트워크/복구 시험이 남는다. Terraform `deployment_prerequisites_confirmed=false`를 기본으로 유지한다. 이를 true로 바꾸는 것 자체가 배포 승인 또는 quota 검증을 대신하지 않는다.

이번 로컬 IaC 검증에는 아래 세 명령을 사용한다. 클러스터·Helm·Azure 조사 스크립트의 GET/list/show 조회도 허용 범위다. 실제 생성·등록·변경·삭제는 승인 전 실행하지 않는다. `init -backend=false`는 provider와 lockfile을 준비하며 Azure backend 및 provider 등록을 수행하지 않는다. `validate`는 변수의 실제 값, quota, Azure policy, 네트워크 연결을 검증하지 않는다.

```bash
terraform -chdir=infra/terraform/p1 fmt -check -diff
terraform -chdir=infra/terraform/p1 init -backend=false -input=false
terraform -chdir=infra/terraform/p1 validate
```

검증 도구 고정: Terraform 1.15.9, AzureRM 4.64.0, `.terraform.lock.hcl` 유지. provider 자동 등록은 `none`, 명시 등록 목록은 빈 목록이다. 업데이트는 별도 호환성 검토 후 수행한다.

## 책임 / secret 경계

| 담당 | 관리 내용 | 금지 입력 |
| --- | --- | --- |
| Terraform testbed | RG, VNet, private Subnet, NSG, static Standard PIP, accelerated NIC, ARM VM, VM inline managed OS disk, Basic ACR | Tailscale auth key, k3s token, SSH private key, ACR token, Azure client secret, kubeconfig |
| 별도 backend bootstrap | state storage/container/RBAC/보호·복구, 별도 root/state | access key·SAS를 backend 파일 또는 Git에 기록 |
| node bootstrap operator | Ubuntu/시간/OS firewall, Tailscale 등록, k3s 고정 version·설정·token 파일, registry pull 인증 | Terraform provisioner/custom_data/cloud-init에 secret 전달 |
| 후속 CI identity operator | 보호된 main OIDC와 scoped ACR push; node read-only pull token 별도 | PR branch push 권한, ACR admin 계정 |

현재 Terraform에는 provisioner, cloud-init, random password, role assignment, ACR token 리소스가 없다. VM에는 공개 SSH key만 입력하고 password 인증을 끈다. ACR admin/anonymous pull을 끈다. state/plan을 source와 분리하고 `.gitignore`만으로 secret 보호를 주장하지 않는다. 미래 provider resource 추가 시 computed 민감 attribute가 state에 들어가는지도 검토한다.

Bootstrap 인증은 운영자가 승인된 방식으로 별도 처리한다. Tailscale은 가능하면 대화형 장치 승인을 사용한다. auth key가 필요하면 Terraform 밖의 보호된 전달 경로를 사용하고 shell history·argv·로그에 남기지 않는다. k3s token 및 ACR pull credential은 필요한 노드의 root 전용 파일(0600)에만 후속 단계에서 배치한다. 이 단계에서 secret 파일은 생성하지 않는다.

## P1-B 선행 확인 — 먼저 완료

1. 기존 Control Plane `opsloop-control` (tailnet `100.73.4.76`, NAT `192.168.122.102`)과 Physical Worker `sarmubuntuserver` (tailnet `100.82.241.77`, LAN `192.168.45.168`)의 구축 결과를 재사용한다. Arch Host `archgram` (`100.80.16.28`)은 Node가 아니다. 확인된 기존 SSH 경로에서 known host 검증을 유지한 읽기 전용 조회로 NTP·route·전원 복구 경로를 확인한다. 광범위 LAN 스캔 또는 host key 자동 수락을 하지 않는다. Physical의 기존 Docker/MySQL/애플리케이션 및 UFW를 초기화·재설치하지 않는다.
2. 비용 상한·운영 기간·잔여 Students credit/만료·cleanup 책임을 정한다. 사용자 승인 후 필요한 resource provider를 등록한다. 이 작업은 Azure 변경이다.
3. 아래 읽기 전용 조회 또는 `python3 scripts/azure-arm64-survey.py --help`의 조사 명령을 재실행한다. regional total과 SKU family에서 각각 `limit-currentValue >= 2`임을 확인한다. Korea Central의 D2pls_v6 family는 `StandardDplsv6Family`이며 최신 limit는 0이다. Japan East B2pls_v2 대안의 family는 `standardBpsv2Family`이고 최신 limit 10, regional total limit 6이다. 조회 성공은 실제 capacity·최소 image disk·policy 통과를 보장하지 않는다. 응답 `[]`는 중단 조건이다. 등록 후에도 빈 경우 Portal Usage+quotas/API 또는 Azure 지원 경로로 확인한다. region 목록에 있다는 것만으로 student 정책 허용을 단정하지 않는다.

```bash
az provider show -n Microsoft.Compute --query registrationState -o tsv
az provider show -n Microsoft.Network --query registrationState -o tsv
az provider show -n Microsoft.ContainerRegistry --query registrationState -o tsv
az vm list-usage --location koreacentral -o table
az vm list-skus --location koreacentral --size Standard_D2pls_v6 --all --query "[?name=='Standard_D2pls_v6'].{name:name,family:family,restrictions:restrictions}" -o json
az vm image show --location koreacentral --urn Canonical:ubuntu-24_04-lts:server-arm64:24.04.202609040 --query '{version:name,arch:architecture,generation:hyperVGeneration,plan:plan}' -o json
```

4. SKU Location/Zone 제한을 구분한다. Korea Central D2pls_v6의 zone 2는 제한 대상이다. Japan East B2pls_v2의 별도 SKU/zone 정보를 적용하며 기본 Terraform은 zone을 지정하지 않는다. quota가 있어도 실제 capacity는 배포 때 실패할 수 있다. 대안 region/SKU도 별도 quota·image·가격 재검사가 필요하다.
5. 각 노드와 기존 Azure/tailnet/VPN 경로에 [CIDR plan](../experiments/p1-a-environment-inventory.md#cidr-plan)을 대조한다. `10.80/16`, `10.42/16`, `10.43/16`은 축약 표현이며 실제 설정은 표의 완전한 CIDR을 사용한다.
6. 실제 ACR 이름을 선정해 가용성을 read-only 확인하고 RG 생성·network·VM·ACR 권한과 subscription policy를 확인한다. OIDC/role assignment는 별도 승인 범위다.
7. state backend를 먼저 별도 root로 준비한다. Entra/Azure CLI 기반 접근, 최소 RBAC, 잠금, private 보관 정책, versioning/복구·보존을 정하고 bootstrap state를 testbed와 분리한다. testbed destroy가 backend를 삭제하지 않도록 한다.

## P1-B Terraform 실행 — 별도 승인 이후에만

`infra/terraform/p1/terraform.tfvars.example`은 non-secret 템플릿이다. UUID, region, 글로벌 고유 ACR 이름과 **공개** RSA key를 운영자 로컬 설정으로 채운다. 확인된 Ubuntu exact version을 재확인하고 `latest`는 사용하지 않는다. `bootstrap_ssh_source_cidr=null`이면 public SSH도 닫힌다.

testbed root에는 Entra/Azure CLI 인증을 사용하는 partial `azurerm` backend 선언과 비밀값 없는 설정 예제가 준비되어 있다. 실제 backend Storage/container/RBAC·보호/복구는 별도 준비 대상이다. [Azure 배포 준비 runbook](azure-arm64-deployment.md)의 절차로 승인된 backend 설정을 검토하고 인증은 실행 환경으로 공급한다. backend 준비 없이 plan/apply를 실행하지 않는다. 선언/인증/RBAC까지 확인된 뒤 `init` 및 plan을 실행하며 plan artifact는 source 밖 제한 권한 경로에 보관한다. 실제 변경 목록·가격·승인된 subscription을 검토한 뒤에만 **추가 사용자 승인을 받아 apply**한다. P1-A에서는 plan도 실행하지 않았다.

Terraform VM `os_disk`가 managed OS disk를 관리한다. 임시 디스크는 datastore 용도로 사용하지 않는다. 기본 OS disk는 32 GiB Standard SSD LRS다. PIP는 private subnet VM의 명시적 outbound 경로이며 NSG inbound는 deny-all이다. [Azure 공식 outbound 설명](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access).

## P1-B node bootstrap — Terraform와 별도 승인

1. 초기 VM 관리는 운영자 현재 public IPv4 `/32`만 임시 SSH 허용하는 방식을 승인받아 사용한다. LAN 사설 IP를 넣지 않는다. Azure의 관리 경로를 대안으로 쓰려면 별도 권한/실행 승인이 필요하다. Tailscale 관리 접속을 검증한 뒤 임시 SSH 예외를 제거한다.
2. Ubuntu patch/time/disk/hostname, Host 절전 방지와 Physical 복구 경로를 확인한다. OS firewall을 끄는 방식 대신 `tailscale0`와 필요한 Pod forwarding 경로로 규칙을 제한한다.
3. 각 Ubuntu 노드를 승인된 tailnet에 등록한다. `tailscale0`의 실제 IPv4를 node-ip로 사용하고 서버/agent config에서 `flannel-iface=tailscale0`를 명시한다. Pod/Service CIDR subnet route는 광고하지 않는다.
4. 기존 로컬 k3s `v1.35.9+k3s1`과 동일 버전으로 Azure agent를 준비한다. Arch의 OpsLoop 전용 `~/.local/bin/kubectl-opsloop --kubeconfig ~/.kube/opsloop.yaml`을 사용하고 기본 kubectl의 지원 skew는 별도로 검토한다. Helm `4.3.0`·기존 chart `91.8.1`은 실행 결과와 함께 기록한다. [server example](../../bootstrap/nodes/k3s-server.yaml.example), [agent example](../../bootstrap/nodes/k3s-agent.yaml.example)을 노드별로 채운다. server는 SQLite 단일 control plane이다. token은 Terraform 밖 root-only token-file로 공급한다. 설치 스크립트·고정 binary checksum과 재실행 멱등성은 실제 설치 전 검토한다.
5. tailnet ACL과 OS firewall에서 노드 간 TCP 6443(server), TCP 10250 및 UDP 8472(VXLAN)를 필요한 source/destination으로만 허용한다. Host 관리 SSH 및 Demo의 선정 NodePort, 관측 port는 역할별로 별도 제한한다. UDP 8472, Kubernetes API, kubelet, NodePort를 Azure public NSG에 열지 않는다. peer identity·ACL 권한을 먼저 확인한다. [K3s requirements](https://docs.k3s.io/installation/requirements).
6. ACR repository read-only pull credential 발급·만료/교체를 별도 수행한다. Terraform state/Git에 credential을 넣지 않는다. GitHub OIDC는 Node pull 인증을 대체하지 않는다.

## P1-B 수용 검증 / cleanup

- 세 Node Ready, Ubuntu/amd64/arm64, unique hostname와 실제 hostname label, server SQLite 상태를 대조한다. Host는 node 목록에 없어야 한다. Traefik·ServiceLB 비활성화와 관련 기본 Pod/host port 80·443 점유가 없는지도 확인한다.
- direct/relay, RTT, 노드별 underlay MTU, 실제 flannel MTU, 양방향 Pod 통신/DNS/큰 payload, Host 고정 NodePort HTTP 경로를 확인한다. `tailscale0` MTU 1280 관측을 고려하되 임의 MTU 변경으로 성공 처리하지 않는다.
- public endpoint에서 SSH 예외 종료 후 22/6443/10250/8472/선정 NodePort의 비노출, 노드 outbound HTTPS/ACR pull을 검증한다. NSG와 OS firewall/ACL은 서로 다른 경계다.
- ARM worker에는 관측 stack/engineering build를 배치하지 않는다. Laptop VM 4 vCPU/8 GiB 시작 구성은 실제 가용 자원에 맞춰 확인한다.
- SQLite backup/restore와 k3s token 복구, backend state 복구 및 재생성 절차를 검증한다. 전원 종료/partition과 서비스 복구 실험은 후속 Phase다.
- VM 정지만으로 disk/PIP/ACR 청구가 종료되지 않는다. 별도 승인된 destroy 계획에서 testbed만 제거하고 backend 보존·Tailscale device 폐기·pull token 폐기·OIDC 정리를 각 담당 경계에서 수행한다. 임의 전체 RG 삭제 또는 backend 삭제를 하지 않는다.

## 비용 / 승인 대상

P1-A에서는 과금 리소스를 만들지 않았다. P1-B 비용 승인 대상은 ARM VM 사용 시간, 32 GiB managed SSD, Standard static Public IP, ACR SKU/저장량, 인터넷 outbound 전송, 별도 backend Storage다. 실제 가격·통화·세금·student credit 적용을 확인하지 않았으므로 월 비용을 추정 수치로 확정하지 않는다. 예산과 자동 종료/cleanup 시점을 배포 전에 정한다. deallocate는 compute 비용만 줄일 수 있고 나머지 리소스 비용은 남는다.

사용자 승인 대상은 Azure provider 등록·backend/리소스 생성 및 apply, Tailscale 장치 등록/ACL 변경, OS firewall·절전 설정, k3s 설치, ACR credential 발급·GitHub OIDC/RBAC/보호 설정, 삭제/cleanup이다. 현재 read-only 조회 및 로컬 파일 준비 승인을 이 변경들의 승인으로 확대하지 않는다.

## 과거 P1-A 검증 기록

2026-10-12: Terraform 1.15.9 / AzureRM 4.64.0에서 `fmt -check -diff`, `init -backend=false -input=false`, `validate` 통과. init·validate는 sandbox의 plugin socket 제한 때문에 승인된 외부 실행으로 검증했다. plan/state/apply는 실행·생성하지 않았다. CIDR 계산 15쌍 비중첩과 subnet containment, 로컬 문서 링크, 변경 파일 whitespace 검사도 통과했다. reviewer의 Traefik·ServiceLB 비활성화 누락 지적을 server 예제와 수용 검증에 반영했다. quota/capacity/backend/실제 network/bootstrap 검증은 미수행이며 P1 DoD가 아니다.


## 기존 로컬 환경의 읽기 전용 재검증

```bash
~/.local/bin/kubectl-opsloop --kubeconfig ~/.kube/opsloop.yaml get nodes -o wide
python3 scripts/verify-platform.py --help
python3 scripts/azure-arm64-survey.py --help
```

검증 스크립트는 관리자 kubeconfig 본문·Secret·raw Helm values를 출력하지 않는다. 클러스터 변동과 접근 실패는 실패 사유로 보고한다. 현재 관측 PVC/Targets/values와 스크립트 사용법은 [관측 기록](../experiments/p2-observability-verification.md)을 따른다. 사용자 기존 실험은 Control Plane Pod → Worker Pod HTTP/DNS 성공이며, 테스트 namespace 삭제 이후 이번에는 Pod를 생성해 재실행하지 않는다.

Docker FORWARD DROP와 libvirt NAT 충돌은 사용자 기록에서 `DOCKER-USER` VM 네트워크 허용으로 해소되었다. `opsloop-libvirt-forwarding.service`의 active·enabled 및 unit 설정은 재확인했다. 현재 kernel iptables 규칙은 `sudo -n` 권한 부족으로 미확인이다. unit 상태만으로 실제 영구 규칙 적용·재부팅 복구 완료를 주장하지 않는다. libvirt VM autostart도 disabled이므로 Host 재부팅 뒤 기동 절차를 별도 확인한다. 해결 확인을 위해 firewall/service를 변경하지 않는다.

위 P1-A 검증 기록은 당시 사실로 유지한다. 이번 Terraform 검증 결과·backend 차단 조건은 [Azure 준비 절차](azure-arm64-deployment.md), 실제 로컬 환경은 [P1-B 기록](../experiments/p1-b-local-platform.md)을 따른다.

## 이번 통합 검증 기록

2026-10-12: 최종 `fmt -check -diff`·`validate` 통과, `init -backend=false -input=false -lockfile=readonly` 통과. 최종 validate의 sandbox provider handshake 제한은 동일 명령의 허용된 외부 실행으로 확인했다. backend 연결·plan·apply·destroy는 실행하지 않았다.

`python3 -m unittest discover -s tests -p 'test_*.py' -v`로 fixture **23개 통과**. 최종 클러스터 검증은 **08:31:02 UTC / 17:31:02 KST PASS**: Node 2개, monitoring Pod 7개, Targets 17개, Helm allowlist 36개 불일치 0. Azure Japan East 캐시 재평가는 기존 관측 시각을 유지하며 exit 2, gate=false: B2pls/B2ps의 SKU·regional/family quota·이미지·NIC 조건 pass, image 최소 disk unknown, 미등록 provider blocked. 새로운 cloud allocation 또는 이전 Pod probe 재실행 결과가 아니다.

로컬 문서 링크·Python 구문·bootstrap YAML·`git diff --check` 통과. libvirt NAT 포함 주소 21쌍 비중첩·subnet containment를 정적으로 확인했다. 사용자 values SHA256, P1-A Inventory와 Terraform lockfile 원문을 보존했고 P1 root의 state/plan 산출물은 없다. 독립 검토에서 확인한 YAML 오류 원문 노출 경로를 고정 JSON 오류 처리와 회귀 테스트로 해결했으며 최종 미해결 확정 결함은 없었다. backend·capacity·secret 보관·실제 네트워크/장애/backup 검증은 별도 범위로 남는다.
