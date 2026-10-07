# OpsLoop Implementation Plan

기준: [Architecture v0.1](architecture/architecture-v0.1.md), 2026-10-07.

**현재 상태는 P0 설계·문서화다. P1~P8 기능·환경은 미구현이며 아래 DoD는 향후 수용 기준이다.** 문서에 적힌 명령·시나리오가 실행되거나 통과했다는 의미가 아니다.

## 진행 원칙

- Runtime Recovery·플랫폼·운영 검증에 약 70%, Engineering Recovery에 약 30%를 배정한다.
- `ObserveOnly → Runtime Demo A → Evidence / Bundle → Engineering Demo B` 순서를 지킨다.
- 실패·미확정 환경을 성공으로 대체하지 않는다. DoD에는 실제 명령, 환경, artifact, 결과와 제한을 남긴다.
- P4가 완료되기 전에 AI 기능을 중심 작업으로 확장하지 않는다.
- 정상 경로뿐 아니라 재시작, 충돌, timeout, 후보 없음, 권한 차단을 검증한다.
- 각 Phase의 완료를 문서화하되 구현이나 실험이 없는 Phase를 완료로 표시하지 않는다.
- Phase 이동 자체가 resource 생성, 비용 지출, commit/push, 배포 승인을 의미하지 않는다. 해당 작업의 사용자 지시와 권한 범위에 따른다.

## Phase 개요

| Phase | 범위 | 선행조건 | 상태 |
| --- | --- | --- | --- |
| P0 | Architecture·ADR·계약·범위·환경 가정 | 요구사항 | 설계 기준 문서화 |
| P1 | Terraform·Tailscale·3-Node k3s·ACR·OIDC 기반 | P0, 환경 적합성 | 미착수 |
| P2 | Demo API·Multi-Arch pipeline·관측 stack | P1 | 미착수 |
| P3 | ObserveOnly Controller·분류·후보 평가·정책 | P2 | 미착수 |
| P4 | Runtime 실행·검증·guardrail·Demo A | P3 | 미착수 |
| P5 | 반복 Application Failure·Bundle·격리 | P4 | 미착수 |
| P6 | Runner·Codex·Feedback·검증·PR·Demo B | P5, Engineering 환경 | 미착수 |
| P7 | 전체 재현·시연 결과·운영 문서·보안 보강 | P6 | 미착수 |
| P8 | 전용 Dashboard·Argo CD·확장 장애 | 핵심 End-to-End 완료 | 선택, 미착수 |

## P0 — Architecture 기준 고정

### 범위

요구사항을 비판 검토하고 Component 책임, Interface, Runtime·Engineering 상태 머신, CRD, Incident Bundle, 정책, 권한, 테스트와 제한을 고정한다. 결정과 외부 환경 가정을 구분한다.

### Definition of Done

- Architecture v0.1과 ADR-001~005가 일관된 결정을 설명한다.
- README가 포트폴리오·개발 진입점이며 미구현 상태를 명시한다.
- Runtime 한도 소진, 후보 부재, 관측 실패, 운영 장애를 구분한다.
- 재시작·중복·외부 Deployment 변경·partition 처리 계약을 기록한다.
- P1~P8 수용 기준과 미확정 환경을 기록한다.
- 문서 구조·상대 링크·생성 파일·git diff·git status를 검토한다.
- 기능 코드, Azure 리소스, Terraform apply, cluster 구축 없이 종료한다.

### 현재 증거 범위

Phase 0 분석에서 공식 문서와 설치된 Codex CLI interface를 확인하고, 전문가 설계 검토와 통합안 독립 검토를 수행했다. 실제 운영·코드 동작 검증은 없다. 이번 문서화 작업도 구현 검증을 대신하지 않는다.

## P1 — 환경과 플랫폼 기반

### 범위

환경 적합성 확인 후 Azure Terraform, remote state bootstrap, Tailscale, 고정 version의 k3s, ACR와 CI OIDC 기반을 마련한다. 단일 control plane과 혼합 architecture worker를 구성한다.

### Definition of Done

- region·ARM SKU·quota·Ubuntu ARM image의 실제 가용성을 확인한다.
- CIDR 충돌, Host/VM 자원, 복구용 관리 경로, 비용 범위를 확인한다.
- Terraform `init`, `fmt`, `validate`, `plan`이 재현 가능하다.
- 허가된 환경에서 생성·삭제·재생성 절차를 검증하고 결과를 기록한다.
- bootstrap state와 testbed state가 분리되고 testbed destroy로 backend를 삭제하지 않는다.
- secret이 Terraform state, Git, cloud-init 본문에 들어가지 않는 bootstrap 절차를 검증한다.
- 3개 Node가 Ready이며 architecture·Node identity가 예상과 일치한다.
- Kubernetes 포트의 public 미노출, tailnet 통신, Pod 간 통신, DNS, MTU·큰 payload를 확인한다.
- Azure 명시적 egress, ACR 접근, main 전용 OIDC trust 조건을 확인한다.
- ACR pull 인증의 별도 발급·교체 방법을 마련한다.
- version, 설정, 검증 결과, cleanup 절차를 기록한다.

