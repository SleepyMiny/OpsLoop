import type { EvaluationResult } from '../../domain/evaluate';
import type { Narration } from '../../domain/types';
import { actionTypeLabel, rejectReasonLabel } from '../../domain/labels';
import type { ScenarioInfo } from '../source';
import { NODE_SEEDS } from './baseline';
import type { Cue, ScenarioContext, ScenarioScript } from './engine';

/**
 * Demo Mode scripts. Times are simulated seconds; `pace` compresses the boring
 * stretches (Kubernetes grace periods, deadlines) and the UI shows the multiplier.
 * Outcomes are not hard-coded: the narration for a decision is built from the
 * evaluator result, so it can only say what the policy actually computed.
 */
export interface DemoScenario extends ScenarioInfo {
  script: ScenarioScript;
}

const PRIMARY = 'sarmubuntuserver';
const LAPTOP = 'opsloop-control';
const AZURE = 'azure-arm64-worker';

function nodeLabel(name: string): string {
  return NODE_SEEDS.find((n) => n.name === name)?.displayName ?? name;
}

function describeRejections(result: EvaluationResult): string {
  const parts = result.candidates
    .filter((c) => c.rejectReasons.length > 0 || c.exclusion)
    .map((c) => {
      if (c.exclusion === 'FAILED_NODE') return `${nodeLabel(c.nodeName)}는 장애 노드라 제외`;
      return `${nodeLabel(c.nodeName)}는 ${c.rejectReasons.map(rejectReasonLabel).join('·')}로 탈락`;
    });
  return parts.join(', ');
}

function describeSelection(result: EvaluationResult): string {
  if (!result.selected?.score) return '적격 후보가 없습니다.';
  return `${nodeLabel(result.selected.nodeName)}(${result.selected.arch.toUpperCase()}) ${result.selected.score.total.toFixed(1)}점으로 선택. ${actionTypeLabel(result.actionType)}`;
}

/** Shared opening: Physical Primary loses power, Kubernetes notices, OpsLoop confirms. */
function nodeFailurePrelude(loadCue: Cue): Cue[] {
  return [
    {
      at: 0,
      chapter: '평상시',
      run: (ctx) => {
        ctx.pace(2);
        ctx.narrate({
          title: 'Demo API는 물리 서버에서 실행 중',
          body: 'OpsLoop는 Kubernetes API로 노드 상태를, Prometheus로 실제 CPU·Memory 사용량을 함께 지켜봅니다.',
          focus: 'cluster',
        });
      },
    },
    loadCue,
    {
      at: 14,
      chapter: '장애 발생',
      run: (ctx) => {
        ctx.pace(1);
        ctx.powerOff(PRIMARY);
        ctx.narrate({
          title: '물리 서버 전원이 꺼졌습니다',
          body: '외부에서 보내는 HTTP 요청이 실패하기 시작합니다. 아직 아무도 자동으로 움직이지 않았습니다.',
          focus: 'cluster',
        });
      },
    },
    {
      at: 19,
      run: (ctx) => {
        ctx.pace(8);
        ctx.narrate({
          title: 'Kubernetes가 노드 응답을 기다리는 중',
          body: 'kubelet 보고가 끊긴 뒤 유예 시간이 지나야 API가 NotReady를 보고합니다. 이 구간은 빨리 감기로 보여 드립니다.',
          focus: 'pipeline',
        });
      },
    },
    {
      at: 54,
      chapter: '감지',
      run: (ctx) => {
        ctx.pace(4);
        ctx.setReadiness(PRIMARY, 'Unknown');
        ctx.narrate({
          title: 'API가 NotReady를 보고, 20초 지속을 확인합니다',
          body: '순간적인 끊김을 장애로 오판하지 않도록 정책의 nodeUnreadyFor(20초) 동안 상태가 유지되는지 봅니다.',
          focus: 'pipeline',
        });
      },
    },
    {
      at: 74,
      run: (ctx) => {
        ctx.pace(1);
        ctx.openIncident({ type: 'NodeNotReady', nodeName: PRIMARY, observedAt: ctx.nowMs - 20_000 });
        ctx.condition('Detected', 'True', 'NodeNotReady', `${PRIMARY} Ready=Unknown 20초 지속`);
        ctx.narrate({
          title: 'RecoveryIncident 생성',
          body: '장애 하나에 Incident 하나. 이름을 먼저 예약해 Controller가 재시작돼도 중복 Incident나 중복 조치가 생기지 않습니다.',
          focus: 'incident',
        });
      },
    },
    {
      at: 77,
      run: (ctx) => {
        ctx.classify('INFRASTRUCTURE_FAILURE', 'Node Ready=Unknown 지속, Application 근거 없음 → 인프라 장애');
        ctx.narrate({
          title: '규칙으로 분류: 인프라 장애',
          body: 'AI가 추측하지 않습니다. 버전이 있는 규칙이 근거(노드 상태)를 보고 분류합니다. 인프라 장애는 코드 수정 대상이 아닙니다.',
          focus: 'incident',
        });
      },
    },
  ];
}

