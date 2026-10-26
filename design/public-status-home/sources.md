# 재사용한 증거와 공식 자료

공식 자료는 2026-10-20 조사 결과를 재사용했다. 계정별 활성 설정의 증거로 확대하지 않는다.

- [Workers script routing](https://developers.cloudflare.com/workers/static-assets/routing/worker-script/): selective run_worker_first와 asset binding. `/api/*`만 우선 실행하고 기존 static은 별도 경로 유지.
- [Static Assets headers](https://developers.cloudflare.com/workers/static-assets/headers/): `_headers`는 Worker 생성 응답에 자동 적용되지 않음. API는 직접 headers 구성.
- [Workers limits](https://developers.cloudflare.com/workers/platform/limits/): Free dynamic100,000 requests/day, CPU10ms. 계정 공유량·실제 runtime 검증 필요.
- [D1 pricing](https://developers.cloudflare.com/d1/platform/pricing/): Free read5million/day, write100,000/day, storage5GB total. 한도 도달 시 query 실패, 별도 유료 활성화 안 함.
- [D1 database API](https://developers.cloudflare.com/d1/worker-api/d1-database/): batch transaction 및 primary query/Session 의미. 이번 replay gate는 단일 conditional UPDATE로 줄임.
- [Workers Rate Limiting](https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/): counters는 PoP별·eventually consistent, strict global accounting이 아님.
- [Error1010](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-1xxx-errors/error-1010/): browser signature 관련 차단 설명. 관측 클라이언트의403만으로 사이트 전체 상태를 단정하지 않음.

로컬 근거:

- 기준 main `1c2740b9e337bf85c32dba849be36291d2ad3e39`; PR#5 MERGED 원격 읽기 확인.
- `~/.local/state/opsloop/dashboard-public-static-20261020`: pure-static 소스·브라우저 증거.
- `~/.local/state/opsloop/dashboard-public-pr-20261020`: PR#5 CI, public587개 manifest, 원본 보존.
- `~/.local/state/opsloop/dashboard-public-project-name-20261020`: 프로젝트명 opsloop, CI39346110645 SUCCESS.
- `~/.local/state/opsloop/public-dashboard-readiness-20261020/server-inventory.md`: Ubuntu24.04,
  CPU4/RAM7.70GiB, 당시 Tailscale/K8s TCP/Prometheus ready 접근. 현재 전용 SA/Collector 가용성을 대신하지 않음.
- `evidence/baseline.json`: 이전 세션 probe 결과를 그대로 보존. 원본 headers/credentials 수집 없음.

새 SSH/클러스터 조사·fault injection·CI 전체 재실행은 하지 않았다.
순수 정적 산출물 및 Replay 원본 데이터는 변경하지 않았다.
