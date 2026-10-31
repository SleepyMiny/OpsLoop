# M3 5~8분 전시 Runbook

관람객에게 두 복구 루프가 어디서 연결되고 어디서 멈추는지를 실제 증거로 설명합니다. 기본 시연에는 장애 주입·Enforce·모델 재호출·PR 변경·배포가 없습니다.

## 최신 실제 성공 회차를 설명하는 방법

`terminal-handoff-execution/final-result.json`에 연결된 이번 성공 회차는
Runtime 조치 1회 후 동일 오류가 남아 `FAILED / RUNTIME_BUDGET_EXHAUSTED`에
도달했습니다. 이는 Runtime의 복구 한계이며 전체 회차 실패를 뜻하지 않습니다.
같은 run의 `STOP_CONFIRMED`·증거 봉인 후 실제 모델 요청 1회와 독립 검증을 거쳐
`VERIFIED_PENDING_REVIEW`가 됐고, 별도로 정리·baseline·Controller 복원을 확인했습니다.

종료된 실행은 **실행 기록 · Replay · 공개 근거**에서 정확한 run을 선택합니다.
Runtime 조치 → 동일 결함 잔존 → 강한 정지 → 봉인 → 모델 요청 → 수정안 →
독립 검증 → 정리 완료를 해당 run의 이벤트로 설명합니다. 원본의 빈 주문은 HTTP 500,
후보는 같은 입력에서 HTTP 200이며 후보 검사 15개가 통과했습니다. diff와 입력 해시는
서버에서 해당 run에 공개를 허용한 자료만 펼쳐 봅니다. PR 미게시·앱 미배포 상태입니다.
이 성공 회차에 과거 B1의 PR #6이나 수정안을 붙이지 않습니다.

Live는 승인된 현재 관측, 종료 후 마지막 관측은 시각이 고정된 과거 snapshot,
실행 Replay는 저장한 사건 순서입니다. 이번 화면 점검은 별도 loopback 미리보기에서
저장 자료만 읽으며 기존 4173/4174·클러스터·Runner에 요청하지 않습니다.
전체 순간을 화면으로 관측하지 못했다면 원장에서만 확인한 단계와 구분합니다.
수정된 화면 빌드는 성공 당시 화면 빌드와 별도 후보이며, 실환경 성공에 사용했다고
소급 기록하지 않습니다. 검증된 후보의 통합 검사와 남은 미검증 범위는 수용 표를 따릅니다.

## 시작 전

1. 최종 수용 표와 `verification.json`에서 실행한 검증과 미실행 항목을 확인합니다. Demo A/B 원본 ZIP의 해시를 대조한 파생 자료를 사용합니다.
2. `npm --prefix frontend/dashboard run build` 후 `python3 scripts/m3-serve.py --mode replay`를 실행합니다. Live도 보여주려면 [운영 Runbook](deployment-runbook.md)의 읽기 전용 Live launcher를 사용합니다.
3. 노트북에서 `http://127.0.0.1:4173/`를 열고 글꼴·화면 크기·버튼·근거 링크가 보이는지 확인합니다. 전시 모니터에서는 브라우저 확대를 조절하고 내용이 잘리지 않는지 점검합니다.
4. 인터넷이 있는 경우 PR #6을 별도 탭에 열어둡니다. 인터넷이 없으면 Replay에 보존된 PR URL·commit·Draft receipt로 설명하고 원격 현재 상태는 미조회라고 말합니다.
5. Live Node/Policy/Pod 상태가 설명과 다른 경우 실제 상태를 우선합니다. 이전 관측 수치를 발표용 값으로 고정하지 않습니다.

## 초기 Demo A/B의 역사적 7분 Replay 흐름