/** Shared middle: candidates, filter, score, act, verify, recover. */
function placementAndVerify(start: number, podStartSec: number, extra: (r: EvaluationResult) => string): Cue[] {
  let result: EvaluationResult | null = null;
  const placed = start + 15 + podStartSec;
  return [
    {
      at: start,
      chapter: '후보 평가',
      run: (ctx) => {
        ctx.phase('COLLECTING_CANDIDATES', 'Kubernetes API(상태·requests) + Prometheus(실사용량) snapshot 수집');
        ctx.narrate({
          title: '두 출처의 근거를 모읍니다',
          body: 'Ready·taint·요청량은 Kubernetes API가, 실제 CPU·Memory 사용량은 Prometheus가 권위입니다. 낮은 CPU 사용률과 충분한 배치 여유는 같지 않습니다.',
          focus: 'candidates',
        });
      },
    },
    {
      at: start + 3,
      run: (ctx) => {
        ctx.phase('FILTERING', 'Hard filter 적용');
        result = ctx.evaluate();
        ctx.narrate({
          title: 'Hard filter: 조건을 하나라도 어기면 탈락',
          body: `${describeRejections(result)}. 탈락 이유는 하나도 빠짐없이 기록됩니다.`,
          focus: 'candidates',
        });
      },
    },
    {
      at: start + 8,
      run: (ctx) => {
        ctx.phase('SCORING', '통과 후보 점수 계산');
        ctx.narrate({
          title: '남은 후보에 점수를 매깁니다',
          body: '점수 = 100 × (0.30×CPU 여유 + 0.35×Memory 여유 + 0.35×요청 여유) − 최근 실패 감점. 가중치는 코드 상수가 아니라 RecoveryPolicy 데이터입니다.',
          focus: 'candidates',
        });
      },
    },
    {
      at: start + 12,
      chapter: '배치',
      run: (ctx) => {
        ctx.phase('TARGET_SELECTED', result ? describeSelection(result) : '선택');
        ctx.condition('TargetSelected', 'True', 'Scored', result ? describeSelection(result) : '');
        ctx.narrate({
          title: result?.selected ? `${nodeLabel(result.selected.nodeName)}를 선택했습니다` : '선택 대상 없음',
          body: result ? `${describeSelection(result)} ${extra(result)}` : '',
          focus: 'candidates',
        });
      },
    },
    {
      at: start + 15,
      run: (ctx) => {
        ctx.pace(1.5);
        ctx.execute(podStartSec);
        ctx.narrate({
          title: 'nodeSelector만 바꾸고, 배치는 Scheduler에게',
          body: 'OpsLoop는 Deployment의 nodeSelector와 attempt-id만 조건부로 patch합니다. 새 Pod를 먼저 띄우고(maxSurge=1) 이전 Pod는 따로 추적합니다.',
          focus: 'cluster',
        });
      },
    },
    {
      at: placed + 1,
      chapter: '검증',
      run: (ctx) => {
        ctx.pace(2);
        ctx.phase('VERIFYING', '현재 attempt Pod에 HTTP 검증 시작', 'verify');
        ctx.narrate({
          title: '새 Pod를 직접 검증합니다',
          body: '연속 5회 HTTP 성공과 30초 안정화를 모두 통과해야 복구입니다. 응답이 이전 Pod에서 온 것이면 인정하지 않습니다.',
          focus: 'pipeline',
        });
      },
    },
    {
      at: placed + 31,
      chapter: '복구 완료',
      run: (ctx) => {
        ctx.pace(1);
        ctx.recovered('현재 attempt Pod /health·/ready, HTTP 연속 성공, 30초 안정화 통과');
        ctx.condition('Recovered', 'True', 'HttpVerified', '연속 5회 성공·30초 안정화');
        ctx.narrate({
          title: '복구 완료',
          body: '장애부터 복구까지 걸린 시간은 Incident에 단계별로 남습니다. 아래 시간은 시뮬레이션 값이며 실측 결과가 아닙니다.',
          focus: 'incident',
        });
      },
    },
    {
      at: placed + 43,
      run: (ctx) => {
        ctx.narrate({
          title: '자동으로 되돌리지 않습니다',
          body: '물리 서버가 돌아와도 자동 failback은 하지 않습니다. 연결이 끊긴 이전 Pod는 oldPodsOutstanding으로 계속 추적합니다.',
          focus: 'cluster',
        });
      },
    },
  ];
}

