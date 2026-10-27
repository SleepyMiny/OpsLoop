# M3 Git 소스 통합 계획

이 문서는 검토용 통합 계획입니다. M3 로컬 개발 중 stage·commit·push·PR 변경·merge를 실행하지 않습니다. 실행 승인에는 최종 변경 경로·소스 해시·기대 효과·복원 방법을 포함해야 합니다.

## 확인한 기준점

| 위치/참조 | 확인 상태 |
| --- | --- |
| canonical worktree | `feat/public-status-home`, HEAD `1c2740b` |
| canonical의 `origin/main` 및 실제 원격 `main` | `1c2740b9e337bf85c32dba849be36291d2ad3e39`; GitHub 읽기 전용 receipt 시점에 일치 |
| 별도 worktree의 로컬 `main` | `f1e1af4`; canonical HEAD와 다름 |
| Demo B 별도 로컬 저장소 | `.opsloop/demo-b-20261027/work/baseline-repository`, 조사 시 clean |
| Demo B baseline | `171d8c10ed522848d7ccd4dd33b0b4b86d5341fc` |
| PR #6 head | `7e38e2b0196b726140c157b8d51b0fd5d33896da` |
| PR #6 base/head branch | `demo/demo-b-baseline-20261027` / `opsloop/demo-b-empty-order-20261027` |

GitHub 읽기 전용 비교 receipt `reports/m3/github-graph.json`의 관측 시각은 `2026-10-26T23:02:55.723Z`입니다. Demo B baseline은 원격 main보다 2 commits 앞서고 뒤처진 commit은 없으며, main 대비 변경 파일은 168개입니다. `1c2740b` 위에 Demo A 소스 보존 commit과 결함 애플리케이션 기준점을 추가한 별도 이력입니다. PR #6의 변경 파일 2개라는 사실은 그 baseline과의 차이입니다. 이를 main으로 바로 merge하면 M3 플랫폼 소스 전체가 검토·반영된다고 볼 수 없습니다.

`reports/m3/baseline.json`은 작업 시작 당시 406개 소스 해시와 index SHA-256을 기록합니다. `baseline-status.txt`와 `baseline.patch`는 기존 미커밋·untracked 상태를 보존합니다. 최종 source manifest와 대조해 기존 파일의 유실 여부와 M3 변경 범위를 검토합니다. 원본 증거·작업 파일을 정리하기 위해 `reset`, `clean`, `stash`, `checkout`, 무분별한 `pull`을 사용하지 않습니다.

## 검토 가능한 PR 구조

1. **플랫폼 통합 PR**: 확인된 원격 main SHA `1c2740b9e337bf85c32dba849be36291d2ad3e39`를 기준으로, 승인받은 격리 작업공간에서 만듭니다. 실제 게시 직전에 원격 SHA가 변하지 않았는지 다시 확인합니다. 검토된 canonical 소스 manifest를 기준으로 Runtime/Guard/Observer/Engineering/BFF/React/테스트/문서를 포함합니다. 기존 미커밋 구현을 누락하지 않습니다.
2. **M3 전시·검증 변경의 리뷰 단위**: 위 PR 안에서 실행 엔진 보존 변경, 증거·BFF 계약, React 전시, 로컬 CI·문서를 검토 가능한 commit 묶음으로 정리합니다. 이것은 여러 중간 릴리스를 요구하는 계획이 아닙니다. 실제 commit은 별도 승인 후에만 만듭니다.
3. **기존 PR #6 유지**: 실제 AI 수정과 독립 검증의 증거 역할을 유지합니다. base를 임의 변경하거나 자동 merge하지 않습니다. 운영 결함을 유지한 Demo 기준점과 수정본의 차이를 계속 추적합니다.
4. **수정본 채택은 별도 결정**: PR #6의 두 파일을 제품 기준점에 반영하려면 부모 소스 SHA와 회귀 검증 결과를 확인한 별도 변경으로 제안합니다. canonical의 결함 기준 소스를 몰래 교체하지 않습니다.

## 포함·제외와 CI

포함 대상은 기존 기능을 재현하는 소스·고정 의존성 파일·테스트·비밀 없는 매니페스트 예제·허용 필드 Replay 자료입니다. `.opsloop/`, `reports/`, node_modules, 빌드 결과, 원본 ZIP 전체, kubeconfig, 키·토큰·환경 인증 파일은 소스 PR에서 제외합니다. ignore 규칙만 믿지 않고 source scan과 최종 diff 검토를 수행합니다.

`m3-local.yaml`은 로컬에 준비한 검증 workflow입니다. 게시 전 실제 trigger·권한·변경 파일을 검토합니다. 기존 `ci.yaml`에는 main push에서 ACR publish로 이어지는 job이 있으므로 M3 소스 반영 승인이 이미지 게시 승인까지 자동 확대되지 않습니다. 원격 CI가 실행되지 않았으면 `NOT_RUN`으로 표시합니다. 로컬 테스트 PASS를 GitHub Actions PASS로 바꾸지 않습니다.

## 승인 패킷과 복원

제안 branch는 `feat/m3-integrated-exhibition-20261027`, Draft PR 제목은 **Integrate verified Runtime and Engineering exhibition platform**입니다. 아직 branch 게시·stage·commit·push·PR 생성은 수행하지 않았습니다. 기존 PR #6과 별개인 플랫폼 통합 검토안입니다.

승인 시 정확한 원격 base SHA, 제안 branch, 변경 파일 목록과 SHA-256, 로컬 검증 receipt, secret scan, workflow 차이, 게시 영향, PR 제목·본문 초안을 제시합니다. stage/commit/push 대상은 새 격리 작업공간으로 제한하며 canonical index를 보존합니다.

게시 후 되돌려야 한다면 이미 공개한 이력을 강제로 삭제하거나 force push하지 않습니다. 미merge PR은 승인된 branch/PR을 닫는 방안, merge된 변경은 검토된 revert PR을 제안합니다. 클러스터 복원은 Git 복원과 별도 승인·절차입니다.
