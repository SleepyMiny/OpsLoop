# 다음 구현 순서와 검증 게이트

실행 계획이며 이번에 운영 적용하지 않았다. 현재 제품 소스·정적 산출물은 변경하지 않았다.
예시 Wrangler 파일에는 존재하지 않는 미래 entry와 미지정 database_id가 있어 배포할 수 없다.
Secrets/ratelimit namespace는 실제 계정 충돌·권한 확인 및 승인 뒤 별도로 설정해야 한다.

## 1. 로컬 계약과 Collector/Worker 구현

- `internal/publicstatus`: 공개 DTO, typed observation→explicit projection, 숫자/시각/enum 검증.
  기존 `internal/dashboardbff`/`candidates.QueryMetrics`와 transport를 필요한 범위에서 재사용.
  Controller runtime/정책/fence 코드는 변경하지 않는다.
- `cmd/status-collector`: 고정 resourceNames GET, namespace Incident LIST, Prometheus7query,
  per-source deadline/partial heartbeat, local alias mapping, single-process durable seq,
  HTTPS fixed-origin signing sender. CLI server 없음. credentials 내용/URL query를 로그에 출력하지 않음.
- `worker/public-status.ts`: `/api/status` GET, `/api/collect/status` POST만.
  bounded body/header parsing, unknown field 거절, HMAC검증, D1 single-statement CAS,
  error/dataState 계산, JSON no-store 응답. 외부 URL proxy·Kubernetes credentials 없음.
- 고정 test vector를 Go sender와 Worker WebCrypto 양쪽에서 검증한다.
  Python 모델 PASS만으로 runtime 구현을 완료 처리하지 않는다.

## 2. Home / Demo History와 빌드 분리

- 새 public-status profile: `/` Home, `?source=demo`, `?source=replay`, History.
  Home 전용 `PublicStatusSource`/parser와 lifecycle을 만든다.
  private Live `HttpDataSource`와 내부 DashboardSnapshot parser를 변경하지 않는다.
- `PublicStatusHome`은 snapshot이 없어도 loading/unavailable 상태로 렌더링한다.
  실제 fresh data 없이 Ready3개나 Incident0개 같은 초기값을 보여주지 않는다.
- 기존 Demo 엔진에 최소 completion hook 또는 wrapper 추가, autoLoop=false,
  Session별 idempotent finalize, StrictMode/재진입에서도 중복 저장 금지.
  IndexedDB 저장 실패·만료·제거·중단·seek 후 종료를 명확히 표시한다.
- Replay fixture와 engine 의미는 고정한다. Simulation History는 과거 실증 메뉴와 구분.
  실제 자동복구 검증과 시뮬레이션 자동재배치를 혼동하지 않도록 배지/설명을 유지한다.
- 새 profile CSP는 `connect-src 'self'`. 코드의 network 경로는 정확히 GET `/api/status` 하나만.
  서명/수집용 Secret는 VITE_*·브라우저 코드에 없다. public `source=live`는 계속 차단.
- 기존 pure-static `build:public`, `wrangler.public.json`, API0 검사를 그대로 남긴다.
  새 build:status/audit/tests를 추가하여 정적 전용 계약을 몰래 완화하지 않는다.

## 3. same-origin 라우팅 수용 기준

기존 Worker name `opsloop`와 hostname/route를 보존한다. 다른 프로젝트/도메인 생성 불필요.
`run_worker_first:["/api/*"]`, 일반 asset는 asset-first; root query string 기반 기존 링크 유지.
unknown API는 JSON404, unsupported method405이며 HTML SPA fallback을 하지 않는다.
Worker가 `ASSETS.fetch`를 호출하는 fallback에는 같은 header 정책을 명시적으로 적용/확인한다.
`_headers`가 Worker 생성 응답에 자동 적용된다고 가정하지 않는다.
API 응답에는 Worker 코드가 직접 no-store/nosniff/CSP를 설정한다.
필요한 routes/bindings가 실제 기존 프로젝트와 일치하는지는 배포 전 read-only account 조회가 필요하다.

## 4. 로컬 테스트 수용표 (계획과 완료 구분)