const nodeFailureToLaptop: DemoScenario = {
  id: 'node-failure-laptop',
  title: '노드 장애 → 노트북 VM',
  shortTitle: '노트북 VM으로 복구',
  summary: 'Azure ARM64가 바쁠 때, 실제 사용량을 보고 노트북 VM을 고릅니다.',
  tag: 'demo-a',
  designRef: 'architecture §17 Demo A',
  script: {
    seed: 7,
    durationSec: 156,
    cues: [
      ...nodeFailurePrelude({
        at: 6,
        run: (ctx) => {
          ctx.setLoad(AZURE, { cpu: 0.93, memory: 0.72 });
          ctx.narrate({
            title: 'Azure ARM64 워커에 부하를 걸어 둡니다',
            body: 'Demo Driver가 제한된 CPU 부하를 줍니다. 이 부하가 잠시 뒤 복구 대상 선택을 바꿉니다.',
            focus: 'cluster',
          });
        },
      }),
      ...placementAndVerify(80, 8, () => '같은 AMD64라서가 아니라, 지금 자원 상태가 가장 좋기 때문입니다.'),
    ],
  },
};

const nodeFailureToArm: DemoScenario = {
  id: 'node-failure-arm64',
  title: '노드 장애 → Azure ARM64',
  shortTitle: 'ARM64로 복구',
  summary: '노트북 VM이 바쁠 때, 같은 이미지를 ARM64 노드에서 native로 실행합니다.',
  tag: 'demo-a',
  designRef: 'architecture §17 Demo A',
  script: {
    seed: 11,
    durationSec: 162,
    cues: [
      ...nodeFailurePrelude({
        at: 6,
        run: (ctx) => {
          ctx.setLoad(LAPTOP, { cpu: 0.91, memory: 0.6 });
          ctx.narrate({
            title: '노트북 VM에 제한된 부하를 걸어 둡니다',
            body: 'Control Plane이 죽지 않도록 부하 범위를 제한합니다. 이번에는 노트북 VM이 바쁜 상황입니다.',
            focus: 'cluster',
          });
        },
      }),
      ...placementAndVerify(
        80,
        14,
        () => '이미지 index에 linux/arm64 manifest가 있어 에뮬레이션 없이 ARM64에서 그대로 실행됩니다.',
      ),
    ],
  },
};

/**
 * FILTERING → SCORING → TARGET_SELECTED as separate beats so the candidate board
 * can reveal reasons, then scores, then the choice (Demo B re-evaluations).
 */
function decisionCues(at: number, narration: (r: EvaluationResult) => Narration): Cue[] {
  let result: EvaluationResult | null = null;
  return [
    {
      at,
      run: (ctx) => {
        ctx.phase('FILTERING', 'Hard filter 적용');
        result = ctx.evaluate();
        ctx.narrate({ title: 'Hard filter로 후보를 거릅니다', body: `${describeRejections(result)}.`, focus: 'candidates' });
      },
    },
    { at: at + 1.5, run: (ctx) => ctx.phase('SCORING', '통과 후보 점수 계산') },
    {
      at: at + 2.5,
      run: (ctx) => {
        if (!result) return;
        ctx.phase('TARGET_SELECTED', describeSelection(result));
        ctx.narrate(narration(result));
      },
    },
  ];
}

