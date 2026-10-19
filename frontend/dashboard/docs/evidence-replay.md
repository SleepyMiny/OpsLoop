# Evidence Replay v1

`opsloop-evidence-replay/v1`은 **과거 ObserveOnly 실증의 공개 가능한 파생 입력**이다. 실행 스크립트나 재적용 가능한 CRD bundle이 아니다. 원본 증거는 비공개 영구 보관소에 유지한다.

```text
schemaVersion: opsloop-evidence-replay/v1
experiments[]:
  id, title, verification, limitations[]
  provenance[]: {file: 원본 basename, sha256: 원본 파일 SHA256}
  milestones[]: {label, at?: epoch-ms, elapsedSeconds?: number}
  frames[]:
    label
    snapshot: {observedAt, cluster, workload, policy, activeIncident: null, recentIncidents, http}
```

공개 파일은 `src/data/replay/evidence.json`, 오프라인 생성기는 `scripts/export-dashboard-replay.py`다. 생성기는 두 원본 archive의 SHA256SUMS를 먼저 검사하고 **읽은 각 파일이 checksum 목록에 포함됐는지도** 확인한다. 원본 디렉터리 내부 출력과 서로 다른 기존 출력의 덮어쓰기를 거절한다.

```bash
python3 scripts/export-dashboard-replay.py \
  --imagepull /private/path/to/imagepull-evidence \
  --nodenotready /private/path/to/nodenotready-evidence \
  --output /tmp/reviewed-replay.json
```

출력 검토 후 명시적으로 공개 파일을 갱신한다. 원본 경로·로그·kubeconfig·SSH 키·Secret·인증 토큰·Pod 환경 변수는 파생 형식에 넣지 않는다. 포함되는 필드는 Policy 규칙/상태, Incident 분류·terminal phase·Controller Decision의 원본 측정값/점수/선택, Deployment 리소스 요청/digest, 실제 보존 시각과 운영자 복원 milestone으로 제한한다. 객체 UUID는 일관된 pseudonym으로 치환한다. 원본 name/node명·규칙 signature·hash는 증거 해석용으로 유지한다. 이것은 임의 archive에 대한 범용 DLP/비밀 탐지기가 아니므로 새 실험을 추가할 때 공개 필드 검토가 필요하다.

검증된 두 기록:

| 기록 | 실제 결과 | 검증하지 않은 것 |
| --- | --- | --- |
| IMAGE_PULL | CONFIGURATION_FAILURE / IMAGE_PULL → MANUAL_REQUIRED / CLASS_NOT_AUTOMATED, Policy circuit | 자동 복구, NodeNotReady, 후보 평가 |
| NODE_NOT_READY | INFRASTRUCTURE_FAILURE / NODE_NOT_READY → 저장된 후보 Decision → OBSERVED, attempts=0 | Enforce 자동 재배치/HTTP 복구 |

NODE_NOT_READY 복원 요청 약 69.05초, Node Ready·agent active 확인 약 89.27초는 **운영자의 start-only 복원** 시간이다. 자동 복구나 `T_recover`로 사용하지 않는다. IMAGE_PULL 정리 경고는 원본에 보존됐고 후속 NodeNotReady 및 Namespace 정리 증거와 구분한다. 두 시험의 잔여 리소스 정리는 완료됐으며 이 자료를 만들기 위해 장애를 재주입하지 않는다.

각 실험은 초기 disabled 상태와 정리 직전 terminal 상태 **2개 frame**만 제공한다. 첫 frame 시각은 Policy 생성 시각이며 조회 시각의 정밀한 증거가 아니다. terminal 시각은 보존 파일의 실제 `at`이다. 시점 사이를 보간하지 않는다. 마지막 frame Policy는 정리를 위한 disabled 상태일 수 있으며 Incident의 원본 Policy snapshot과 구분한다. 전체 Node snapshot·HTTP probe는 포함하지 않는다. 현재 Live 노드를 과거 기록에 섞지 않는다.

Replay의 `activeIncident`는 항상 null이다. UI는 Replay에서만 `recentIncidents[0]`의 종료 기록을 표시한다. Live에는 이 규칙을 적용하지 않아 현재 Incident가 없으면 그대로 없음이다. 저장된 Controller Decision은 현재 지표로 재평가하지 않는다. `OBSERVED`는 종료 상태이고 동작 중 lamp/복구 attempt를 만들어 표시하지 않는다.

오프라인 검증:

```bash
python3 -B -m unittest discover -s tests -p test_dashboard_replay.py -v
npm --prefix frontend/dashboard test -- src/data/replay/replaySource.test.ts
```