### 결과물

Terraform·bootstrap 구성, 환경 점검표, network 검증 결과, state 보관·복구와 cleanup runbook. 현재 문서화 작업에서는 생성하지 않는다.

## P2 — Demo API·이미지·Observability

### 범위

Go Demo API와 `/`, `/health`, `/ready`, `/metrics`, metadata 응답을 구현한다. Multi-Arch pipeline과 최소 관측 stack을 구축한다.

### Definition of Done

- API 기본 동작과 단위 테스트를 검증한다.
- 같은 OCI image index를 AMD64·ARM64 Node에서 native 실행한다.
- Node·Pod·Version·Architecture 응답과 실제 배치를 대조한다.
- SHA tag, index digest, 플랫폼 digest, source commit provenance가 연결된다.
- PR 검증은 credentials 없이 실행하고 보호된 main만 OIDC로 ACR에 게시한다.
- 제3자 image의 고정 version과 architecture 지원을 확인한다.
- node exporter·kube-state-metrics·Application metric이 수집되고 Node 매핑이 정확하다.
- Prometheus/Grafana는 Laptop VM에 배치하며 저장량·retention·requests를 제한한다.
- 고정 NodePort/Service 경로를 Host에서 호출할 수 있다.
- cold/warm cache와 direct/relay의 측정 구분을 준비한다.

## P3 — ObserveOnly 평가기

### 범위

RecoveryPolicy·RecoveryIncident의 최소 CRD, Monitor, deterministic Classifier, Candidate Evaluator를 구현하되 workload 변경은 하지 않는다.

### Definition of Done

- 실제 cluster에서 Node·Pod·requests·metrics snapshot을 모아 판단한다.
- API 상태와 Prometheus 사용량의 권위를 분리한다.
- 모든 후보와 모든 reject reason, score breakdown, policy hash를 기록한다.
- missing·stale·NaN metric을 0 사용률로 취급하지 않는다.
- Application 분류의 threshold·signature·운영 원인 우선순위를 검증한다.
- score·동점·resource reserve·cooldown 경계의 단위 검증을 통과한다.
- CRD schema·status·Incident 예약의 재시작 일관성을 검증한다.
- ObserveOnly가 Deployment를 변경하지 않음을 확인한다.

## P4 — Runtime Recovery와 Demo A

### 범위

조건부 nodeSelector patch, action intent, placement·HTTP verification, cooldown·예산·circuit을 구현한다. 양방향 resource-aware Node Recovery를 검증한다.

### Definition of Done

- 기본 Scheduler와 RollingUpdate surge가 선택 Node에 새 Pod를 배치한다.
- 현재 attempt의 Pod UID·ReplicaSet·Node·HTTP·안정화 결과로 성공을 판정한다.
- 전체 rollout 완료나 old Pod 삭제를 성공 전제로 두지 않는다.
- oldPodsOutstanding과 partition 한계를 기록한다.
- Demo A: Azure 부하 → Laptop 선택, Laptop 부하 → Azure 선택을 각각 최소 5회 검증한다.
- 동일 image index의 실제 플랫폼 실행과 후보 reject 이유를 입증한다.
- 모든 후보 reject, Prometheus 중단, image pull 실패, ARM manifest 누락을 안전하게 처리한다.
- patch 직후 Controller 종료·중복 전달·외부 image 변경 시 잘못된 추가 action이 없다.
- workload가 변경되면 SUPERSEDED로 중단하고 외부 변경을 덮어쓰지 않는다.
- 후보 부재·운영 장애가 Engineering으로 잘못 전환되지 않는다.
- T_detect, T_decide, T_place, T_recover 원자료와 환경을 기록한다.

초기 warmed 복구 목표는 180초 이내지만 실측 전 보장이 아니다. 목표 미달은 원인·측정 조건·조정 결과를 남기며 숫자만 바꿔 통과시키지 않는다.

## P5 — Application Failure·Evidence·격리

### 범위

고정 입력에 의한 실제 process crash, 반복 signature, 증거 보존, Bundle schema, handoff 준비, opt-in quarantine을 구현한다.

### Definition of Done

