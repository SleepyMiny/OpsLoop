# P2-E Recovery Guard — 로컬 구현, 실환경 미검증

`demo-driver guard`는 별도 Linux 프로세스로 실행하는 제한된 복원 감시기다.
기존 `finish-run`은 여전히 synthetic-local/test-executable 전용이며
Policy stop 확인 → 복원 → 사후 확인을 유지한다. 이 문서는 실환경 실행 승인이 아니다.

## 코드와 명령 경계

`recovery_backend.go`의 운영 경로는 명시한 실행 파일에 다음 argv만 생성한다.

```
--connect qemu:///system domuuid <UUID>
--connect qemu:///system domstate <UUID>
--connect qemu:///system resume <UUID>
```

`qemu:///session`도 명시적으로 선택할 수 있다. 다른 URI, URI 옵션, 원격 host,
SSH, 기본 connection, 환경변수 대상 선택, VM 이름, start/destroy/reset/undefine/
suspend/list, shell/sudo와 임의 command 인터페이스는 없다. UUID는 소문자 정규형이다.
실제 실행 파일 경로·SHA-256·URI·UUID를 자동 검색하지 않는다.

정확히 일치하는 domuuid와 paused/running 상태만 처리한다. paused는 한 번의
resume intent를 durable 기록한 뒤 실행하고, running은 조회로 끝난다. shutoff,
부재, 알 수 없는 상태, 조회 실패·UUID 불일치는 ABORTED_MANUAL이다. resume의
exit 0 자체는 복원 확인이 아니다. 오류/timeout/응답 유실 후에도 남은 권한·작업
기한 안에서 새 domuuid/domstate 조회로 확인한다. intent 이후 중단한 epoch는
fresh 조회가 paused이면 재전송하지 않고 수동 확인으로 끝난다.

`recovery_command.go`는 기존 P2-D helper를 추출한 것이다. hash preflight,
4 KiB stdout 제한, context deadline, Start 전후 callback, Linux process group
종료·Wait·제한된 reap, 100 ms I/O grace를 공유한다. fixture alias와 기존 오류
문구를 보존했다. 자식에는 LANG/LC_ALL만 전달하며 HOME, proxy, kubeconfig,
libvirt 환경·운영 credentials를 전달하지 않는다. 실행 파일과 상위 경로를
다른 주체가 교체하지 못하는 배치가 전제다. hash 검사와 pathname exec 사이의
적대적 교체나 detached/uninterruptible 자식까지 격리하는 sandbox는 아니다.

## CLI와 승인 경계

설정 형식은 인접 `recovery-guard.example.json`을 참고한다. 모든 값은 placeholder다.
자동으로 실행 가능한 실환경 설정·서비스 관리자 파일은 제공하지 않는다.

```
demo-driver guard plan -config <CONFIG>
demo-driver guard owner -pid <EXPLICIT_LOCAL_EXECUTOR_PID>
demo-driver guard arm -config <CONFIG> -execute -confirm <CONFIG_HASH>
demo-driver guard watch -config <SAME_CONFIG> -execute -confirm <CONFIG_HASH>
demo-driver guard heartbeat -config <SAME_CONFIG> -execute -confirm <CONFIG_HASH> -sequence <N>
demo-driver guard disarm -config <SAME_CONFIG> -execute -confirm <CONFIG_HASH>
demo-driver guard status -config <SAME_CONFIG> -execute -confirm <CONFIG_HASH>
```

`plan` 또는 `-execute` 생략은 항상 UNARMED/commands=0이다. `owner`는 지정한
로컬 PID의 boot ID와 `/proc/<pid>/stat` startTicks만 읽는다. PID 단독으로 소유자를
판정하지 않는다. configHash는 파싱한 설정 구조의 canonical Go JSON SHA-256이다.

`arm`은 전용 private root와 대상 lock, 소유 실행기 identity/기한, 실행 파일 hash
준비를 확인하고 state 파일과 디렉터리 fsync가 끝나야 ARMED를 출력한다. 이어서
같은 **별도 CLI 프로세스**가 감시한다. arm의 준비 확인은 VM 존재나 실환경 등록을
입증하지 않는다. 미래 injector는 이 응답의 run/epoch/hash와 durable 상태·유효
기한을 확인해야 한다. 이번에는 injector/suspend를 구현하지 않았으므로 지연된
fault 차단을 보장하지 않는다.

`mode: live-libvirt`는 pinned 파일명이 virsh여야 하며 위 모든 실행 명령에 별도
`-live-authorization <ATTESTATION>`을 요구한다. attestation 필드는 다음과 같다.

