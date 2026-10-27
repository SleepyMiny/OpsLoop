# Public Status Home — 설계 확정 / 운영 구현·배포 미실행

2026-10-21 KST. 메인 세션 단독. 기준 main `1c2740b9e337bf85c32dba849be36291d2ad3e39`.
브랜치 `design/public-status-home-20261020`, 전용 worktree 안에서만 작성했다.
기존 `evidence/baseline.json`을 보존했으며 이전 조사·CI·실험을 반복하지 않았다.

**결정:** Ubuntu의 읽기 전용 Collector가 허용 필드만 구성해 HTTPS로 전송한다.
같은 `opsloop` Worker와 무료 D1의 단일 행이 최신 관측/재송신 방지를 제공한다.
`https://opsloop.sleepyminy44.workers.dev/`와 Demo/Replay 링크를 유지하고
새 기본 Home만 same-origin `GET /api/status`를 사용한다.
Demo History는 우선 관람객 브라우저의 IndexedDB에 저장한다. 공유 서버 이력은 범위 밖이다.

| 파일 | 내용 |
|---|---|
| architecture.md | 재사용 코드·데이터 흐름·D1 선택·UI 경계·비용 |
| data-contract.md | 공개 허용 필드, 측정/수신 시각, 실패 응답 |
| public-status.schema.json | 공개 snapshot JSON Schema 2020-12, 추가 필드 금지 |
| public-status.synthetic.json | **합성** 예시. 현재 클러스터 관측값이 아님 |
| security.md | 서명·재전송·rate limit·최소 RBAC·자격 갱신 |
| status-storage.sql | 승인 전 실행 금지인 저장소 설계; 로컬 SQLite에서만 테스트 |
| wrangler.status.example.json | 기존 URL을 유지하는 후속 구성 예시. ID 미지정·실행 대상 아님 |
| implementation-plan.md | 파일별 변경, 회귀/배포 수용 기준·승인 게이트 |
| prototype/ | stdlib Python의 오프라인 계약 모델·집중 테스트 |
| test-results.md, evidence/ | 실제 로컬 결과와 기존 증거, 미검증 항목 |
| sources.md, checkpoint.md | 공식 문서 근거와 다음 재개 지점 |

로컬 검증:

```bash
python3 -B -m unittest discover -s design/public-status-home/prototype -p 'test_*.py' -v
```

프로토타입은 네트워크/HTTP 서버/Worker/Collector가 아니다.
SQLite CAS 모델 통과를 실제 D1·Cloudflare 동작으로 주장하지 않는다.
JSON Schema 테스트기는 여기서 생성한 제한된 keyword 집합만 검사한다.
운영용 Go/TypeScript 포트와 실제 배포는 다음 단계다.

공개 URL은 기존 조사 클라이언트에 403/1010을 반환했다.
클라이언트 차단과 부합하지만 계정 정책·실제 Worker revision/bindings는 미검증이다.
보안 설정 완화·우회·Cloudflare 로그인·SSH 재조사는 수행하지 않았다.

Git commit/Push/PR/Merge, D1 생성/Secret 등록/배포, Ubuntu 설치,
Kubernetes/Azure/DNS/Enforce 변경 및 장애 주입은 모두 미실행이다.
