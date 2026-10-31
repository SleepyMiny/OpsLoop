# M3 Git 소스 통합 계획

이 문서는 검토용 통합 계획입니다. M3 로컬 개발 중 stage·commit·push·PR 변경·merge를 실행하지 않습니다. 실행 승인에는 최종 변경 경로·소스 해시·기대 효과·복원 방법을 포함해야 합니다.

## 최초 검토 당시 기준점 (역사적 기록)

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

## 최초 검토 당시 PR 구조 제안 (역사적 기록)

1. **플랫폼 통합 PR**: 확인된 원격 main SHA `1c2740b9e337bf85c32dba849be36291d2ad3e39`를 기준으로, 승인받은 격리 작업공간에서 만듭니다. 실제 게시 직전에 원격 SHA가 변하지 않았는지 다시 확인합니다. 검토된 canonical 소스 manifest를 기준으로 Runtime/Guard/Observer/Engineering/BFF/React/테스트/문서를 포함합니다. 기존 미커밋 구현을 누락하지 않습니다.
2. **M3 전시·검증 변경의 리뷰 단위**: 위 PR 안에서 실행 엔진 보존 변경, 증거·BFF 계약, React 전시, 로컬 CI·문서를 검토 가능한 commit 묶음으로 정리합니다. 이것은 여러 중간 릴리스를 요구하는 계획이 아닙니다. 실제 commit은 별도 승인 후에만 만듭니다.
3. **기존 PR #6 유지**: 실제 AI 수정과 독립 검증의 증거 역할을 유지합니다. base를 임의 변경하거나 자동 merge하지 않습니다. 운영 결함을 유지한 Demo 기준점과 수정본의 차이를 계속 추적합니다.
4. **수정본 채택은 별도 결정**: PR #6의 두 파일을 제품 기준점에 반영하려면 부모 소스 SHA와 회귀 검증 결과를 확인한 별도 변경으로 제안합니다. canonical의 결함 기준 소스를 몰래 교체하지 않습니다.

## 포함·제외와 CI

포함 대상은 기존 기능을 재현하는 소스·고정 의존성 파일·테스트·비밀 없는 매니페스트 예제·허용 필드 Replay 자료입니다. `.opsloop/`, `reports/`, node_modules, 빌드 결과, 원본 ZIP 전체, kubeconfig, 키·토큰·환경 인증 파일은 소스 PR에서 제외합니다. ignore 규칙만 믿지 않고 source scan과 최종 diff 검토를 수행합니다.

`m3-local.yaml`은 로컬에 준비한 검증 workflow입니다. 게시 전 실제 trigger·권한·변경 파일을 검토합니다. 현재 `ci.yaml`의 publish는 main의 명시적 `workflow_dispatch`에서만 후보가 되며 verify/image/dashboard와 `acr-publish` 환경 승인을 요구합니다. PR 및 main push는 검증만 실행하고 publish는 SKIPPED입니다. 소스 반영 승인이 이미지 게시 승인까지 자동 확대되지 않습니다. 원격 CI가 실행되지 않았으면 `NOT_RUN`으로 표시합니다. 로컬 테스트 PASS를 GitHub Actions PASS로 바꾸지 않습니다.

## 승인 패킷과 복원