function crashCue(at: number, title: string, body: string): Cue {
  return {
    at,
    run: (ctx: ScenarioContext) => {
      ctx.crash();
      ctx.narrate({ title, body, focus: 'cluster' });
    },
  };
}

const applicationFailureToPr: DemoScenario = {
  id: 'application-failure-pr',
  title: '반복 앱 장애 → 검증된 PR',
  shortTitle: '검증된 PR까지',
  summary: '운영 조치로 풀리지 않는 코드 결함을 증거와 함께 개발 단계로 넘깁니다.',
  tag: 'demo-b',
  designRef: 'architecture §17 Demo B, §13',
  script: {
    seed: 23,
    durationSec: 160,
    cues: [
      {
        at: 0,
        chapter: '평상시',
        run: (ctx) => {
          ctx.pace(2);
          // Azure carries a moderate background load, so the first action is a restart
          // on the healthy primary and only the second one relocates.
          ctx.setLoad(AZURE, { cpu: 0.4, memory: 0.46 });
          ctx.narrate({
            title: 'Demo API는 물리 서버에서 실행 중',
            body: '이번에는 하드웨어가 아니라 코드 결함입니다. 노드는 모두 건강합니다.',
            focus: 'cluster',
          });
        },
      },
      {
        at: 6,
        chapter: '반복 장애',
        run: (ctx) => {
          ctx.pace(1);
          ctx.narrate({
            title: '고정 입력을 재생합니다',
            body: 'Demo Driver가 필터를 거치면 빈 집합이 되는 배치 집계 입력을 보냅니다. 경계 처리 결함이 숨어 있습니다.',
            focus: 'cluster',
          });
        },
      },
      crashCue(9, '프로세스가 종료됩니다', 'background worker의 집계 invariant 위반으로 프로세스가 exit 2로 끝납니다. kubelet이 다시 띄웁니다.'),
      { at: 12, run: (ctx) => ctx.pace(3) },
      crashCue(24, '같은 방식으로 다시 종료', 'kubelet 재시작만으로는 해결되지 않습니다. 재시작 횟수는 OpsLoop 조치 횟수와 따로 셉니다.'),
      crashCue(44, '120초 안에 3번째 종료', '정책의 restartThreshold(120초 동안 3회)에 도달했습니다.'),
      {
        at: 46,
        run: (ctx) => {
          ctx.pace(1.5);
          // The third exit was observed at t=44; the Incident is reserved two seconds later.
          ctx.openIncident({ type: 'RepeatedRestart', podName: 'demo-api-7c9d4-x2kq8', restarts: 3, observedAt: ctx.nowMs - 2000 });
          ctx.condition('Detected', 'True', 'RepeatedRestart', '120초 동안 3회 종료');
          ctx.narrate({
            title: 'RecoveryIncident 생성',
            body: '반복 종료를 근거로 Incident를 예약합니다.',
            focus: 'incident',
          });
        },
      },
      {
        at: 49,
        run: (ctx) => {
          ctx.classify(
            'APPLICATION_FAILURE',
            'Node Ready, OOM·이미지·설정 근거 없음, 같은 exit code·정규화 stack signature 3회',
            'sig:agg-empty-window/9f3a1c',
          );
          ctx.narrate({
            title: '규칙으로 분류: Application 장애 후보',
            body: '운영 원인(노드·메모리·이미지·설정)을 먼저 배제한 뒤에야 Application으로 봅니다. CrashLoopBackOff 하나만으로 코드 문제라고 단정하지 않습니다.',
            focus: 'incident',
          });
        },
      },
      {
        at: 52,
        chapter: 'Runtime 조치 1',
        run: (ctx) => {
          ctx.phase('COLLECTING_CANDIDATES', 'snapshot 수집');
        },
      },
      ...decisionCues(54, (r) => ({
        title: '먼저 운영 조치로 해결을 시도합니다',
        body: `${describeSelection(r)} 대부분의 장애는 여기서 끝납니다.`,
        focus: 'candidates',
      })),
      { at: 58, run: (ctx) => ctx.execute(6) },
      {
        at: 64,
        run: (ctx) => {
          ctx.phase('VERIFYING', '같은 fixture 재생으로 잠복 실패 확인', 'verify');
          ctx.narrate({
            title: '같은 입력으로 다시 확인합니다',
            body: '정상 요청만 보면 살아난 것처럼 보일 수 있습니다. 실패를 만든 고정 입력을 다시 재생해 진짜 복구인지 확인합니다.',
            focus: 'pipeline',
          });
        },
      },
      crashCue(68, '같은 signature로 다시 종료', '재시작으로는 해결되지 않았습니다.'),
      {
        at: 70,
        run: (ctx) => {
          ctx.failAttempt('동일 signature 재현');
          ctx.phase('COOLDOWN', '실패한 workload–node 조합 120초 cooldown', 'verify');
        },
      },
      {
        at: 72,
        chapter: 'Runtime 조치 2',
        run: (ctx) => ctx.phase('COLLECTING_CANDIDATES', 'snapshot 재수집'),
      },
      ...decisionCues(74, (r) => ({
        title: '다른 노드로 옮겨 봅니다',
        body: `${describeRejections(r)}. ${describeSelection(r)}`,
        focus: 'candidates',
      })),
      { at: 78, run: (ctx) => ctx.execute(12) },
      {
        at: 90,
        run: (ctx) => ctx.phase('VERIFYING', '같은 fixture 재생', 'verify'),
      },
      crashCue(93, '다른 노드에서도 같은 signature', '노드를 바꿔도 같은 방식으로 종료됩니다. 운영 문제가 아니라 코드 문제일 가능성이 큽니다.'),
      {
        at: 95,
        chapter: '증거·격리',
        run: (ctx) => {
          ctx.failAttempt('동일 signature 재현');
          ctx.phase('RUNTIME_RECOVERY_EXHAUSTED', 'Runtime 조치 예산 2/2 소진, 동일 signature', 'fail');
          ctx.narrate({
            title: 'Runtime 예산 소진',
            body: '같은 Application 장애가 조치 예산(2회)을 모두 쓰고도 재현될 때만 개발 단계로 넘깁니다. 후보 부족이나 관측 실패는 이 예산에 포함하지 않습니다.',
            focus: 'pipeline',
          });
        },
      },
      {
        at: 99,
        run: (ctx) => {
          ctx.evidence(
            `configmap/${ctx.incidentName}-bundle`,
            'Incident Bundle 확정: 최초 장애·변경 전 증거, exit code, 정규화 stack, fixture ID, image digest (로그 일부 truncation 표시)',
          );
          ctx.narrate({
            title: '증거를 먼저 보존합니다',
            body: '격리하기 전에 최초 장애와 변경 전 상태를 Bundle로 남깁니다. 빠지거나 잘린 증거도 숨기지 않고 표시합니다.',
            focus: 'incident',
          });
        },
      },
      {
        at: 103,
        run: (ctx) => {
          ctx.quarantine('QuarantineRequested', 'opt-in 격리: 원래 replicas 기록 후 replicas=0 요청');
          ctx.narrate({
            title: '반복 장애를 격리합니다',
            body: '정책이 허용한 경우에만 replicas=0을 요청해 재시작 폭주를 멈춥니다. 서비스는 이 시점에 멈춰 있습니다.',
            focus: 'cluster',
          });
        },
      },
      { at: 106, run: (ctx) => ctx.quarantine('QuarantineConfirmed', 'replicas=0 반영 확인') },
      {
        at: 108,
        chapter: 'Engineering',
        run: (ctx) => {
          ctx.phase('ENGINEERING_HANDOFF', 'Bundle 준비 및 격리 완료 → Engineering 대기열');
          ctx.engineering({ phase: 'ENGINEERING_QUEUED', branch: `opsloop/${ctx.incidentName}` }, 'Runner 대기열 등록');
          ctx.narrate({
            title: '독립된 Engineering Runner로 넘깁니다',
            body: 'Runner는 클러스터 밖에서 따로 실행됩니다. 운영 자격 증명도, PR 게시 권한도 Codex에게 주지 않습니다.',
            focus: 'engineering',
          });
        },
      },
      { at: 110, run: (ctx) => ctx.engineering({ phase: 'VALIDATING_INPUT' }, 'Bundle schema·digest·repository allowlist·provenance 검증') },
      {
        at: 113,
        run: (ctx) => {
          ctx.engineering({ phase: 'ANALYZING', iteration: 1 }, '격리된 worktree에서 원인 분석 (iteration 1/3)');
          ctx.narrate({
            title: 'Codex가 증거로 원인을 분석합니다',
            body: '격리된 worktree에서 Bundle만 보고 분석·수정합니다. 수정 가능한 경로와 횟수가 제한됩니다.',
            focus: 'engineering',
          });
        },
      },
      { at: 117, run: (ctx) => ctx.engineering({ phase: 'PATCHING' }, 'patch 작성') },
      {
        at: 119,
        run: (ctx) => {
          ctx.engineering({ phase: 'BUILDING' }, 'build');
          ctx.stage({ stage: 'build', iteration: 1, outcome: 'pass', summary: 'go build 통과' });
        },
      },
      {
        at: 121,
        run: (ctx) => {
          ctx.engineering({ phase: 'TESTING' }, 'unit · regression fixture');
          ctx.stage({ stage: 'unit', iteration: 1, outcome: 'pass', summary: '정상 입력 단위 테스트 통과' });
          ctx.stage({ stage: 'regression-fixture', iteration: 1, outcome: 'fail', summary: '빈 집합 fixture에서 panic만 숨김' });
        },
      },
      {
        at: 124,
        run: (ctx) => {
          ctx.engineering({ phase: 'FEEDBACK' }, 'regression fixture 실패 → Feedback');
          ctx.narrate({
            title: '실패하면 결과를 되돌려 줍니다',
            body: '첫 patch는 panic을 숨기기만 해서 신뢰된 regression fixture를 통과하지 못했습니다. 실패 결과를 다시 분석 단계로 보냅니다.',
            focus: 'engineering',
          });
        },
      },
      { at: 127, run: (ctx) => ctx.engineering({ phase: 'ANALYZING', iteration: 2 }, 'Feedback 반영 재분석 (iteration 2/3)') },
      { at: 130, run: (ctx) => ctx.engineering({ phase: 'PATCHING' }, '빈 집합 경계에 typed error 처리') },
      {
        at: 132,
        run: (ctx) => {
          ctx.engineering({ phase: 'BUILDING' }, 'build');
          ctx.stage({ stage: 'build', iteration: 2, outcome: 'pass', summary: 'go build 통과' });
        },
      },
      {
        at: 134,
        run: (ctx) => {
          ctx.engineering({ phase: 'TESTING' }, 'unit · integration · regression fixture');
          ctx.stage({ stage: 'unit', iteration: 2, outcome: 'pass', summary: '단위 테스트 통과' });
          ctx.stage({ stage: 'integration', iteration: 2, outcome: 'pass', summary: '통합 테스트 통과' });
          ctx.stage({ stage: 'regression-fixture', iteration: 2, outcome: 'pass', summary: 'baseline 실패 → patched 통과' });
        },
      },
      {
        at: 137,
        run: (ctx) => {
          ctx.engineering({ phase: 'VERIFYING' }, 'Multi-Arch smoke');
          ctx.stage({ stage: 'multi-arch-smoke', iteration: 2, outcome: 'pass', summary: 'amd64·arm64 native 실행 통과' });
          ctx.narrate({
            title: '두 아키텍처에서 모두 확인합니다',
            body: '같은 fixture로 baseline 실패 → patched 통과를 확인하고, AMD64·ARM64 양쪽에서 smoke test를 돌립니다.',
            focus: 'engineering',
          });
        },
      },
      {
        at: 141,
        run: (ctx) => {
          ctx.engineering({ phase: 'INDEPENDENT_REVIEW' }, '독립 review');
          ctx.stage({ stage: 'independent-review', iteration: 2, outcome: 'pass', summary: '수정 범위·테스트 변경 없음 확인' });
        },
      },
      { at: 144, run: (ctx) => ctx.engineering({ phase: 'PR_READY' }, '검증한 SHA 재확인') },
      {
        at: 146,
        chapter: 'PR',
        run: (ctx) => {
          ctx.engineering(
            {
              phase: 'PR_CREATED',
              pullRequest: {
                number: 0,
                title: 'fix(demo-api): handle empty aggregation window as typed error',
                url: null,
                headSha: '3e91c07',
              },
            },
            'PR 생성 (Demo: 실제 GitHub PR은 만들지 않음)',
          );
          ctx.narrate({
            title: 'PR까지. 그 다음은 사람이 결정합니다',
            body: 'PR 생성은 서비스 복구가 아닙니다. 자동 Merge도 자동 재배포도 없습니다. Demo API는 사람이 검토해 배포하기 전까지 격리 상태로 남습니다.',
            focus: 'engineering',
          });
        },
      },
    ],
  },
};