```json
{
  "version": "opsloop-guard-authorization/v1",
  "configHash": "<EXACT_CONFIG_HASH>",
  "approvalReference": "<EXTERNAL_APPROVAL_REFERENCE>",
  "operator": "<APPROVING_OPERATOR>",
  "expires": "<UTC_TIME_AT_OR_AFTER_CONFIG_AUTHORITY_EXPIRY>",
  "scope": "dedicated-test-target-query-resume"
}
```

이 파일은 별도 명시적 운영자 승인 경계이며 서명 검증이나 권한 발급 시스템이
아니다. 실제 대상 등록·격리·libvirt 권한·승인 진위 확인은 미검증이다. confirm hash,
isolated 표기 또는 로컬 파일만으로 그 절차를 대체할 수 없다. synthetic-local은
정확히 지정한 합성 실행 파일을 위한 모드이며 실제 virsh 실행 허가가 아니다.

## 상태·기한·조정

UNARMED → ARMED → DISARMED 또는 RESTORING →
EMERGENCY_RESTORE_CONFIRMED / ABORTED_MANUAL.

run/epoch, 전체 설정 hash, URI/UUID, 허용 동작, 실행 파일 hash, 소유자 boot/PID/
startTicks, 권한 만료, hard deadline, 최대 시험 시간, heartbeat 간격을 함께 묶는다.
부트 내 CLOCK_BOOTTIME(절전 시간 포함)과 wall time을 arm 시 고정한다. hardTick은
명시한 deadline과 최대 시험 시간 중 빠른 쪽이다. heartbeat는 증가하는 sequence만
허용하고 hardTick/expiryTick을 변경하지 않는다. 유실된 heartbeat 허용 기간이 지난
뒤의 늦은 heartbeat는 감시를 다시 연장하지 못한다.

실행기 종료/identity 변경, heartbeat 상실, hard deadline은 비상 복원 계기다.
API/Controller Quiesced를 기다리지 않는다. 다른 boot, 역행한 boottime 또는
wall/boottime 차이 2초 초과는 기한 불명확으로 수동 확인하며 새 명령을 허가하지
않는다. 권한 만료 후에도 새 명령을 허가하지 않는다. 재시작은 `watch`로 같은
state를 읽으며 기한을 새로 계산하지 않는다. `arm` 재사용은 닫힌 epoch에도 거부한다.

단일 등록 대상은 **동일한 전용 root**를 사용해야 한다. 대상 URI/UUID hash별
flock으로 조정하고 run/epoch별 state를 유지한다. 감시 중에는 lock을 잡지 않는다.
한 작업의 상한은 6 × command timeout + 1초(권한 만료가 더 빠르면 단축), lock
재시도 상한은 6 × timeout + 2초다. lock 실패는 비성공이며 무한 대기하지 않는다.
finish-run 장기 journal lock을 사용하지 않는다. 별도 root를 임의 사용하거나
관리자가 직접 실행하는 작업까지 호스트 전역으로 조정하지 않는다.

협조하는 정상 복원도 이 짧은 대상 lock과 fresh 상태 확인을 사용해야 한다.
합성 정상 복원과 두 Guard의 경쟁은 시험하지만, 실환경 정상 finish-run backend
연결은 아직 없다. 기존 finish-run stop 조건을 우회하는 호출 경로는 추가하지 않았다.

heartbeat/disarm은 동일 config/run/epoch만 받고, heartbeat는 현재 owner identity도
확인한다. disarm은 조회 전과 DISARMED 저장 직전에 같은 판단 함수를 사용한다.
마지막 fresh moment의 clock/권한, hard deadline, heartbeat, boot/PID/startTicks와
실제 owner 생존, 기존 Emergency/RESTORING을 다시 확인해야 정상 해제한다.
조회 중 heartbeat/hard/owner 위반이 생기면 nonzero로 거부하고 Emergency와
RESTORING을 저장한다. 이후 watch는 기존 비상 경로로 이어지며 이미 running이면
resume하지 않는다. 권한 만료나 읽을 수 있지만 유효하지 않은 clock은 새 명령 없이
ABORTED_MANUAL로 기록한다. clock 자체를 읽을 수 없으면 새 시각이나 manual 이벤트를
만들지 않고 nonzero/수동 확인을 요구하며 기존 durable 상태와 책임을 보존한다.
직전 running 조회 사실은 정상 해제 자격이나 서비스/Controller 성공으로 승격하지 않는다.
최종 검사까지의 근거를 확인할 뿐 시계·프로세스 상태·파일 fsync 사이 원자성을 보장하지 않는다.
실행기가 사라졌다는 이유로 해제하지 않는다. 비상 전환 후에는 disarm으로 정상 결과로 바꿀 수 없다.
닫힌 state를 삭제하면 재arm 방지 근거도 사라지므로 외부 보존 정책이 필요하다.

## 실패·receipt·소비 한계