최초 제안 branch는 `feat/m3-integrated-exhibition-20261027`였습니다. 현재 로컬 정리의 기준 HEAD는 `b5a60a775d0e4660b320c4c04e331d3727afc098`이며, 그 위 누적 미커밋 변경을 검토합니다. 2026-10-30 읽기 전용 조회에서 main은 `16eb6aeb0300951a6461da1ded4d87a34f494b35` (PR #7 merge commit)이며 이 로컬 HEAD보다 1 commit 앞서고 파일 차이는 없습니다. PR #7은 merged, PR #6은 Draft/Open입니다. 게시 직전 원격 SHA를 다시 대조해야 하며, 기존 PR #6과 새 검증 후보 앱 수정은 플랫폼 변경과 분리합니다.

승인 시 정확한 원격 base SHA, 제안 branch, 변경 파일 목록과 SHA-256, 로컬 검증 receipt, secret scan, workflow 차이, 게시 영향, PR 제목·본문 초안을 제시합니다. stage/commit/push 대상은 새 격리 작업공간으로 제한하며 canonical index를 보존합니다.

게시 후 되돌려야 한다면 이미 공개한 이력을 강제로 삭제하거나 force push하지 않습니다. 미merge PR은 승인된 branch/PR을 닫는 방안, merge된 변경은 검토된 revert PR을 제안합니다. 클러스터 복원은 Git 복원과 별도 승인·절차입니다.

## 실제 성공 후 게시 검토 단위

최종 로컬 source manifest와 정확한 변경 목록을 승인 기준으로 사용합니다. 기본 구성은
하나의 플랫폼 PR 안에 (1) Controller 권한·재현 범위·자원 조건, (2) 실행기·정지·증거·
반복 수명주기와 연결 검사, (3) 관측·전시 화면과 문서 순서의 commit입니다. 중간 commit을
개별 배포하지 않으며 전체 후보 검증이 완료된 상태로 검토합니다. 실제 commit은 미수행입니다.

성공 run의 `candidate.patch`는 별도 앱 수정 검토 대상이며 이 플랫폼 PR에 자동 포함하지
않습니다. `.opsloop/`, `command-diagnostics/`, 원장·DB·서명키·토큰·kubeconfig·원본 ZIP·
원본 API 응답·브라우저 공개 catalog·머신별 실행 계획은 제외합니다. 게시 목록에는
합성임을 표시한 독립 테스트 fixture만 포함하고 실제 비공개 응답을 복사하지 않습니다.
기존 공개 운영 식별자와 머신별 경로가 남은 소스는 비밀정보와 별개로 명시하여
익명화 정책을 확정한 뒤 게시합니다. 완전히 익명화된 소스라고 주장하지 않습니다.

CI는 기존 `m3-verify.py`의 전체 Go 일반/race(envtest 필수), Python discover, Frontend
타입·Vitest·빌드와 cross-build를 실행합니다. Go suite의 Controller↔Python heartbeat·
기간 snapshot·terminal/handoff 연결 검사를 누락하지 않습니다. 실환경·모델·native ARM64·
원격 게시 검사는 로컬 PASS로 대체하지 않습니다. workflow 권한·게시 정책은 변경하지 않습니다.

## CI 재현과 게시 직전 조건

설치 기준은 Python `.python-version` (3.14.8), `requirements-ci.txt`의 PyYAML 6.0.3
wheel SHA-256, Go 1.27.3, Node 24.22.0, setup-envtest v0.25.2 / Kubernetes 1.35.0입니다.
Python 설치 action도 commit SHA로 고정합니다. PyYAML wheel lock은 CPython 3.14의
Linux x86_64/aarch64용이며 다른 플랫폼 설치나 native ARM64 실행을 입증하지 않습니다.
공식 버전·패키지 출처는 [setup-python](https://github.com/actions/setup-python/tree/e797f83bcb11b83ae66e0230d6156d7c80228e7c)과
[PyYAML 6.0.3](https://pypi.org/project/PyYAML/6.0.3/)입니다.

```bash
# 위 버전의 Python/Go/Node를 먼저 설치합니다. 네트워크는 의존성 준비 단계에만 필요합니다.
python3 -m pip install --require-hashes --only-binary=:all: -r requirements-ci.txt
go mod download
npm --prefix frontend/dashboard ci --no-audit --no-fund
ci_envtest_assets=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.35.0 --bin-dir "$RUNNER_TEMP/envtest" -p path)
python3 -B scripts/m3-verify.py \
  --go "$(command -v go)" --node "$(command -v node)" \
  --gomodcache "$(go env GOMODCACHE)" --gocache "$RUNNER_TEMP/m3-cache" \
  --envtest-assets "$ci_envtest_assets" --output "$RUNNER_TEMP/m3-checks"
```

로컬에서는 `RUNNER_TEMP`를 새로운 전용 임시 디렉터리로 지정합니다. 실행기는 선택한
Python 경로를 자식 Go→Python 검사에도 전달하며 운영 인증 환경은 상속하지 않습니다.
실제 kubeconfig·`.opsloop` 자료는 입력이 아닙니다. envtest용 임시 키만 local API 검사에
사용합니다. `--only go-connections`는 필요한 5개 연결만 검사하는 부분 실행이며 전체
회귀 PASS를 뜻하지 않습니다. 기본 실행 및 `ci.yaml`의 Go JSON 로그에서는 정확한
package/test 이름의 5개 PASS를 요구하고 누락·SKIP·중복 결과·실패를 거부합니다.

필수 연결은 Go Policy 직렬화→Python, heartbeat 전송, source startup→첫 조치,
readiness/철회 경합, Runtime terminal→강한 정지→봉인→Engineering 호출 의도입니다.
모델 전송과 kubelet 상태는 테스트 대역이며 실제 모델·클러스터 검증이 아닙니다.
`OPSLOOP_REQUIRE_ENVTEST=1`을 유지합니다. 환경·버전 확인 실패는 검사 실패이며,
필수 연결이 빠져도 녹색으로 집계하지 않습니다.

Frontend의 `liveIntegration.test.ts` 1개는 기존 opt-in 제외입니다. 별도 읽기 전용
실환경 승인과 유효한 BFF가 있을 때만 `OPSLOOP_LIVE_BFF_URL`을 설정합니다. 이 검사는
3개 Ready 노드·ARM64 포함·ObserveOnly/Monitoring·현재 지표·Incident 없음이라는
특정 baseline을 요구하므로 임의 URL만 제공해 실행하지 않습니다. CI에는 URL이나
운영 자격증명을 주입하지 않으며 이 제외를 PASS로 계산하지 않습니다.

이전 전체 `m3-verify.py`는 Go `TestFinishFixPreStart/start-failure` fixture의 긴
shebang 경로 문제로 exit 1이었습니다. 부모 test의 fail event도 같은 실패입니다.
`cmd/demo-driver/executor_fix_boundaries_test.go` 수정 후 해당 전체 묶음의 일반/race와
해당 package vet는 통과했습니다. 이후 전체 race와 Python/Frontend 결과는 동일 소스
해시에 따라 재사용합니다. **최종 소스 전체 Go 일반 실행을 새로 통과했다는 주장이
아닙니다.** 원래 FAIL 로그와 수정 전후 해시·명령·재사용 근거는 비공개 검증 receipt에
연결합니다. 최종 게시 소스 전체 일반/race 검사는 게시 승인 후 원격 CI에서 확인합니다.

게시 시에는 새 격리 checkout을 확인된 main에서 만들고, source manifest의 정확한
변경만 적용합니다. 제안 branch는 `feat/repeatable-exhibition-release-20261030`이며,
3개 commit은 Controller/Guard 계약 → 실행기/수명주기 → 화면·문서·CI 준비 순서입니다.
CI 설정은 모든 연결 소스가 포함되는 마지막 commit에 둡니다. 단일 플랫폼 PR이며
PR #6과 새 앱 수정 후보는 포함하지 않습니다. 이 문서는 commit/push/PR 실행 기록이 아닙니다.

게시 후 새 PR HEAD의 verify/dashboard/image/M3 offline-checks와 publish SKIPPED를
확인합니다. 실패하면 원본 로그를 보존하고 원인을 분류하며, 실패를 무시한 merge·
이미지 게시·배포는 하지 않습니다. 추가 수정/push/CI 재실행은 승인된 범위 안에서만
수행하고 범위가 없으면 묶어서 요청합니다. workflow 권한·OIDC·main 수동 게시 및
환경 승인 조건은 이번 CI 준비에서 변경하지 않습니다.
