# 로컬 검증 결과와 한계

2026-10-21. 기존 실패 실험/CI/브라우저 조사를 재실행하지 않았다.
현재 작성물은 설계와 오프라인 계약 모델이며 실제 Home 구현 완료 보고가 아니다.

## 이번에 실행

- `python3 -B -m unittest discover -s design/public-status-home/prototype -p 'test_*.py' -v`
  : **29 tests PASS**, 전체 로그 `evidence/local-tests.log`.
- 허용 필드 projection: 임의 raw node name·문서용 IP·가짜 UID/token/env/labels/spec가
  출력에 포함되지 않음. 이 테스트는 합성 입력이며 실제 Secret/로그를 읽지 않았다.
- 엄격 스키마/enum/range: unknown field, alias 중복·site 불일치, bool 숫자, NaN/Infinity,
  매우 큰 정수, 추가 JSON key, source 오류에 새 정상값 붙이기를 거절.
- 관측 의미: null≠0, 원본 시각 불변,75초/90초·미래5초 경계, 현재 NotReady와 stale 구분,
  과거 Incident와 active count 구분, 플랫폼 결측/0 replica의 가짜 정상 방지.
- HMAC: body/key/서명/host/path/method/identity/시각/seq 위변조 및 replay 거절.
  oversized 입력과 duplicate key 검사는 그 검사 외에는 정상 snapshot으로 파싱되는
  완전한 body를 사용해 실패 원인이 단순 필수 필드 누락이 되지 않도록 했다.
- SQLite CAS: 8개의 동시 중복 요청 중1개만 수락, 순서 역전/seq reset/15초 최소간격,
  연결 재시작 후 counter 보존, SQL 오류 시 body/counter 동시 불변, 누락 행 자동 생성 금지.
- JSON Schema와 생성 모델 일치, 합성 예시와 설정 JSON/Python 구문 검증 PASS.
  `run_worker_first`는 `/api/*`만, name은 opsloop, DB ID는 미승인 placeholder.
- `git diff --check` 및 새 텍스트 파일 whitespace 검사 PASS.
- 기존 tracked 제품 소스 변경0. 공개 Wrangler/정제 Replay/기존 Replay/Demo/CSP의 byte hash가
  기준 main과 동일. 원본 main HEAD/status 및 Terraform SHA256 이전값과 동일.

## 재사용한 기존 결과

기존 공개 profile: Dashboard291 PASS/실클러스터 opt-in1SKIP, 경계5 PASS,
desktop/mobile10cases, API/external request0, public587파일7,522,369바이트 및
PR#5 verify/image/dashboard SUCCESS. 이는 **기존 Demo/Replay**의 검증 결과다.
새 Home/Worker/Collector가 그 테스트를 통과했다고 주장하지 않는다.

## 미실행·미검증

- Go Collector, Worker WebCrypto/HTTP/D1 binding, runtime parser 및 TLS 전달.
- D1 실제 CAS/배포/권한·Free CPU/실계정 quota·key rotation.
- 전용 SA GET 거부/허용, 단기 token 유효기간·서버 측 갱신/접근.
- Ubuntu 서비스 계정/systemd/실제 status HTTPS 송신. 서버 조사 반복 없음.
- 새 Home 브라우저, IndexedDB History와 completion lifecycle,
  실제 Edge routes/CSP/캐시/기존 정적 URL 유지 여부.
- 기존 403/1010을 반환한 계정 정책과 일반 브라우저 접근 상태.

모든 테스트는 네트워크·실제 자격증명 없이 실행됐다. SQLite 모델 통과는 D1 Edge 검증이 아니다.
전체 JSON Schema 일반 적합성 테스트가 아니라 작성된 keyword subset 모델의 검사다.
공개 값의 실제 정확성은 다음 Go/Worker 포트와 승인된 관측 비교에서 확인해야 한다.
