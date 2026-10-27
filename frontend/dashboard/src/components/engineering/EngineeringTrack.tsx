import { useMemo } from 'react';
import { ENGINEERING_PHASE_LABEL, STAGE_LABEL } from '../../domain/labels';
import type { IncidentCondition, IncidentEngineeringStatus, StageResult } from '../../domain/types';
import { EnumCode, Instrument, Lamp, type LampState } from '../shared/primitives';
import type { RegionProps } from '../types';
import {
  PR_PHASES,
  buildTrack,
  engineeringBlock,
  groupByIteration,
  type FeedbackState,
  type IterationGroup,
  type TrackModel,
  type TrackState,
} from './engineeringModel';
import { engineeringIncident, runtimeAlongside, type RuntimeAlongside } from './runtimeState';
import styles from './EngineeringTrack.module.css';

const TRACK_LAMP: Record<TrackState, { tone: LampState; word: string }> = {
  done: { tone: 'ok', word: '완료' },
  current: { tone: 'act', word: '진행 중' },
  pending: { tone: 'off', word: '아직 아님' },
  failed: { tone: 'fault', word: '실패' },
};

const STAGE_LAMP: Record<StageResult['outcome'], { tone: LampState; word: string }> = {
  pass: { tone: 'ok', word: '통과' },
  fail: { tone: 'fault', word: '실패' },
  running: { tone: 'act', word: '실행 중' },
};

const OUTCOME_WORD: Record<IterationGroup['outcome'], string> = {
  pass: '통과',
  fail: '실패',
  running: '진행 중',
};

export function EngineeringTrack({ snapshot, emphasized }: RegionProps) {
  const incident = engineeringIncident(snapshot);
  const eng = incident?.status.engineering;
  const block = incident ? engineeringBlock(incident.status.conditions) : null;
  const runtime = runtimeAlongside(snapshot, incident);
  const track = useMemo(() => buildTrack(eng), [eng]);

  const aside = eng ? <RunReadout eng={eng} track={track} /> : null;

  return (
    <Instrument title="Engineering Runner" emphasized={emphasized ?? false} aside={aside}>
      <div className={styles.root} tabIndex={0} role="region" aria-label="Engineering Runner 진행 상황">
        <Boundary runtime={runtime} workloadName={snapshot.workload.name} />
        {eng?.evidence ? <p className={styles.explain}>{incident === snapshot.activeIncident ? '현재 Incident' : '종료된 Incident 이력'}: {incident?.namespace}/{incident?.name}. Runtime 조치 실패. 현재 정책 {snapshot.policy.spec.enabled ? '활성' : '비활성'}. 실제 Codex {eng.evidence.model}의 코드 응답을 coordinator가 적용하고 독립 검증했습니다. 로컬 검증 통과 · ARM64 교차 빌드만 확인 · 원격 CI 미실행. GitHub 조회 시각: {eng.evidence.observedAt}. 이 표시는 증거의 요약이며 운영 복구를 뜻하지 않습니다.</p> : null}
        {eng?.evidence ? <details className={styles.explain}><summary>Incident · Runtime 실행 · 증거 연결</summary><dl className={styles.meta}>{Object.entries({ 'Incident UID': incident?.uid, 'Runtime run ID': eng.evidence.runtimeRunID, epoch: eng.evidence.epoch, 'Policy UID': eng.evidence.policyUID, 'Grant SHA-256': eng.evidence.grantSha256, 'Bundle SHA-256': eng.evidence.bundleSha256, '검증 result SHA-256': eng.evidence.resultSha256, 'PR receipt SHA-256': eng.evidence.prReceiptSha256 }).map(([name, value]) => <div key={name}><dt>{name}</dt><dd><code>{value ?? '미확인'}</code></dd></div>)}</dl></details> : null}
        {eng ? (
          <Run eng={eng} track={track} />
        ) : snapshot.source !== 'demo' ? (
          <p className={styles.explain}>이 관측 기록에 연결된 검증 가능한 Engineering 실행 결과가 없습니다.</p>
        ) : (
          <NotStarted track={track} block={block} />
        )}
      </div>
    </Instrument>
  );
}

/** Current Runner phase with its raw enum, for the instrument header. */
function RunReadout({ eng, track }: { eng: IncidentEngineeringStatus; track: TrackModel }) {
  const done = eng.phase === 'PR_CREATED';
  const tone: LampState = done ? 'ok' : track.terminal ? track.terminal.tone : 'act';
  const working = !done && track.terminal === null;
  return (
    <span className={styles.asidePhase}>
      <Lamp state={tone} pulse={working} label={done ? '완료' : working ? '진행 중' : '멈춤'} />
      {ENGINEERING_PHASE_LABEL[eng.phase]} <EnumCode>{eng.phase}</EnumCode>
    </span>
  );
}

// ---------------------------------------------------------------------------

