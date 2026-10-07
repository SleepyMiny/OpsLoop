# ADR-004 — Engineering Recovery Boundary

- 상태: Accepted — Architecture v0.1 설계 결정, 미구현
- 날짜: 2026-10-07
- 관련: [Architecture](../architecture/architecture-v0.1.md), [Implementation Plan](../implementation-plan.md), [ADR-002](ADR-002-runtime-recovery-placement.md)

## 배경

AI 코드 수정은 비결정적 소요 시간, 임의 코드 실행, 인증과 repository 변경을 수반한다. 이를 Kubernetes Controller 안에 넣으면 운영 복구 루프와 권한이 섞인다. codex-orchestrator-kit은 범용 전문가 협업 기능이며 OpsLoop의 Incident 저장·검증·재시도·게시 계약을 대신하지 않는다.

## 결정

- `Controller → Collector → Incident Bundle → 별도 Engineering Runner → Codex Orchestration`으로 분리한다.
- Collector는 Controller binary의 모듈이며 최초 장애부터 evidence를 보존한다.
- 같은 Application Failure가 실제 Runtime action 예산을 소진하고 근거가 충분한 경우에만 handoff한다.
- 작은 마스킹 JSON Bundle은 불변 ConfigMap으로 전달하고 Runner가 API로 조회한다.
- Runner는 단일 job, 별도 worktree/branch, durable ledger, 제한된 iteration·timeout을 관리한다.
- kit의 skill·역할·협업을 재사용하되 generic orchestration을 재구현하지 않는다. Windows launcher는 필수 dependency가 아니다.
- Codex는 분석·patch 결과를 만들고 Runner가 범위·검증·상태·예산을 강제한다.
- Build/Test sandbox와 PR Publisher의 자격 증명을 분리한다.
- 모든 검증과 독립 review가 통과한 정확한 SHA만 PR로 게시한다. main 직접 변경·Merge·자동 재배포는 하지 않는다.

## 고려한 대안

| 대안 | 평가 |
| --- | --- |
| Controller 안에서 Codex 실행 | 운영 loop의 안정성·latency·권한 경계를 훼손 |
| kit을 완성된 Repair Engine으로 취급 | incident contract, 반복 제한, 검증과 게시 책임이 빠짐 |
| generic agent framework 재구현 | 재사용 요구와 맞지 않고 프로젝트 중심이 AI orchestration으로 이동 |
| queue broker·다수 Runner부터 시작 | 현재 1개 Demo에 비해 분산 상태·운영 부담 증가 |
| Bundle의 verification_command 실행 | 비신뢰 evidence가 실행 권한을 가질 수 있음 |
| AI의 성공 보고만 신뢰 | 테스트 위조·누락·범위 초과를 판별할 수 없음 |
| 검증 후 자동 Merge·배포 | Human Review 경계를 제거 |

## 선택 이유

별도 Runner는 운영 복구와 비신뢰 코드 실행을 분리한다. OpsLoop-specific adapter를 두면 generic 협업 도구를 유지하면서 필요한 Incident context와 검증 결과만 연결할 수 있다. 제한된 concurrency와 Kubernetes API handoff는 현재 규모에서 구현·재현·재시작 검증을 단순하게 한다.

## 결과와 Trade-off

- Runtime 장애에서 PR까지의 전달·상태·실패 처리를 Runner가 구현해야 한다.
- Bundle의 repository·base commit·image provenance·digest·완전성을 확인해야 한다.
- 자유 command 대신 trusted verification profile을 사용하고 AI가 검증 harness를 수정할 수 없게 한다.
- 테스트 코드도 비신뢰 코드이며 운영 credentials·provider 인증·Host Docker socket을 제공하지 않는다.
- credential 격리는 prompt나 역할명만으로 달성되지 않는다. 실제 sandbox와 실행 경로를 P6에서 검증해야 한다.
- Runtime과 Engineering status의 논리적 소유권은 RBAC field 격리와 같지 않다.
- heartbeat 만료만으로 job을 재할당하지 않고 기존 실행 종료를 확인한다.
- 동일 signature circuit과 opt-in scale=0 격리가 native restart 폭주를 제한한다. partition된 process 종료는 확정할 수 없다.
- PR_CREATED는 서비스 RECOVERED가 아니다. 서비스는 Human Review와 별도 배포까지 중단될 수 있다.
- 예상하지 못한 main 변경은 초기에는 자동 rebase 대신 STALE_BASE로 중단한다.

## 검증 기준과 재검토 조건

P5/P6에서 baseline fail → patch pass, Feedback, timeout, 범위 탈출, credential 차단, 부적절한 Bundle, interrupted job, 중복 PR를 검증한다. 다수 workload·다수 Runner·대용량 evidence가 필요해지면 queue·storage·claim 모델을 재검토한다.

## 근거

- [Codex Orchestrator Kit](https://github.com/yoyowasi/codex-orchestrator-kit/blob/main/README.md)
- [Codex Non-interactive Mode](https://learn.chatgpt.com/docs/non-interactive-mode)
- [Kubernetes ConfigMaps](https://kubernetes.io/docs/concepts/configuration/configmap/)
