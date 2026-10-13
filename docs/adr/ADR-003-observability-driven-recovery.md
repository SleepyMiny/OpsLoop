# ADR-003 — Observability-driven Recovery

- 상태: Accepted — Architecture v0.1 설계 결정; 관측 스택 구축, Evaluator·freshness·resource 판단은 PR #2에서 코드·테스트 구현(라이브 검증 미실행)
- 날짜: 2026-10-07
- 관련: [Architecture](../architecture/architecture-v0.1.md), [ADR-002](ADR-002-runtime-recovery-placement.md), [Implementation Plan](../implementation-plan.md)

## 배경

고정된 fallback Node만 사용하면 자원 상태에 따른 복구 결정 역량을 보여주기 어렵다. 반대로 CPU 사용률만 보면 requests가 가득 찬 Node를 선택할 수 있다. Prometheus를 Dashboard용으로만 사용하지 않고 실제 판단에 연결하되, 관측 누락이나 지연이 위험한 배치를 유도하지 않게 해야 한다.

## 결정

- Kubernetes API를 Ready·taint·unschedulable·architecture·allocatable·Pod 상태·requests의 권위로 둔다.
- Prometheus/node exporter를 실제 CPU·Memory 사용량의 권위로 둔다.
- kube-state-metrics는 시각화와 교차 확인에 사용한다.
- 먼저 Hard Filter를 적용하고 통과 후보만 headroom 기반 Scoring한다.
- 정책 가중치와 한도를 RecoveryPolicy에 보관하며 결정에 policy hash와 snapshot을 남긴다.
- 누락·stale·NaN metric은 0 사용량으로 대체하지 않는다. 관측 부족 시 새로운 resource-aware 배치 변경을 보류한다.
- 후보 부재는 WAITING이며 action 예산 소진이나 Engineering 전환으로 계산하지 않는다.

## 고려한 대안

| 대안 | 평가 |
| --- | --- |
| 고정 fallback 순서 | 단순하지만 현재 자원 상태에 따라 대상이 달라지는 요구를 충족하지 못함 |
| 실제 사용률만 평가 | Scheduler가 보는 requests와 allocatable을 놓침 |
| requests만 평가 | 실제 Memory 압박·CPU 부하를 놓침 |
| Prometheus만 모든 상태의 권위로 사용 | scrape 지연·중복 metric과 최신 API 상태의 차이를 통제하기 어려움 |
| AI score·분류 | 재현성·규칙 설명·경계값 검증을 약화 |
| 전체 관측 stack 도입 | 4 GiB worker와 프로젝트 범위에 불필요한 부담 |

## 선택 이유

제어 상태와 관측 상태를 구분하면 decision 근거를 대조할 수 있다. hard filter는 금지 조건을 점수로 상쇄하지 않게 하고, score는 충분한 자원이 있는 후보 사이의 선택을 설명한다. 가중치·freshness·timeout을 정책으로 노출하면 실측에 따른 조정이 가능하다.

## 결정 모델

필수 reject에는 Node NotReady, unschedulable, taint, architecture 불일치, CPU·Memory pressure, 요청 자원 부족, 가용 Memory 부족, Disk/PID pressure, metric 누락·stale, cooldown, 미지원 constraint가 포함된다. 후보별 모든 reject reason을 기록한다.

CPU headroom, Memory headroom, 새 Pod·reserve를 반영한 requested headroom을 0~1로 계산한다. 초기 가중치는 0.30 / 0.35 / 0.35이며 recent failure penalty를 뺀다. 동점은 안정적인 Node 이름 순서로 해소한다. 세부 기본값은 [Architecture 정책 표](../architecture/architecture-v0.1.md)에 둔다.

