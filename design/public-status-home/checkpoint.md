# CHECKPOINT — DESIGN READY / RUNTIME NOT IMPLEMENTED

- 최신 재개: 2026-10-21 KST. 기준 main1c2740b, 기존 design branch/worktree 유지.
- 이전 파일은 evidence/baseline.json 하나였음. 그대로 보존, 삭제/덮어쓰기 없음.
- 원본 main6aa9daa 및 Terraform 미커밋 hash/status 보존.
- 완료: BFF/PromQL/Demo/Replay 재사용 경계, allowlisted schema,
  D1 단일 최신행·글로벌 seq CAS, HMAC·timestamp·rate-limit 계약,
  최소 GET Identity 및 별도 단기 token 회전, same-origin selective Worker routing,
  freshness/실패 UI·browser-local Demo History 결정, 단계별 구현/배포 테스트 계획.
- 로컬 프로토타입/집중 테스트29 PASS. 모형의 보장 범위는 test-results.md 참고.
- 제품 소스/정적 산출물/Replay byte 변경0. Cloudflare/K8s/Azure/서버 변경0.
- 배포 전 미검증: 실제 Cloudflare bindings/plan/security rule,403/1010,
  D1 runtime/CPU/quota, 전용 Identity access/rotation, 새 UI/HTTP integration.
- 다음 재개: implementation-plan.md 1번부터 로컬 Go DTO/Collector와 Worker port,
  공통 HMAC vector, 모델과 동일한 거절/동시성 계약. 기존 전체 감사는 반복하지 않음.
- source=home(default)/simulation/replay를 분리. 비공개 Live 유지.
  현장 Enforce/자동복구 실증으로 표시 금지.
- D1/Secret·K8s Identity·Ubuntu 설치·실제 Worker/기본 화면 배포는 별도 승인 경계.
- Git commit/Push/PR/Merge 없음. 이후 변경은 이 local design 폴더부터 검토 가능.
