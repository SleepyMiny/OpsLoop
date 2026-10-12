# P1-B 로컬 플랫폼 확인 — 2026-10-12

기준: [Architecture v0.1](../architecture/architecture-v0.1.md), ADR-001/005. 후속 읽기 전용 확인이며 [P1-A 조사](p1-a-environment-inventory.md)를 당시 기록으로 보존한다. **로컬 AMD64 2-Node가 구축되어 있지만 Azure ARM64 노드는 없으므로 P1 전체 DoD는 미완료**다. 이 작업에서 설치·설정 변경·새 probe Pod 생성·삭제는 수행하지 않았다.

## 현재 장비와 클러스터

노드 관측 기준 시각은 **2026-10-12 08:27 UTC / 17:27 KST**다. 기존 인증을 사용하는 `kubectl-opsloop`로 API GET만 실행했다. kubeconfig 내용을 읽거나 출력·복사하지 않았다.

| 대상 | 확인된 상태 | 근거·한계 |
| --- | --- | --- |
| Host `archgram` | Tailscale `100.80.16.28`, BackendState Running, Kubernetes Node 아님 | Host는 관리·VM 실행 경계 |
| VM `opsloop-control` | Ubuntu 24.04.5 LTS, AMD64, `100.73.4.76`, Ready | libvirt `192.168.122.102`; virsh에서 running·persistent, 4 vCPU / 8 GiB 확인 |
| Physical `sarmubuntuserver` | Ubuntu 24.04.4 LTS, AMD64, `100.82.241.77`, Ready | CPU 4 / memory `8072460Ki` allocatable; i5·8 GiB는 사용자 제공 inventory |
| k3s / runtime | 양쪽 `v1.35.9+k3s1`, `containerd://2.2.7-k3s1` | exact version 일치, hostname label 일치 |
| 자원·상태 | 두 노드 schedulable, Memory/Disk/PID pressure False | VM allocatable CPU 4 / memory `8127240Ki`; 지속 부하·OOM·eviction 검증 아님 |
| system Pods | CoreDNS 1.14.7, metrics-server 0.9.0, local-path-provisioner 0.0.37 | 3개 Running/Ready, restart 0; Traefik·ServiceLB Pod 없음 |
| overlay | 노드 metadata의 VXLAN 및 tailnet InternalIP 확인 | 설정 metadata는 실제 패킷 경로·MTU 검증을 대신하지 않음 |

Host Tailscale metadata에서 control peer는 online·active, 현재 주소 `192.168.122.102:41641`를 반환했다. Physical peer는 online이며 현재 direct 주소는 비어 있고 relay 지역 메타데이터는 `tok`였다. 이 조회만으로 **노드 간 direct/relay 경로나 성능을 확정하지 않는다**. Tailscale ACL이나 방화벽·라우팅은 변경하지 않았다.

## VM 시작과 Docker forwarding

`libvirtd` active, `virtqemud` inactive, `tailscaled` active를 확인했다. 기존 VM은 persistent지만 **Autostart disable**다. Host 재부팅 후 VM 자동 기동을 검증하거나 활성화하지 않았다.

기존 `opsloop-libvirt-forwarding.service`는 **active / enabled**다. unit은 `Requires`·`After`·`PartOf=docker.service`, `WantedBy=docker.service`이며 oneshot `RemainAfterExit=yes`로 `/usr/local/sbin/opsloop-libvirt-forwarding`을 실행한다. 해당 스크립트에는 `DOCKER-USER`의 `virbr0` / `192.168.122.0/24` 출발 ACCEPT와 돌아오는 `ESTABLISHED,RELATED` ACCEPT 규칙을 존재 검사 후 추가하는 로직이 있다. unit과 로직은 읽기 전용으로 확인했다.

현재 규칙을 확인하려던 `sudo -n iptables -S DOCKER-USER`는 **password required**로 거절되었다. 따라서 커널에 실제 적용된 규칙·재부팅 후 유지·Docker 재시작 후 재적용·연결 복구는 미검증이다. 서비스 active/enabled만으로 지속성이 완료됐다고 판정하지 않는다. 재시작이나 방화벽 변경을 실행하지 않았다.

## 네트워크 증거와 미완료 항목

사용자가 이미 수행한 control → worker HTTP DNS / Pod IP probe는 `opsloop-worker-ok` 응답을 반환했고 임시 `opsloop-nettest`는 삭제되었다. 이는 **사용자 제공 실행 기록**이며 이번 읽기 전용 검증에서 재실행하지 않았다.

| 항목 | 판정 |
| --- | --- |
| Host → API GET, 두 Node Ready | 이번 작업에서 확인 |
| control → worker HTTP / DNS / Pod IP | 사용자 제공 성공 기록 |
| worker → control 애플리케이션 HTTP | 미검증; kubelet·exporter scrape 성공으로 역방향 앱 통신을 대체하지 않음 |
| underlay / flannel 실제 MTU, 큰 payload | 미검증 |
| 노드 간 direct/relay·RTT·손실·partition | 미검증 |
| public 포트 비노출·ACL·OS firewall 전체·각 노드 route | 미검증 |
| Host/VM 재부팅·절전·전원 장애, datastore backup/restore | 미검증 |
| Azure ARM64 3번째 Node, ACR·OIDC·native ARM 실행 | 미구축·미완료 |

## 재검증

```bash
python3 scripts/verify-platform.py
python3 -m unittest discover -s tests -p 'test_verify_platform.py' -v
```

[검증 도구](../../scripts/README.md)는 정확히 현재 두 노드 이름/IP·AMD64·고정 버전·Ready·pressure·system Pod, [관측 상태](p2-observability-verification.md)를 검사한다. exit 0은 **현재 로컬 baseline**의 선택된 항목만 통과했다는 뜻이며 P1 전체·재부팅 복구·망 보안·P2 전체의 승인이 아니다. 접근 실패는 exit 2로 출력한다. 미래 Azure 노드 추가 시 기대 topology를 명시적으로 개정해야 하며 현재 기대값에 조용히 포함시키지 않는다.