guard state는 별도 복원 receipt이기도 하다. atomic rename 전 file fsync, 이후
parent fsync를 수행한다. 정상 finish receipt나 summary를 수정하지 않는다.
command-intent, vm-start-attempt, 실제 vm-started, command-result, resume-intent,
effect-confirmed와 actor=recovery-guard를 구분한다. vm-started는 Start 성공의
관측이며 VM 효과 시각이 아니다. Start와 durable 기록 사이의 중단 공백은 남는다.

각 이벤트의 At은 로컬에서 그 이벤트를 생성·기록하기 위해 읽은 wall time이다.
resume-intent와 manual-review도 생성 지점에서 fresh moment를 읽으며 step 시작의
판단 시각을 재사용하지 않는다. disarmed는 최종 자격 검사에 사용한 fresh moment로
기록한다. sequence는 기록 순서를 나타내며 At은 명령 Start, VM 상태 전이 또는
fsync 완료 시각을 입증하지 않는다. wall time이 항상 증가한다는 조건은 없고 과거
At을 정렬·clamp·수정하지 않는다. 이벤트 생성 시 clock/read 또는 durable save가
실패하면 성공을 반환하지 않고 새 resume이나 후속 조회를 시작하지 않는다.
존재하는 durable intent와 책임을 보존하며 없는 manual/완료 이벤트를 채우지 않는다.
실패 이벤트를 재귀적으로 기록하거나 예전 시각을 fallback으로 쓰지 않는다.
기록할 수 없는 경우 오류로 그 한계를 반환한다. rename 후 directory fsync 실패는
새 파일이 이미 보일 수 있으므로 재시작은 반드시 실제 durable 파일을 다시 읽는다.

identity/boot/권한이 불명확하면 임의 대상을 복원하지 않는다. 이미 arm된 run에서
record 실패가 나면 nonzero로 끝나며 기존 durable 책임은 유지된다. 외부 재시작은
기존 intent를 먼저 읽고 fresh 조회로 판단한다. intent 기록 전 실패는 새 효과가
없고, intent 후 실패는 무조건 새 resume을 허가하지 않는다. 파일시스템 자체가
멈추거나 모든 기록이 실패하면 비성공을 디스크에 남길 수 있다는 보장도 없다.

비상 running 확인은 Controller 자동 복구·서비스 정상화·정상 cleanup·stop/fence의
증거가 아니다. `automaticSuccessEligible=false`, `consumerIntegration=UNINTEGRATED`
이며 비상 완료도 CLI nonzero다. StopProof나 complete actor coverage를 만들지
않는다. Observer/Acceptance의 현재 소비 계약에 비상 개입 연결은 **미통합**이다.
비상 receipt를 누락한 자료를 자동 성공으로 내보내면 안 된다. 기존 성공 receipt를
사후 수정하는 대신 별도 비상 사실을 함께 전달하고 독립 소비 계약을 검증해야 한다.

## 시험과 미검증 범위

`TestRecovery*`는 실제 backend에 fake runner를 주입하고, 빌드한 CLI는 고정 argv의
Python 합성 VM을 실행한다. VM 상태·호출 수는 fake executable이 기록하며 harness는
resume하지 않는다. SIGKILL/hang, Guard 재시작, 두 Guard, 정상 복원 경쟁,
실제 command timeout을 별도 프로세스로 시험한다. 시간 경계는 주입 clock을 쓴다.
테스트 자식은 allowlist 환경과 정리 상한을 사용한다. `P2E_CLI`가 없으면 별도 CLI
시험은 skip되므로 릴리스 검증에는 반드시 새 build를 지정한다.

기존 고정 Go/cache로 TestRecovery 범위의 test/race, package vet, CLI build와
조회 중 owner 종료/정상 disarm CLI 회귀를 수행한다. P2-E 수정은 공통 P2-D runner를
변경하지 않으므로 종료된 TestFinish 전체 시험은 반복하지 않는다. 일반 build CLI와
race 계측된 시험/helper 프로세스는 구분하며 실제 libvirt 실증 수는 0이다.
실제 virsh(`--version` 포함), libvirt/VM, 운영 Kubernetes, 서비스 설치, host 재부팅,
장애 주입, Enforce, remote CI, 배포는 실행하지 않는다. 외부 supervisor의 재시작과
실환경 대상 인가·권한은 미검증이다. Guard 자체 사망은 외부 재시작이 필요하다.
host 전체 장애·libvirt 복구 불능을 같은 host Guard가 극복한다고 주장하지 않는다.

INDEPENDENT_RECHECK_PENDING · LIVE_LIBVIRT_NOT_RUN · WATCHDOG_LIVE_NOT_PROVEN ·
LIVE_RUNTIME_NOT_PROVEN · REMOTE_CI_NOT_RUN · DEPLOYMENT_NOT_APPROVED · ENFORCE_BLOCKED
