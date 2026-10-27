# M3 5~8분 전시 Runbook

관람객에게 두 복구 루프가 어디서 연결되고 어디서 멈추는지를 실제 증거로 설명합니다. 기본 시연에는 장애 주입·Enforce·모델 재호출·PR 변경·배포가 없습니다.

## 시작 전

1. 최종 수용 표와 `verification.json`에서 실행한 검증과 미실행 항목을 확인합니다. Demo A/B 원본 ZIP의 해시를 대조한 파생 자료를 사용합니다.
2. `npm --prefix frontend/dashboard run build` 후 `python3 scripts/m3-serve.py --mode replay`를 실행합니다. Live도 보여주려면 [운영 Runbook](deployment-runbook.md)의 읽기 전용 Live launcher를 사용합니다.
3. 노트북에서 `http://127.0.0.1:4173/`를 열고 글꼴·화면 크기·버튼·근거 링크가 보이는지 확인합니다. 전시 모니터에서는 브라우저 확대를 조절하고 내용이 잘리지 않는지 점검합니다.
4. 인터넷이 있는 경우 PR #6을 별도 탭에 열어둡니다. 인터넷이 없으면 Replay에 보존된 PR URL·commit·Draft receipt로 설명하고 원격 현재 상태는 미조회라고 말합니다.
5. Live Node/Policy/Pod 상태가 설명과 다른 경우 실제 상태를 우선합니다. 이전 관측 수치를 발표용 값으로 고정하지 않습니다.

## 권장 7분 흐름

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