초기 scrape 10초, sample age 30초, CPU rate window 60초를 사용하되 실측으로 조정한다. 구현은 window를 `2 × maxSampleAge`로 바꿨다(아래 구현 상태 기록, 구현: PR #2). 파생 결과 시각뿐 아니라 원본 sample timestamp·수집 상태를 확인한다. 실행 직전 상태·freshness를 재확인한다.

## 결과와 Trade-off

- Prometheus 가용성이 새로운 자원 기반 복구의 전제조건이 된다. 데이터가 없으면 안전하게 대기한다.
- snapshot과 실제 scheduling 사이의 시간차를 없앨 수 없다. Pending·재평가·deadline이 필요하다.
- Requests와 사용량의 이중 검증으로 일부 Node가 보수적으로 제외될 수 있다.
- Node identity와 exporter target mapping, metric version 관리가 필요하다.
- 상세 evidence는 CRD/Bundle에 두고 고카디널리티 incident·stack label을 metric에 넣지 않는다.
- 최소 stack은 Prometheus, Grafana, node exporter, kube-state-metrics, Application/OpsLoop metrics다.

## 검증 기준과 재검토 조건

P3에서 경계값·단위·누락·NaN·stale·API 불일치와 ObserveOnly 결과를 검사한다. P4에서 실제 부하가 반대 Node 선택으로 이어지는지 검증한다. 관측 장애 중에도 복구해야 하는 요구가 생기면 fallback의 안전성과 의미를 별도 ADR로 정한다.

## 근거

- [Prometheus Querying Basics](https://prometheus.io/docs/prometheus/latest/querying/basics/)
- [Kubernetes Resource Management](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/)

## 구현 상태 기록 (2026-10-12)

설계 책임 경계는 유지한다. 기존 수동 구축과 이번 읽기 전용 재검증의 범위는 [P1-B 기록](../experiments/p1-b-local-platform.md) 및 [관측 기록](../experiments/p2-observability-verification.md)을 따른다. 일부 구현은 ADR의 모든 수용시험 통과 또는 P1/P2 전체 완료를 뜻하지 않는다.

## 구현 상태 기록 (2026-10-13)

Candidate Evaluator와 Prometheus 조회를 구현했다. 상세는 [Architecture §8](../architecture/architecture-v0.1.md#8-recoverypolicy-model)·[§11](../architecture/architecture-v0.1.md#11-prometheus--observability)·[§17](../architecture/architecture-v0.1.md#17-demo-application과-시나리오)에 있다.

- CPU rate window는 고정 60초 대신 `W = 2 × maxSampleAge`이며 W 안에 원본 표본 3개 이상을 요구한다. 실측 scrape가 30초라 demo Policy는 `maxSampleAge: 90s`를 쓰고 CRD 기본값은 30초로 둔다. 범위를 벗어난 표본 값은 `METRICS_MISSING`, 5초를 넘는 미래 표본 시각은 `METRICS_STALE`로 거부한다. (구현: PR #2)
- image index의 플랫폼 manifest 검사는 hard filter에서 하지 않는다. Policy `requiredPlatforms`와 Node label로 `ARCH_MISMATCH`를 판정하고, index의 두 플랫폼은 CI 게시 단계에서 검사하며, 수동 배포된 단일 플랫폼 이미지는 실행 시 pull 실패를 `IMAGE_PLATFORM_MISSING`으로 분류해 cooldown한다. Controller에 registry 조회 권한을 주지 않는 대신 사전 검사가 약해진다. (구현: PR #2)
- Demo A의 Laptop 방향 부하는 실제 CPU 부하가 아니라 requests 포화다. 배경에서 지적한 대로 CPU 사용률만으로는 requests가 가득 찬 Node를 거르지 못하므로 이 경우를 Kubernetes API 축으로 입증하고, Azure 방향은 실제 CPU 부하로 Prometheus 축을 입증한다. (구현: PR #2)
- 결과와 Trade-off의 최소 stack 중 OpsLoop 전용 metric은 구현하지 않았다. (구현: PR #2)
- 증거 범위: 로컬 Windows와 GitHub Actions에서 결측·stale·NaN·범위·미래 시각·중복 series 단위 테스트와 Prometheus 중단 시 WAITING → deadline `FAILED`(envtest)를 통과했다. 실제 Prometheus를 대상으로 한 ObserveOnly 판단과 실제 부하가 반대 Node 선택으로 이어지는지는 검증하지 않았다.