const noCandidateWait: DemoScenario = {
  id: 'no-candidate',
  title: '후보 없음 → 기다림',
  shortTitle: '후보 없음',
  summary: '갈 곳이 없으면 억지로 옮기지 않고, 코드 문제로 넘기지도 않습니다.',
  tag: 'negative',
  designRef: 'architecture §7 WAITING, §11 freshness',
  script: {
    seed: 31,
    durationSec: 700,
    cues: [
      ...nodeFailurePrelude({
        at: 6,
        chapter: '나쁜 조건',
        run: (ctx) => {
          ctx.setLoad(LAPTOP, { cpu: 0.9 });
          ctx.setExporterUp(AZURE, false);
          ctx.narrate({
            title: '노트북 VM은 바쁘고, Azure 관측이 끊겼습니다',
            body: 'Azure의 node exporter 수집이 멈췄습니다. OpsLoop는 값이 없으면 0%로 보지 않습니다.',
            focus: 'cluster',
          });
        },
      }),
      {
        at: 80,
        chapter: '후보 평가',
        run: (ctx) => ctx.phase('COLLECTING_CANDIDATES', 'snapshot 수집'),
      },
      {
        at: 83,
        run: (ctx) => {
          ctx.phase('FILTERING', 'Hard filter 적용');
          const r = ctx.evaluate();
          ctx.narrate({
            title: '모든 후보가 탈락했습니다',
            body: `${describeRejections(r)}.`,
            focus: 'candidates',
          });
        },
      },
      {
        at: 88,
        chapter: '대기',
        run: (ctx) => {
          ctx.wait('NO_CANDIDATE', '적격 후보 없음 → deadline 안에서 재평가');
          ctx.pace(20);
          ctx.narrate({
            title: '점수와 상관없이 기다립니다',
            body: '갈 곳이 없으면 옮기지 않습니다. 정책의 incidentDeadline(10분) 안에서 주기적으로 다시 평가합니다.',
            focus: 'pipeline',
          });
        },
      },
      ...[150, 250, 350, 450, 550].flatMap((t): Cue[] => [
        { at: t, run: (ctx) => ctx.phase('COLLECTING_CANDIDATES', '재평가') },
        {
          at: t + 2,
          run: (ctx) => {
            ctx.phase('FILTERING', 'Hard filter 재적용');
            ctx.evaluate();
          },
        },
        { at: t + 4, run: (ctx) => ctx.wait('NO_CANDIDATE', '여전히 적격 후보 없음') },
      ]),
      {
        at: 674,
        chapter: '운영 실패',
        run: (ctx) => {
          ctx.pace(1);
          ctx.fail('incidentDeadline 10분 초과 → 운영 실패, 사람의 판단 필요');
          ctx.condition('EngineeringEligible', 'False', 'NoCandidate', '후보 부재는 코드 결함의 근거가 아님');
          ctx.narrate({
            title: '운영 실패로 끝내고, 코드 수정으로 넘기지 않습니다',
            body: '후보가 없는 것은 코드 결함이 아닙니다. Engineering으로 넘기지 않고 운영자에게 알립니다. 할 수 없는 일은 할 수 없다고 기록합니다.',
            focus: 'incident',
          });
        },
      },
    ],
  },
};

export const DEMO_SCENARIOS: DemoScenario[] = [
  nodeFailureToLaptop,
  applicationFailureToPr,
  nodeFailureToArm,
  noCandidateWait,
];
