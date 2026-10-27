# M3 도구와 오프라인 검증

실제 로컬 기준은 Go **1.27.3**, Node.js **24.22.0**, Python **3.14.8**이다. Go는 `go.mod`·`go.sum`, Dashboard는 `frontend/dashboard/package-lock.json`을 사용한다. Python 도구와 시험은 표준 라이브러리를 사용하며 별도 pip 패키지를 설치하지 않는다. CI의 `ubuntu-24.04` 기본 Python 3.12와 로컬 Python 버전은 각각 receipt에 기록한다. envtest는 Kubernetes 1.35.0의 `kube-apiserver`·`etcd`를 사용하며 실제 클러스터에 연결하지 않는다.

현재 장비에서 확인한 도구 위치:

| 항목 | 경로 |
|---|---|
| Go | `/home/sleepyminy/.local/share/opsloop/toolchains/go1.27.3-p3b/go/bin/go` |
| Node | `/home/sleepyminy/.local/share/opsloop/toolchains/node-v24.22.0-linux-x64/bin/node` |
| Go 모듈 캐시 | `/home/sleepyminy/Downloads/OpsLoop_Work/P3B_20261025_011718_KST/work/gomod` |
| Go 빌드 캐시 | `.opsloop/demo-a-20261025/work/gocache` |
| envtest | `.opsloop/demo-a-20261025/work/local-test-tools/envtest/controller-tools/envtest` |

위 절대 경로는 이 장비의 위치이며 이식 요구사항이 아니다. 다른 장비에서는 동일 버전 도구를 설치하고 아래 인자로 해당 위치를 전달한다. 캐시나 사용자 비밀정보를 소스에 포함하지 않는다. 새 장비의 최초 의존성 준비는 네트워크가 필요하다:

```bash
go mod download
npm --prefix frontend/dashboard ci --no-audit --no-fund
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2 use 1.35.0 --bin-dir /tmp/opsloop-envtest -p path
```

설치가 끝난 뒤 저장소 루트에서 실행한다. `--gomodcache`만 필수이며, `--go`·`--node`의 기본값은 PATH 검색 결과다. 명시 경로 사용을 권장한다. 출력 경로는 새 경로여야 하고 기존 검증 receipt는 덮어쓰지 않는다.

```bash
python3 -B scripts/m3-verify.py \
  --go /path/to/go1.27.3/bin/go \
  --node /path/to/node-v24.22.0/bin/node \
  --gomodcache /path/to/prepared/go-modules \
  --gocache /path/to/writable/go-build-cache \
  --envtest-assets /path/to/envtest \
  --output reports/m3/checks/local-run
```

`--gocache` 생략 시 출력 폴더에 새 캐시를 만든다. `--envtest-assets` 생략 시 envtest 미실행과 Go skip 목록을 명시한다. 전체 Controller 회귀 검증에는 이 인자가 필요하다. `--timeout`은 검사별 초 단위 제한이며 기본 3600이다. Go test/race 내부 제한은 패키지별 45분, CI job 전체 제한은 90분이다. `--list`는 실행 계획만 출력한다. `--only go-test --only python`처럼 검사를 선택할 수 있고, Go 테스트에는 새 Demo Driver 빌드를 자동 포함한다. 부분 선택 receipt의 PASS는 선택한 검사 범위만 뜻한다. Ctrl+C로 중단하면 현재 검사의 전체 프로세스 그룹을 종료하고 `INTERRUPTED` receipt를 남기며 후속 검사를 실행하지 않는다.

M3 로컬 Demo Driver 전체 race 검사는 1486.747초(약 24분 47초), 799개 테스트 이벤트로 PASS했다. 당시 제한은 25분이었으며 여유가 약 13초뿐이어서 이후 반복 검증의 제한을 위 값으로 늘렸다. 제한 조정으로 테스트 범위나 성공 판정은 바뀌지 않았고 전체 테스트를 다시 실행하지 않았다. 이 로컬 측정은 원격 CI 실행 증거가 아니다.

이미 통과한 패키지를 반복하지 않고 확인하려면 `--only go-race --go-package ./cmd/demo-driver`처럼 지정한다. `--go-package`는 반복 가능하며 Go vet/test/race에만 적용한다. 기본값은 `./...`이고 실제 선택 목록은 receipt의 `goPackages`에 기록한다. `go-race`는 선택 패키지의 전체 테스트를 race detector와 함께 실행하므로 별도의 비-race 재실행 없이 해당 범위의 테스트·race 결과를 함께 제공한다.

실행기는 소스 자격증명 패턴 검사, Demo Driver 새 빌드, Go vet·전체 test·전체 race, Python unittest, Frontend typecheck·Vitest·Vite build, Controller/BFF/Demo A/Demo B 앱의 AMD64 및 ARM64 빌드를 실행한다. 각 명령·종료 코드·시각·로그 해시를 `verification.json`과 개별 `.log`에 남긴다. Go의 pass/fail/skip은 테스트 이벤트 수이며 고유 시나리오 수로 합산하지 않는다. ARM64는 교차 컴파일이며 ARM64 장비에서의 실행 증명이 아니다. Docker 이미지 빌드·push·배포는 이 명령에 없다.

검증 환경은 명시 allowlist로 구성하고 GitHub/Azure/AI 인증값과 proxy를 상속하지 않는다. `KUBECONFIG`는 존재하지 않는 검증 경로, `USE_EXISTING_CLUSTER=false`, `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`이다. Go 테스트용 CLI 경계에는 방금 빌드한 바이너리를 전달한다. 일부 시험은 loopback HTTP/TLS 서버를 사용한다. 이 설정은 의존성의 오프라인 해석이며 OS 수준 외부 통신 차단을 증명하지 않는다.

소스 변경 범위 검사는 최초 manifest를 기준으로 명시 허용 경로를 전달한다:

```bash
python3 -B scripts/m3-source-scan.py \
  --baseline reports/m3/baseline.json \
  --allow frontend/dashboard --allow internal/dashboardbff \
  --allow cmd/dashboard-bff --allow tools/engineering_dashboard_bridge.py \
  --allow tools/m3_evidence.py --allow tests --allow scripts \
  --allow README.md --allow .dockerignore --allow docs/m3 --allow .github/workflows/m3-local.yaml \
  --output reports/m3/source-scope.json
```

허용 목록은 실제 검토 범위에 맞춘다. 검사기는 현재 Git tracked + nonignored untracked 파일을 검사하며 Git index를 수정하지 않는다. 비밀정보 패턴 검사는 고확률 패턴 위주이며 모든 형태의 비밀정보 부재를 증명하지 않는다. 출력에는 발견 경로와 규칙만 기록하고 발견된 값을 출력하지 않는다. 소스 범위 검사와 AI repair 허용 파일 검사는 서로 다르며, 기존 Engineering Python 회귀 시험이 repair 파일/import/의존성 경계를 계속 검증한다.

`.github/workflows/m3-local.yaml`은 로컬 준비된 별도 workflow다. 의존성 준비 단계만 네트워크를 사용하고 본 검증은 운영 서비스 없이 실행한다. 기존 `ci.yaml`의 게시 job을 호출하지 않는다. 이 파일은 게시 승인 전에는 원격 실행되지 않으며, 로컬 PASS를 GitHub Actions PASS로 기록하지 않는다. 실패 시 해당 로그를 확인하고 소스/캐시/환경을 수정한 뒤 새 출력 폴더에서 필요한 검사만 재실행한다. 클러스터 자원을 변경하는 복원 작업은 필요하지 않다.