| 영역 | 필수 검증 | 이번 상태 |
|---|---|---|
| 공개 데이터 | typed 객체의 IP/UID/raw names/env/labels/spec sentinel이 projection 결과에 없음; unknown 필드 거절 | 오프라인 합성 모델 PASS |
| 측정 의미 | null≠0, 원본 timestamp, future5s·75/90s 경계, component 실패 heartbeat | 오프라인 모델 PASS |
| ingest | HMAC tamper/host/path/method/kid/seq/time, oversized/duplicate JSON, reply-loss, persistent CAS | 오프라인 모델 PASS, HTTP/Go/Worker port 미실행 |
| race/storage | 중복8개 경쟁1winner, 순서 역전·restart·DB오류 시 counter/body 일관성 | 로컬 SQLite PASS, D1 runtime 미검증 |
| RBAC | 승인 GET 성공, 다른 node/ns/Secret/exec/create/patch/tokenRequest 거절 | 계획만, 실제 Identity 미생성 |
| metrics | 기존 PromQL fixture와 비교; min3/중복/NaN/stale/분리 CPU·memory 오류 | 기존 query 코드 재사용 확인, 새 Collector integration 미실행 |
| UI | Home 진입 Demo 엔진0개; 선택 시만 play; source/historical 배지; history finalize1회/저장실패 | 계획만 |
| browser offline | API timeout/403/HTML200/invalidJSON/429/503 시 Demo fallback0; Demo/Replay는 API 차단해도 동작 | 기존 정적 증거 재사용, 새 Home 미구현 |
| 빌드/회귀 | 기존 public291tests+1skip/5boundary, local Live 계약, 새 profile typecheck/build;1920×1080/모바일 | 기존 결과 재사용, 새 Home 구현 뒤 실행 |
| Edge | `/api/*` priority, static no-worker, CSP 적용, quota/rollbacks, actual D1 | 미검증, 배포 승인 필요 |

기존 결과는 현재 pure-static 기능의 근거이며 새로운 Home의 동작 증거로 사용하지 않는다.
schema parser의 duplicate key·nonfinite·unknown field·범위를 각 언어에서 동일하게 검사해야 한다.

## 5. 실제 인프라 승인 지점과 배포 순서

이번 단계에서 여기부터 **중단**한다. 다음 리소스 변경은 각각 구체적인 승인 후 진행한다.

1. 최소 Identity/Role/Binding, 전용 단기 token 발급·전달·회전 검증.
   현재 관리자 credentials 재사용이나 cluster resource 변경 없음.
2. 무료 D1 1개·로컬 검증 SQL 승인 bootstrap, ingest HMAC Secret 1세트 및 rotation 준비.
   DB/Secret ID·계정 무료 plan·remaining quotas를 확인하고 예시 placeholder를 교체한다.
3. Ubuntu 전용 계정/읽기 전용 Collector 설치·systemd timer/service,
   고정 HTTPS egress와 private source 접근·clock·성능 검증. Docker/Jenkins/k3s 수정 없음.
4. 기존 Worker의 version/URL/asset manifest를 기록·백업한다.
   초기에는 기존 Demo/Replay 화면을 유지한 채 API/Collector 데이터 흐름을 먼저 검증한다.
   source timestamp와 실제 로컬 K8s/Prometheus 결과를 대조하고 privacy 검토 후 Home 기본 전환.
5. 실제 Edge/브라우저 GET, HMAC 실패/재송신 차단, D1 quota error, Collector 중단 모의
   (운영 서버/클러스터를 끄지 않고 로컬 fixture·테스트 route 사용), Demo/Replay 무영향 확인.

실패 시 Collector 전송부터 중지(해당 신규 서비스만, 별도 승인), 이전 immutable static Worker
version으로 되돌릴 수 있게 준비한다. D1는 삭제/초기화하지 않는다.
이전 counter보다 뒤로 DB를 복구하면서 ingest를 열면 replay 위험이 있으므로
복구 중 ingest 차단 및 key/seq 재동기화 승인 절차가 필수다.
원본 main/Terraform, 운영 Controller/Policy, 기존 Public URL은 변경 대상이 아니다.

## 남은 결정 / 검증

- 공개할 수 있는 상태값·부하 정보·일별 방문량 상한은 실제 배포 승인 때 최종 확인.
- Demo History의 브라우저별 저장은 이번 최소안으로 확정. shared history 요구가 생기면 재설계.
- 전용 token의 실제 최대 유효기간·매일 갱신 운영 가능성 또는 별도 issuer 필요성.
- 403/1010의 계정 보안 이벤트/정상 브라우저 경로, 기존 배포 설정/무료 quota.
- Free CPU10ms에서 bounded parsing/HMAC/serialization의 실제 실행 시간.
  넘어가면 먼저 payload/코드를 줄인다. 자동 유료 업그레이드 금지.