/** Always visible: what a pull request does and does not mean. */
function Boundary({ runtime, workloadName }: { runtime: RuntimeAlongside; workloadName: string }) {
  return (
    <ul className={styles.plate} aria-label="Engineering의 경계">
      <li className={styles.plateRuntime}>
        <Lamp state={runtime.tone} label={RUNTIME_WORD[runtime.tone]} />
        <span>
          <strong>{workloadName}</strong> {runtime.state} <EnumCode>{runtime.fact}</EnumCode>
        </span>
      </li>
      <li>PR 생성은 서비스 복구가 아닙니다</li>
      <li>자동 Merge·재배포 없음</li>
      <li>Codex에 운영 자격 증명 없음</li>
    </ul>
  );
}

const RUNTIME_WORD: Record<LampState, string> = {
  ok: '정상',
  caution: '주의',
  fault: '서비스 중단',
  act: '복구 진행 중',
  off: '해당 없음',
};

// ---------------------------------------------------------------------------

function Track({ track, counter }: { track: TrackModel; counter: { iteration: number; max: number } | null }) {
  return (
    <div className={styles.trackWrap}>
      <FeedbackArch state={track.feedback} counter={counter} />
      <ol className={styles.track} aria-label="Engineering 단계 순서">
        {track.steps.map((step, i) => {
          const lamp = TRACK_LAMP[step.state];
          const next = track.steps[i + 1];
          return (
            <li
              key={step.phase}
              className={styles.station}
              data-state={step.state}
              data-next-reached={(next !== undefined && next.state !== 'pending') || undefined}
              aria-current={step.state === 'current' ? 'step' : undefined}
            >
              <span className={styles.mark} data-glyph={step.state}>
                <Lamp state={lamp.tone} pulse={step.state === 'current'} label={lamp.word} />
              </span>
              <span className={styles.label}>{ENGINEERING_PHASE_LABEL[step.phase]}</span>
              <span className={styles.stepCode}>
                <EnumCode>{step.phase}</EnumCode>
              </span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

/** The loop back from a failed Build/Test/Verify/Review to analysis, with its counter. */
function FeedbackArch({
  state,
  counter,
}: {
  state: FeedbackState;
  counter: { iteration: number; max: number } | null;
}) {
  const tone: LampState = state === 'idle' ? 'off' : 'act';
  const word = state === 'current' ? '되돌리는 중' : state === 'taken' ? '되돌린 적 있음' : '아직 되돌리지 않음';
  return (
    <div className={styles.arch} data-state={state} role="group" aria-label="Feedback 반복">
      <span className={styles.archLabel}>
        <Lamp state={tone} pulse={state === 'current'} label={word} />
        <span>{counter ? 'Feedback' : '실패하면 Feedback으로 되돌려 다시 분석'}</span>
        {counter ? (
          <span className={styles.counter}>
            iteration <strong>{counter.iteration}</strong>/{counter.max}
          </span>
        ) : null}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------

function Run({ eng, track }: { eng: IncidentEngineeringStatus; track: TrackModel }) {
  const groups = useMemo(() => groupByIteration(eng.stages), [eng.stages]);
  const showPr = PR_PHASES.includes(eng.phase);

  return (
    <>
      <Track track={track} counter={{ iteration: eng.iteration, max: eng.maxIterations }} />
      {track.terminal ? (
        <p className={styles.terminal} data-tone={track.terminal.tone}>
          <Lamp state={track.terminal.tone} label={track.terminal.tone === 'fault' ? '실패' : '주의'} />
          <strong>{ENGINEERING_PHASE_LABEL[track.terminal.phase]}</strong>
          <EnumCode>{track.terminal.phase}</EnumCode>
          <span>Runner가 여기서 멈췄습니다. PR은 만들지 않았습니다.</span>
        </p>
      ) : null}
      <dl className={styles.meta}>
        <div>
          <dt>작업</dt>
          <dd>
            <EnumCode>{eng.jobId}</EnumCode>
          </dd>
        </div>
        <div>
          <dt>기준 commit</dt>
          <dd>
            <EnumCode>{eng.baseCommit}</EnumCode>
          </dd>
        </div>
        {eng.branch ? (
          <div>
            <dt>브랜치</dt>
            <dd>
              <EnumCode>{eng.branch}</EnumCode>
            </dd>
          </div>
        ) : null}
      </dl>
      {showPr ? <PullRequest pr={eng.pullRequest} /> : null}
      <Iterations groups={groups} eng={eng} compact={showPr} />
    </>
  );
}

function PullRequest({ pr }: { pr: IncidentEngineeringStatus['pullRequest'] }) {
  return (
    <section className={styles.pr} aria-label="Pull Request">
      <p className={styles.prHead}>
        <strong>Pull Request</strong>
        <span className={styles.prNumber}>{pr ? (pr.number > 0 ? `#${pr.number}` : '번호 없음') : '준비 중'}</span>
        {pr ? (
          <span className={styles.prNote}>
            {pr.url === null ? 'Demo: 실제 GitHub PR은 만들지 않았습니다' : <a href={pr.url} target="_blank" rel="noopener noreferrer">{pr.url}</a>}
          </span>
        ) : null}
      </p>
      {pr ? (
        <p className={styles.prTitleRow}>
          <span className={styles.prTitle}>{pr.title}</span>
          <span className={styles.prSha}>
            head <EnumCode>{pr.headSha}</EnumCode>
          </span>
        </p>
      ) : (
        <p className={styles.prTitle}>검증한 SHA를 다시 확인한 뒤 게시합니다.</p>
      )}
    </section>
  );
}

function Iterations({
  groups,
  eng,
  compact,
}: {
  groups: IterationGroup[];
  eng: IncidentEngineeringStatus;
  compact: boolean;
}) {
  if (groups.length === 0) {
    return (
      <section className={styles.iterations} aria-label="반복별 검증 결과">
        <p className={styles.empty}>
          아직 검증 결과가 없습니다. 수정을 작성한 뒤 Build, Test, 재현 검증, 독립 리뷰 순서로 실행합니다.
        </p>
      </section>
    );
  }
  if (compact) {
    // PR stage: the pull request and the track come first, so the history shrinks to one line.
    return (
      <section className={styles.iterations} aria-label="반복별 검증 결과">
        <p className={styles.digest}>
          {groups.map((g) => (
            <span key={g.iteration} className={styles.digestItem}>
              <span className={styles.iterLabel}>
                iteration <strong>{g.iteration}</strong>/{eng.maxIterations}
              </span>
              <span className={styles.outcome} data-outcome={g.outcome}>
                {OUTCOME_WORD[g.outcome]}
              </span>
              {g.failure ? <span className={styles.carry}>({STAGE_LABEL[g.failure.stage]}, 다음 분석으로 전달)</span> : null}
            </span>
          ))}
        </p>
      </section>
    );
  }

  const latest = groups[groups.length - 1]!;
  const past = groups.slice(0, -1);
  const feeding = eng.phase === 'FEEDBACK' && latest.outcome === 'fail';

  return (
    <section className={styles.iterations} aria-label="반복별 검증 결과">
      {past.map((g) => (
        <p key={g.iteration} className={styles.past}>
          <span className={styles.iterLabel}>
            iteration <strong>{g.iteration}</strong>/{eng.maxIterations}
          </span>
          <span className={styles.outcome} data-outcome={g.outcome}>
            {OUTCOME_WORD[g.outcome]}
          </span>
          {g.failure ? (
            <span>
              {STAGE_LABEL[g.failure.stage]}: {g.failure.summary}{' '}
              <span className={styles.carry}>(iteration {g.iteration + 1} 분석에 전달)</span>
            </span>
          ) : null}
        </p>
      ))}

      <div className={styles.latest}>
        <p className={styles.latestHead}>
          <span className={styles.iterLabel}>
            iteration <strong>{latest.iteration}</strong>/{eng.maxIterations}
          </span>
          <span className={styles.outcome} data-outcome={latest.outcome}>
            {OUTCOME_WORD[latest.outcome]}
          </span>
          {feeding ? <span className={styles.carry}>실패 결과를 다음 분석에 전달하는 중</span> : null}
        </p>
        <ul className={styles.stages}>
          {latest.stages.map((s) => {
            const lamp = STAGE_LAMP[s.outcome];
            return (
              <li key={s.stage} className={styles.stage}>
                <span
                  className={styles.mark}
                  data-glyph={s.outcome === 'pass' ? 'done' : s.outcome === 'fail' ? 'failed' : 'current'}
                >
                  <Lamp state={lamp.tone} pulse={s.outcome === 'running'} label={lamp.word} />
                </span>
                <span className={styles.stageText}>
                  <strong>{STAGE_LABEL[s.stage]}</strong> <span className={styles.stageSummary}>{s.summary}</span>
                </span>
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------

function NotStarted({ track, block }: { track: TrackModel; block: IncidentCondition | null }) {
  return (
    <>
      {block ? (
        <div className={styles.block} role="note">
          <Lamp state="caution" label="주의" />
          <div>
            <p className={styles.blockTitle}>
              Engineering으로 넘기지 않았습니다 <EnumCode>EngineeringEligible=False</EnumCode>{' '}
              <EnumCode>{block.reason}</EnumCode>
            </p>
            <p className={styles.blockBody}>{block.message}</p>
          </div>
        </div>
      ) : (
        <p className={styles.explain}>
          Engineering은 같은 Application 장애가 Runtime 조치 예산을 모두 쓰고도 재현될 때만 시작합니다. 자원·네트워크·이미지·관측
          문제는 코드 수정으로 풀지 않고 운영 영역에 남깁니다.
        </p>
      )}
      <Track track={track} counter={null} />
    </>
  );
}
