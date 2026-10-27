# 선택적 Demo B 수정본 ARM64 릴리스 검증 계획

상태: **준비 계획 / NOT_RUN**. M3 기본 완료에 이 배포를 강제하지 않습니다. 기존 Demo B 결함 증거와 Draft PR #6을 보존하고, 수정본 native ARM64 실행을 별도 승인 범위로 검증할 때 사용합니다.

## 승인 전에 고정할 입력

| 항목 | 값 또는 필요한 확인 |
| --- | --- |
| 수정 commit | `7e38e2b0196b726140c157b8d51b0fd5d33896da` |
| PR | `https://github.com/SleepyMiny/OpsLoop/pull/6`, 기록 시 Draft/Open, merge 없음 |
| 수정 파일 | `apps/engineering-demo/discount.go`, `apps/engineering-demo/discount_test.go` |
| 수정 이미지 index/platform digest | **미생성·미확정**. commit 고정 소스에서 빌드 후 실제 digest·manifest receipt 필요 |
| 대상 후보 | 기존 `opsloop-demo-b/engineering-demo`; 이 문서는 해당 변경을 승인하지 않음 |
| 기록된 Deployment UID | `ee1fee32-691b-410f-b632-4221c07c7bf5`; 실행 직전 현재 UID와 대조 필수 |
| 기록된 Policy UID | `1681e8da-f79a-4c9e-a352-90c6dc598c06`; disabled와 generation 확인 필수 |
| 기존 결함 이미지 digest | `sha256:5208efc730450a40022f787e7304f728f7a0427a33a945332f5da00360c6053b` — Demo B bundle의 과거 값, 현재 spec 재확인 필수 |
| ARM64 후보 Node | `opsloop-azure-worker`; 현재 Ready·arch·requests·가용량을 읽기 전용 확인 |
| 비용/리소스 | 새 Azure VM 생성 없음. 기존 Worker의 임시 Pod/이미지 저장·pull 사용량은 별도 산정. 비용 0이라고 단정하지 않음 |

이미지 digest가 없는 현재 상태에서 “정확한 배포 승인”을 요청하지 않습니다. 먼저 로컬 고정 소스·테스트·이미지 산출물과 배포 patch 초안을 준비합니다. registry push가 필요하면 대상 registry·tag·digest·비용을 포함해 별도 승인받습니다. `latest`나 mutable tag만으로 배포하지 않습니다.

## 실행 전에 준비할 변경 패킷

1. PR 수정 commit의 두 파일이 원본 candidate 해시와 일치하는지 확인하고 독립 검증 receipt를 연결합니다. canonical의 결함 기준 소스를 덮어쓰지 않고 격리된 빌드 입력을 사용합니다.
2. 현재 Deployment spec, resourceVersion, UID, 기존 image와 Pod UID, Service/Endpoint, Policy enabled·generation을 읽기 전용으로 보존합니다. Secret 값·환경 인증정보는 승인 자료와 결과물에서 제거합니다.
3. target Namespace/UID와 기대 resourceVersion을 test하는 JSON Patch 초안을 만듭니다. 변경은 승인된 image digest와 native ARM64 검증에 필요한 최소 placement 조건으로 한정합니다. Controller scope/RBAC/Policy Enforce를 바꾸지 않습니다.
4. 적용 전·후 예상 서비스 영향을 기록합니다. 기존 결함 Pod 교체, Endpoint 변화, 일시 중단 가능성, 최대 대기시간, 재시도 0회, 실패 시 복원 시점을 명시합니다.
5. 승인자에게 적용 명령·patch SHA-256·대상 UID·image digest·복원 patch와 SHA-256·예상 비용/자원·검증 명령을 한 번에 제시합니다.

별도 Namespace를 사용하는 대안은 기존 결함 서비스 영향을 줄이지만 새 리소스·권한·비용 범위가 달라집니다. 그 대안을 택하면 기존 대상 승인으로 실행하지 말고 새 대상과 복원 방법을 다시 승인받습니다.

## 승인 후에만 수행할 검증

승인한 단 한 번의 변경 후 새 Pod UID, 실제 `arm64` Node 배치, container imageID의 digest, startup/health, 빈 주문 HTTP 200과 `{total:0, average:0}`, 정상·경계 입력 결과를 수집합니다. 교차 빌드만으로 native ARM64 실행 PASS를 선언하지 않습니다. Deployment 적용만으로 서비스 복구 PASS를 선언하지 않습니다.

실패·기한 초과 시 자동 재주입이나 이미지 재교체를 반복하지 않습니다. 승인된 복원 patch를 현재 UID/resourceVersion 조건과 대조해 적용하고 원래 image·placement·replica spec·Pod/Endpoint 상태를 확인합니다. 단순 `rollout undo`에 의존하지 않고 보존한 정확한 spec을 기준으로 합니다. 복원도 실패하면 현재 상태와 원인을 기록하고 추가 변경 승인을 요청합니다.

원본 Demo B ZIP, bundle SHA-256 `b2c0eb09d308abb8359ee7647d9def3c2381c94e6ea334fe138d6e38d8de572b`, 결함 이미지 manifest, PR receipt를 변경하지 않습니다. 새 검증은 새 run ID/epoch와 별도 receipt로 연결합니다. 실제 배포하지 않은 현재 상태는 계속 **PR_CREATED / deployment NOT_RUN / native ARM64 UNPROVEN**입니다.