- 비어 있지 않은 입력이 필터 후 빈 집합이 되는 집계 경계를 fixture로 재현한다.
- Go handler의 panic recovery와 구분해 실제 process 종료를 확인한다.
- 각 Runtime action 후 동일 fixture를 재생하고 잠복 실패를 RECOVERED로 오판하지 않는다.
- kubelet restart와 OpsLoop action 예산을 따로 기록한다.
- 최초 장애와 변경 전 evidence를 보존하며 누락·truncation·redaction을 표시한다.
- Bundle의 schema·provenance·digest·크기 제한을 검증한다.
- 부족한 evidence·잘못된 repository·명령 삽입으로 Engineering을 시작하지 않는다.
- 예산 소진 후 evidence 저장 → 조건부 scale=0 → handoff 순서를 검증한다.
- QuarantineRequested/Confirmed/Unconfirmed와 circuit reset 조건을 확인한다.
- Infra·OOM·설정 장애를 코드 수리로 자동 전환하지 않는다.

## P6 — Engineering Recovery와 Demo B

### 선행 환경

고정 Codex CLI·kit revision, 모델 접근·인증, 격리된 worktree와 Build/Test 환경, 제한된 GitHub App 권한, 신뢰된 verification profile을 준비한다. API provider 인증이 테스트 코드에 노출되지 않음을 확인한다.

### Definition of Done

- queued Incident를 검증하고 단일 job claim·durable ledger·재시작 복구를 수행한다.
- 별도 worktree와 incident branch, source base commit을 사용한다.
- Codex와 orchestration kit은 분석·patch에 사용하고 generic orchestration을 재구현하지 않는다.
- allowed paths, symlink·경로 탈출, 파일 크기·삭제, 금지 workflow·harness 변경을 강제 차단한다.
- 최대 iteration과 stage/전체 timeout, cancel, 환경 실패를 구분한다.
- Build·Unit·Integration·고정 regression·Multi-Arch smoke·독립 review가 모두 통과한다.
- baseline fail → patched pass를 같은 신뢰된 fixture로 입증한다.
- 실패 Feedback loop를 결정적인 fixture/test double로 검사하고 실제 Codex 결과도 별도 기록한다.
- QEMU 결과는 emulated로 표시하고 native 전용 원인은 필요한 검증 없이 통과시키지 않는다.
- Publisher가 정확한 tested SHA·diff를 재확인하고 실제 PR을 생성한다.
- PR 생성 실패·생성 직후 종료에도 중복 PR이 생기지 않는다.
- Codex·test sandbox에 Kubernetes/Azure/GitHub credentials와 Host Docker socket이 없다.
- main 직접 변경, Auto Merge, 자동 재배포가 없다.
- PR_CREATED와 Runtime RECOVERED를 별개로 표시한다.

## P7 — 재현성과 포트폴리오 완성

### Definition of Done

- 새 환경에서 준비·설치·Demo·측정·cleanup 순서를 재현한다.
- Demo A/B의 실제 결과, 실패 시나리오, 정책·image·tool version, 측정 원자료를 보관한다.
- README의 구현 상태를 증거와 함께 갱신한다.
- credential 교체·quarantine 해제·수동 복귀·backup/restore·비용 정리 runbook을 작성한다.
- Human Review → 승인된 Merge → 별도 배포의 경계를 문서화한다.
- known limitations와 Production 대안을 시연 결과와 함께 설명한다.
- 핵심 부정 시나리오와 보안 경계 검증을 완료한다.

## P8 — 선택 확장

후보는 전용 Dashboard, Argo CD/GitOps, 추가 장애 유형이다. 필요가 확인된 항목만 선택한다.

### Definition of Done

- 선택 기능의 범위·사용자 가치·검증 기준을 먼저 정한다.
- Controller 소유 nodeSelector·replicas와 GitOps의 변경 충돌을 해결한다.
- Demo A/B, 권한 경계, 정책·상태 계약에 회귀가 없다.
- 운영 부담·resource 사용량·새 dependency를 문서화한다.
- 선택 기능 미구현을 핵심 End-to-End의 미완료로 취급하지 않는다.

## P1 시작 전 사용자 준비

1. Azure 구독·region·ARM quota·Ubuntu image와 리소스/역할 할당 권한을 확인한다.
2. Laptop VM 자원, 절전 방지, Physical Server의 장애 후 접근 방법을 확인한다.
3. CIDR 계획, Tailscale 장치 등록·ACL 권한, MTU 시험 조건을 정한다.
4. GitHub Actions, main 보호, OIDC trust 설정 권한과 실제 subject를 확인한다.
5. tool version 고정, state·secret 보관 위치, 비용·부하 한도를 정한다.

현재 단계에서 이러한 자원을 생성하거나 설치할 필요는 없다. [외부 환경 목록](architecture/architecture-v0.1.md#external-environment)의 확인 결과를 다음 Phase의 입력으로 사용한다.