| 시간 | 화면과 조작 | 설명과 제시할 근거 |
| --- | --- | --- |
| 0:00–0:50 | 기본 Overview, 상단 Exhibition 표시 | “기존 AMD64 Control Plane·물리 Worker·Azure ARM64 Worker를 연결한 관제 화면입니다. 지금 표시하는 카드가 과거 검증 기록인지 현재 Live인지 먼저 구분합니다.” |
| 0:50–1:30 | Live → scope A. 연결 불가면 생략하고 Replay를 명시 | 실제 Node Ready, 관측 시각, CPU/메모리 결측 여부, Policy enabled 상태를 확인합니다. 모든 노드가 Ready라는 결론은 화면의 현재 관측이 있을 때만 말합니다. |
| 1:30–2:50 | `/?source=replay&view=runtime`, “기록 재생” | Demo A readiness 장애 → 감지 → 후보 평가 → ARM64 선택 → Deployment 변경 → 새 Pod/Scheduler/Endpoint → 직접 Service 회복을 따라갑니다. 후보의 CPU·메모리·requests·arch 근거를 보여줍니다. |
| 2:50–3:30 | “일시 정지”, 서비스 회복·안전 종료 이벤트 | HTTP 13회 연속 정상, 32.42초 안정화, Policy disabled gen8을 근거 파일과 함께 확인합니다. “물리 서버 전원 차단을 검증한 것은 아닙니다.” |
| 3:30–4:50 | `/?source=replay&view=engineering`, “기록 재생” | 빈 주문의 HTTP 500 `QUOTE_DIVISION_BY_ZERO`, Runtime 재배치 후 동일 오류, budget exhausted, Guard STOP_CONFIRMED, Incident Evidence Bundle을 설명합니다. Runtime 한계가 개발 단계로 이어집니다. |
| 4:50–5:50 | 검증·PR 이벤트, 근거 링크 | 실제 Luna/Medium/ChatGPT 호출 1회, 수정 파일 2개, 수정 전 FAIL과 독립 검증 15/15 PASS를 확인합니다. [실제 Draft PR #6](https://github.com/SleepyMiny/OpsLoop/pull/6)을 엽니다. |
| 5:50–6:30 | Evidence → Demo B | Incident UID/run/epoch, bundle hash, commit `7e38e2b0196b726140c157b8d51b0fd5d33896da`의 연결을 보여줍니다. “PR_CREATED이며 merge·운영 배포·서비스 복구를 선언하지 않습니다.” |
| 6:30–7:00 | Live/Replay 모드 비교와 제약 | Live는 현재 관측, Replay는 봉인된 과거입니다. 원격 CI NOT_RUN, 수정본 native ARM64 실행·운영 배포 미실증, 전원 차단 및 runtime-v1 전체 증명 미실증을 밝힙니다. 선택적 Live Engineering 연결이 켜져 있어도 PR receipt는 과거 증거이고 Pod 상태는 현재 관측임을 구분합니다. |

5분 시연은 후보 상세·Evidence 상세를 줄이고, 8분 시연은 원본 파일 하나를 열어 해시·UID·시각을 직접 대조합니다. 재생의 단계당 5초는 설명용 간격입니다. 화면에 표시된 원본 사건 시각과 실제 소요 시간을 대체하지 않습니다.

## 조작표

- 상단 `Live`, `Replay`, `Exhibition`: 데이터 의미를 명시적으로 선택합니다.
- 기록 화면 `Overview`, `Runtime`, `Engineering`, `Evidence`: 설명 단계로 이동합니다.
- `기록 재생`, `일시 정지`, `이전`, `다음`, 재생 위치 slider: 원본 사건 순서를 탐색합니다.
- 근거 파일 링크: 검토된 원본 또는 개인정보 최소화 파생 파일을 확인합니다. 원본 SHA와 다운로드 SHA·변환 설명을 구분합니다. 전체 ZIP과 모든 로그를 브라우저에 공개하지 않습니다.
- `?source=simulation`은 개발용 합성 시뮬레이션이며 이 전시의 성공 증거로 사용하지 않습니다.

## 비상 대체 시연

API가 끊기면 “현재 관측 연결이 끊겼습니다”라고 말하고 마지막 관측 시각을 보여준 후 Replay를 직접 선택합니다. 과거 성공을 자동으로 Live healthy로 바꾸지 않습니다. Control Plane이 접근 불가해도 이미 빌드한 Replay는 로컬 정적 서버로 재생합니다.

인터넷이 끊겨 PR을 열 수 없으면 로컬 Engineering/Evidence 화면의 PR receipt·URL·commit을 보여줍니다. 원격 상태를 방금 재확인했다고 주장하지 않습니다. 앱이 중단되면 같은 launcher를 다시 실행하고 저장된 URL로 돌아갑니다. 포트 충돌은 기존 프로세스 소유자를 확인한 뒤 해결하며 운영 서비스나 Controller를 임의 재시작하지 않습니다.

전시 종료는 `Ctrl+C`로 로컬 launcher와 그 자식 BFF만 종료합니다. Policy 변경·워크로드 복원·Azure VM 종료는 이 명령에 포함하지 않습니다. 운영 자원에 조치가 필요하면 정확한 대상·영향·복원 방법을 제시한 별도 승인을 받습니다.

## 통합 Live 실행 관제 (로컬 후보)

새 화면은 서버의 capability를 먼저 확인합니다. Live launcher에서는 현재 관측과 실행
관제가 기본이며, Replay launcher에서는 Live 버튼이 비활성화됩니다. 관측 연결 실패는
서비스 HTTP 장애와 별도로 표시합니다. 기존 봉인 기록은 보조 Replay에 그대로 남습니다.

실행 기능은 기본 비활성입니다. 서버에서만 해시로 고정한 승인 계획 catalog와 영구
원장을 지정해야 준비/실행/중지 버튼을 사용할 수 있습니다. 브라우저는 계획 ID와
중복 방지 request ID만 보내며 명령·자격증명·UID를 새로 지정할 수 없습니다.

```
python3 scripts/m3-serve.py --mode live --kubeconfig <server-only-file> \
  --bff-bin <built-dashboard-bff> --control-config <approved-catalog.json> \
  --control-config-sha256 <exact-sha256> --control-state <private-durable-directory>
```

이 명령의 실환경 관측 및 실험은 별도 승인 대상입니다. 승인 전 로컬 테스트에서는
가짜 전송 계층과 loopback 브라우저 fixture만 사용합니다. 실제 복구·실제 모델 실행의
증거로 사용하지 않습니다. 새 VM이나 운영 클러스터를 만들지 않습니다.

Catalog schema는 `opsloop-exhibition-control/v1`이며 `plans`는 서버 전용 절대 경로와
원본 SHA-256의 목록입니다. 각 `opsloop-exhibition-plan/v1`에는 `planId`, 새
`authorizationId`, `approvalReference`, `track`, `source: measured`, `expiresAt`,
`totalSeconds`, `binding: {runID, epoch, policyUID, deploymentUID}`가 필요합니다.
Runtime의 상세 필드는 `tools/exhibition_runtime.py`의 `settings`, Engineering은
`tools/exhibition_engineering.py`의 `FIELDS`와 `_args`가 엄격하게 검증합니다.
승인 문서 없이 과거 계획을 복제하여 실행 권한으로 사용하지 않습니다.

Runtime 준비는 현재 UID·정상 baseline을 읽어 확인합니다. 실행은 기존 Guard를
먼저 확보한 뒤 정확한 Policy에 제한된 권한을 적용하고 readiness 장애를 한 번만
요청합니다. Controller 판단, 후보 평가, 서비스 요청, Endpoint 결과는 실제 관측으로만
진행합니다. 중지 요청과 안전 종료 확인은 서로 다른 상태입니다. 종료가 불명확하거나
서버가 실행 중 재시작되면 원장에 `UNCONFIRMED_TERMINATION`을 보존하고 다음 실행을
차단합니다. 외부에서 기존 Guard/Finish로 실제 상태를 조정하고 검토하기 전 원장을
삭제하거나 상태만 덮어써서 재개하지 않습니다.

기존 단일 실행 모드의 자원 조건은 Controller의 `--resource-conditions-file`과
`--resource-conditions-sha256`에 고정합니다. 아래 반복 모드는 서명된 실행 permit에
조건을 결속하며 두 모드를 혼용하지 않습니다. 실측은 변경하지 않고 CPU floor와
가용 메모리 ceiling을 후보 평가의 복사본에만 적용합니다. UI는 실측과 적용값을
구분합니다. 파일 SHA는 원본 bytes, 이벤트 `configSHA256`은 파싱한 Go JSON의
정규화된 내용이므로 두 해시는 목적이 다릅니다. CRD가 새 증거 필드를 보존하지 않거나
실측·Node UID·run·권한·만료가 확인되지 않으면 실제 조치를 거부합니다.

Engineering은 기존 수집된 Runtime 실패·정지 증거를 검증하고 독립 baseline 검사를
수행합니다. 모델·최대 호출 수(1–3)·총시간(최대900초)은 별도 해시 고정 실행 계획에서
정합니다. 검증 실패에 따른 재수정도 같은 예산 안에서만 수행하며 이전 attempt의
증거를 덮어쓰지 않습니다. 모델 원문과 인증정보는 공개 이벤트에 포함하지 않습니다.
최종 성공은 `VERIFIED_PENDING_REVIEW`이며 배포나 서비스 복구를 의미하지 않습니다.
Publisher의 준비·검토·승인 경계는 별도로 유지하고 이 서버에는 GitHub 쓰기 API가 없습니다.

완료된 실행은 같은 화면의 실행 선택과 이벤트 cursor로 다시 봅니다. 새로고침은
현재 원장과 sequence에 재연결하며 실행을 자동으로 시작하지 않습니다. 다음 시연은
정지·baseline 복원·프로세스 종료·증거 보존·정리가 모두 확인된 뒤 고정 Policy UID와
새 run/epoch·새 권한 및 현재 Deployment/Pod/Node UID를 검증한 계획으로 준비합니다.
Policy 누적 상태와 기존 Runner 실패 원장은 지우지 않습니다.

코드 diff 공개는 별도 검토한 정확한 SHA에 한정합니다. 선택적인
`--public-artifacts <catalog> --public-artifacts-sha256 <sha>`로 허용된 diff/checks만
전달하며 브라우저도 내용 해시를 대조합니다. 비공개 원문 로그나 모델 rationale은
직접 제공하지 않습니다. 공개 승인 전에는 검증 상태와 공개 검토 대기만 표시합니다.


### 실측 연결 전 후보 화면

기존 Node 도구를 명시하여 `frontend/dashboard`에서
`"$OPSLOOP_NODE" node_modules/vite/bin/vite.js build`를 실행합니다.
저장소 루트에서 `python3 scripts/m3-serve.py --mode disconnected --port 4174`로 엽니다.
이 모드는 Live 화면을 유지하지만 BFF·Kubernetes·모델에 연결하지 않고 실측 미설정과
실행 승인 대기를 표시합니다. 기존 4173 Replay 서버는 그대로 유지합니다.

### 같은 run의 Demo B Runtime → Engineering 연결

새 실행 계획의 `runtimeCapture`를 지정하면 사전 bundle 수동 공급을 사용하지 않습니다.
`tools/exhibition_handoff.py`가 기존 Demo B Guard trial 코어를 새 전용 디렉터리에서
실행하고, 실제 원본 capture와 강한 정지 receipt를 검증한 뒤 기존 sealer를 호출합니다.
새 bundle 해시를 같은 run/epoch/Policy UID/Deployment UID와 모델 계획에 결속하여
Runner에 직접 전달합니다. capture·봉인·실행계획 파일을 운영자가 중간에 옮기지 않습니다.

`runtimeCapture`의 정확한 필드는 `root`, `plan`, `planSHA256`, `setupReceipt`,
`setupReceiptSHA256`, `provenance`, `provenanceSHA256`입니다. root는 새 실행 전용
0700 디렉터리입니다. Coordinator/Runner의 기존 중복 방지 원장 경로는 유지합니다.
Runtime plan은 재사용한 코어 자체와 helpers·snapshot·실제 이미지 provenance·새
Controller scope 설정 receipt를 승인 해시로 고정해야 합니다. UI는 setup을 실행하지 않습니다.
반복 모드는 `STATIC_CONTROLLER_READY_POLICY_DISABLED` receipt와 고정 Policy UID를 사용하고,
현재 누적 incidentSequence·generation을 다음 실행에 결속합니다. legacy setup의 새 Policy
생성 경로를 반복 실행에 사용하지 않습니다.
Demo A 보호 대상의 Policy 이름·generation·spec 역시 현재 값에 정확히 고정합니다.

이 연결의 모델 예산은 기존 Runtime 계획의 한도에 맞춰 Luna/medium/ChatGPT 1회,
coordinator의 Runtime+Engineering 전체 상한 900초와 절대 만료를 적용합니다.
Runtime 자체는 최대600초입니다. 취소·기한 초과는 새 조치를 차단하고 기존 Guard가
disable-only 정지를 수행합니다. 정지 또는 봉인이 불명확하면 Runner는 호출하지 않고
다음 실행을 차단합니다. 실패 시 자동 재주입하지 않습니다.
Runtime의 결함 설정을 원복하는 Deployment 변경은 Guard의 disable-only 권한과
별도이며, 승인된 baseline rollback을 확인한 뒤에만 다음 시연을 준비합니다.


읽기 전용 Live의 일시 승인에는 `--observe-until <UTC RFC3339>`를 지정합니다.
현재 launcher는 BFF Pod 조회를 승인된 세 namespace로 제한하며 해당 요청량은 전체
클러스터 합계가 아님을 표시합니다. 만료 시 새 상위 GET을 거부하고 BFF·Prometheus
proxy 자식을 종료합니다. Dashboard는 마지막 snapshot과 원래 관측 시각을 남기고
연결 종료를 표시하며 반복 polling을 멈춥니다. 실험 control catalog는 별도입니다.

### 고정 Policy의 반복 실행 수명주기 — 로컬 후보

Controller를 한 번 `--exhibition-authority-file` 모드로 설치하면 고정된 두 대상의
UID와 Ed25519 공개키만 읽습니다. 기존 `--execution-scope-file` 및 외부 자원 조건
파일과 혼용할 수 없습니다. 회차별 Controller 재배포·Policy 생성은 없습니다.
호스트의 `tools.exhibition_permit` CLI만 정확한 payload/승인 해시, 비공개 서명키와
영구 발급 원장을 사용합니다. 브라우저에는 발급·서명 API가 없습니다.

각 permit은 현재 Controller instance, 증가하는 ordinal, run/epoch, Policy generation/hash,
기존 incidentSequence의 다음 Incident, 최대 Incident/조치 각 1회, 만료, 직전 완료 증거
해시에 결속됩니다. CPU floor도 이 서명 안에 포함됩니다. Demo B 모델은
Luna/medium/ChatGPT 최대 1회·Runtime+Engineering 최대 900초로 제한합니다.
같은 실패의 새 시연에는 새 승인이 필요하며 운영 장애의 자동 중복 방지는 유지합니다.

처음 전환할 때 `tools.exhibition_migration`은 별도 승인된 정확한 기존 객체·UID/RV·
내용 해시만 처리합니다. 정지·baseline·모델 종료 확인 후 원본을 지속 작업공간에 복사하고
해시를 확인한 다음 terminal 객체만 정리합니다. 고정 Policy와 누적 상태는 보존합니다.
첫 대상별 migration 완료 증거와 전체 직전 실행 완료 증거가 모두 있어야 다음 권한을
발급할 수 있습니다. 기존 정책·증거를 새 run으로 다시 라벨링하지 않습니다.

매 실행 종료 시 `tools.exhibition_lifecycle`은 현재 generation의 FenceClosed,
baseline spec/Ready, Guard·모델·sandbox 종료, archive 무결성, 임시 객체 부재를 확인합니다.
Incident 최초 관측 UID를 보존하며 이름만 같은 교체 객체는 채택하지 않습니다.
활성·다른 run·미보존 객체는 삭제하지 않고 DELETE에는 UID/RV 조건을 사용합니다.
ConfigMap은 현재 참조가 없어야 하며 Job은 terminal이어야 합니다. API 단절이나 중간
재시작이면 새 실행은 차단하고 기존 run의 확인·정리만 재개합니다. 장애·모델은 재실행하지 않습니다.

Dashboard의 `다음 시연 준비`는 이 완료 영수증과 archive 재검증에 연결됩니다.
호스트의 선택적 `planInbox: {path, publicKey}`는 0600 단일 pointer `{path, sha256}`로
새 승인 계획을 전달합니다. 현재 pointer와 서명은 prepare/start에서 다시 확인하며,
철회된 계획은 시작할 수 없습니다. append-only `plan_pins`는 재시작 후 정리에 필요한
과거 참조를 보존합니다. 서버/Controller를 매번 재시작할 필요가 없습니다.

정리 후 Kubernetes 실행 객체는 남기지 않지만 로컬 원장·원본 증거·Replay는 계속
보존하므로 디스크 사용량은 실행 횟수에 따라 증가합니다. 용량 부족은 새 실행을 차단할
운영 조건이며 원장/증거 삭제나 Quota 증가로 해결하지 않습니다.
고정 Policy UID·개수와 누적 이력을 유지한 실제 A 두 회차 및 B Runtime→Engineering
성공 기록은 보존되어 있습니다. 회차 사이 Controller 복원·재전환이 있었으므로 동일
instance 연속 반복 실증은 아닙니다. 수정 후보의 native ARM64 실행·앱 배포도 미검증입니다.
