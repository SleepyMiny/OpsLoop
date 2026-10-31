import { useEffect, useRef, useState } from 'react';
import { canPrepareNext, canReconcile, comparisonFacts, engineeringSummary, eventFocus, getJSON, isTerminal, lifecycleComplete, mergeEvents, parseArtifacts, parseState, sourceLabel, type Capabilities, type ExhibitionEvent, type ExhibitionRun, type ExhibitionState, type PublicArtifact } from './exhibition';
import styles from './console.module.css';

const names: Record<string, string> = {
  SOURCE_PENDING: '최초 source 기동 대기 · 복구 조치 전', SOURCE_BOUND: '이번 source 신원 결속 · 복구 성공 아님', SOURCE_REJECTED: 'source 검증 거부 · 실행 중단',
  EXHIBITION_REPLAY_APPROVED: '승인된 전시 재실행 · 이번 회차 circuit 예외 · 운영 회복 선언 아님',
  RUNTIME_CAPTURE_STARTED: '이번 실행 Runtime 실험 시작', RUNTIME_STAGE: 'Runtime 실행 · 증거 수집', EVIDENCE_SEALED: '이번 실행 증거 검증 · 봉인 완료',
  BASELINE_VERIFYING: '정상 baseline / 결함 재현 확인', ANALYZING: '모델 요청 준비', MODEL_REQUESTED: '실제 모델 요청 전송', PROPOSAL_RECEIVED: '모델 수정안 수신', PATCH_APPLIED: '수정안 적용',
  EVIDENCE_COLLECTED: '결속된 Runtime 증거 확인', RUNTIME_LIMIT_CONFIRMED: 'Runtime 복구 한계 확인', VERIFICATION_RESULT: '독립 검증 결과', REVISION_REQUESTED: '허용된 재수정 준비', CLEANUP_PENDING: '중지 후 장애 해소 확인 중', SERVICE_RECOVERED: '서비스 회복 확인',
  PATCHING: '수정안 적용', VERIFYING: '독립 검증', SERVICE_PROBE: '실제 서비스 요청', CANDIDATES: '후보 평가',
  CONTROLLER_ACTION: 'Controller 조치', ENDPOINT: 'Endpoint 전환', STOP_CONFIRMED: '종료 확인',
  COMPLETED: '회차 처리 완료 · 앱 배포와 별도', NEXT_RUN_ALLOWED: '종료 절차 확인 · 다음 실행에는 새 승인 필요',
  PR_CREATED: '수정 PR 생성 · 배포 아님', VERIFIED_PENDING_REVIEW: '후보 검증 완료 · 검토 대기 · 배포 아님',
  LIFECYCLE_STARTED: '실행 종료 · 복원 · 보존 · 정리 확인', LIFECYCLE_STOP_CONFIRMED: 'Policy와 실행 프로세스 정지 확인',
  LIFECYCLE_BASELINE_RESTORED: '정상 baseline 복원 확인', LIFECYCLE_EVIDENCE_ARCHIVED: '실행 증거 보존 · 무결성 확인',
  LIFECYCLE_OBJECTS_CLEANED: '해당 실행의 임시 객체 정리 확인', LIFECYCLE_COMPLETE: '다음 실행을 위한 종료 절차 완료',
  RECONCILIATION_REQUIRED: '종료 · 정리 확인 필요', RESTART_RECONCILIATION_REQUIRED: '재시작 후 종료 · 정리 확인 필요',
};
export function eventLabel(kind: string) { return names[kind] ?? kind; }
export function ExhibitionConsole({ capabilities, onFocus, onHistory, onTrack }: {
  capabilities: Capabilities; onFocus: (focus: ReturnType<typeof eventFocus>) => void; onHistory: (history: boolean) => void; onTrack: (scope: string) => void;
}) {
  const [state, setState] = useState<ExhibitionState | null>(null);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState('');
  const [eventError, setEventError] = useState('');
  const [planId, setPlanId] = useState('');
  const [selectedRun, setSelectedRun] = useState('');
  const [viewHistory, setViewHistory] = useState(false);
  const [events, setEvents] = useState<ExhibitionEvent[]>([]);
  const [selectedSeq, setSelectedSeq] = useState<number | null>(null);
  const [pending, setPending] = useState(false);
  const busy = useRef(false);
  const runRef = useRef('');
  const eventsRef = useRef<ExhibitionEvent[]>([]);
  const [refresh, setRefresh] = useState(0);
  const run = state?.runs.find(r => r.runId === selectedRun) ?? (state?.active?.runId === selectedRun ? state.active : undefined);
  const historical = !!run && isTerminal(run);
  const plan = state?.plans.find(p => p.planId === planId);
  const selected = selectedSeq === null ? events.at(-1) : events.find(e => e.seq === selectedSeq);
  const nextAllowed = canPrepareNext(state);
  const blockedRun = state?.active ?? state?.runs.find(r => !lifecycleComplete(r));
  const executing = !!blockedRun && ['RUNNING', 'STOPPING'].includes(blockedRun.state);
  const currentEvent = !historical && selectedRun === state?.active?.runId ? selected : undefined;

  useEffect(() => {
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const next = parseState(await getJSON('/api/exhibition/state', abort.signal));
        if (abort.signal.aborted) return;
        setState(next); setConnected(true);
        setSelectedRun(current => !viewHistory && next.active ? next.active.runId : current || next.runs[0]?.runId || '');
        setPlanId(current => next.plans.some(p => p.planId === current) ? current : next.plans.find(p => p.ready)?.planId || next.plans[0]?.planId || '');
      } catch { if (!abort.signal.aborted) setConnected(false); }
      if (!abort.signal.aborted) timer = setTimeout(poll, 1500);
    }
    void poll(); return () => { abort.abort(); clearTimeout(timer); };
  }, [refresh, viewHistory]);

  useEffect(() => {
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout>;
    runRef.current = selectedRun; eventsRef.current = []; setEvents([]); setSelectedSeq(null);
    if (!selectedRun) return () => abort.abort();
    async function poll() {
      try {
        const body = await getJSON(`/api/exhibition/events?runId=${encodeURIComponent(selectedRun)}&after=${eventsRef.current.at(-1)?.seq ?? 0}`, abort.signal);
        if (abort.signal.aborted || runRef.current !== selectedRun) return;
        const merged = mergeEvents(eventsRef.current, body, selectedRun);
        eventsRef.current = merged; setEvents(merged); setEventError('');
      } catch { if (!abort.signal.aborted) setEventError('이벤트 연결 또는 run/순서 검증 실패 · 마지막 확인 기록을 유지합니다.'); }
      if (!abort.signal.aborted) timer = setTimeout(poll, 1000);
    }
    void poll(); return () => { abort.abort(); clearTimeout(timer); };
  }, [selectedRun]);
  useEffect(() => { onFocus(historical ? undefined : eventFocus(selected, run?.track)); }, [selected, run?.track, historical, onFocus]);
  useEffect(() => { onHistory(historical && viewHistory); }, [historical, viewHistory, onHistory]);
  useEffect(() => { if (state?.active) onTrack(state.active.track === 'engineering' ? 'b' : 'a'); }, [state?.active?.track, onTrack]);

  async function action(kind: 'start' | 'stop' | 'prepare' | 'reconcile') {
    if (busy.current || !capabilities.control || !capabilities.csrfToken) return;
    busy.current = true; setPending(true); setError('');
    try {
      let body: Record<string, string>;
      if (kind === 'reconcile') {
        if (!canReconcile(blockedRun) || !blockedRun) return;
        body = {runId: blockedRun.runId};
      } else if (kind === 'stop') {
        if (!state?.active || state.active.state !== 'RUNNING' || state.active.stopConfirmed) return;
        body = {runId: state.active.runId};
      } else {
        if (!plan || !nextAllowed || !connected || kind === 'start' && !plan.ready) return;
        body = {planId: plan.planId};
        if (kind === 'start') {
          const key = `opsloop-exhibition-request:${plan.planId}`;
          const requestId = sessionStorage.getItem(key) ?? crypto.randomUUID();
          sessionStorage.setItem(key, requestId); // persisted before sending; uncertain response reuses the same key
          body.requestId = requestId;
        }
      }
      const response = await fetch(`/api/exhibition/${kind}`, {method: 'POST', credentials: 'same-origin', redirect: 'error', headers: {'Content-Type': 'application/json', 'X-OpsLoop-CSRF': capabilities.csrfToken}, body: JSON.stringify(body)});
      if (!response.ok) throw Error(`HTTP_${response.status}`);
      const next = parseState(await getJSON('/api/exhibition/state'));
      setState(next); setConnected(true);
      if (next.active) { setSelectedRun(next.active.runId); setViewHistory(false); }
      setRefresh(value => value + 1);
    } catch { setError('요청 결과를 확인하지 못했습니다. 자동 재실행하지 않습니다. 현재 run과 중지 상태를 확인하세요.'); setRefresh(value => value + 1); }
    finally { busy.current = false; setPending(false); }
  }

  return <section className={styles.console} aria-label="실험 실행 관제">
    <div className={styles.heading}><div><h1>Live 관제 · 승인된 실행</h1></div><div className={styles.current}><strong>{state?.active?.state ?? '대기'}</strong>{currentEvent ? <span>{currentEvent.actor} · {eventLabel(currentEvent.kind)}</span> : <span>{historical ? '저장된 실행 기록 · 현재 클러스터 상태와 별도' : '현재 실행 이벤트 대기'}</span>}</div><span data-connected={connected}>{!capabilities.control ? connected ? '호스트 실행 원장 · 관측 전용' : '실행 원장 연결 확인 중' : connected ? '실행 원장 연결됨' : '실행 원장 연결 확인 중 / 끊김'}</span></div>
    <div className={styles.controls}>
      <label>서버에 등록된 실행 계획<select value={planId} onChange={e => setPlanId(e.target.value)} disabled={pending || !nextAllowed}>{!state?.plans.length ? <option value="">새 승인 계획 없음</option> : state.plans.map(p => <option key={p.planId} value={p.planId}>{p.track === 'runtime' ? 'Demo A' : 'Demo B'} · {p.planId}</option>)}</select></label>
      <button onClick={() => void action('prepare')} disabled={!capabilities.control || !connected || pending || !plan || !nextAllowed}>다음 시연 준비</button>
      <button className={styles.start} onClick={() => void action('start')} disabled={!capabilities.control || !connected || pending || !plan?.ready || !nextAllowed}>승인된 실험 시작</button>
      <button className={styles.stop} onClick={() => void action('stop')} disabled={!capabilities.control || pending || !state?.active || state.active.state !== 'RUNNING' || state.active.stopConfirmed}>현재 실행 중지</button>
    </div>
    {executing ? <p className={styles.notice} role="status">현재 실행 진행 중 · 다음 시연은 정지 · baseline 복원 · 증거 보존 · 임시 객체 정리 확인 후 가능합니다.</p> : blockedRun ? <div className={styles.lifecyclePanel} aria-label="다음 시연 전 확인">
      <LifecycleStatus run={blockedRun} />
      <div className={styles.reconcile}><button onClick={() => void action('reconcile')} disabled={!capabilities.control || !connected || pending || !canReconcile(blockedRun)}>{blockedRun.state === 'RECONCILING' ? '종료 · 정리 확인 중' : '종료 · 복원 · 정리 확인'}</button><p>확인과 정리만 이어갑니다. 장애 주입이나 모델 호출은 다시 시작하지 않습니다.</p></div>
    </div> : state ? <p className={styles.notice} role="status">{nextAllowed ? '이전 실행의 종료 절차가 확인됐습니다. 다음 시연에는 새 run과 새 승인 계획이 필요합니다.' : '다음 실행 허용 상태를 확인하지 못했습니다. 새 시연은 차단됩니다.'}</p> : null}
    {!capabilities.control ? <p className={styles.notice}>브라우저 실행 승인 대기 · 화면은 관측 전용입니다. 승인된 실행과 중지는 호스트 실행기가 담당하며, 실제 원장 이벤트만 표시합니다.</p> : null}
    <div className={styles.footline}><span>{run ? sourceLabel(run.source) : plan ? sourceLabel(plan.source) : '새 승인 계획 필요'}{state?.active ? ` · ${state.active.runId}` : ''}</span>{plan ? <details><summary>실행 계획 · 기한 · 호출 한도</summary><p>실행 상한 {plan.totalSeconds}초 · 승인 만료 {plan.expiresAt}{plan.model ? ` · ${plan.model} · 최대 ${plan.maxModelCalls ?? '미확인'}회 모델 호출` : ''}</p></details> : null}</div>
    {state?.active?.state === 'UNCONFIRMED_TERMINATION' ? <p role="alert" className={styles.error}>이전 실행의 종료가 불명확합니다. 새 실행은 차단됩니다. 원장을 삭제하거나 이전 승인을 재사용하지 마세요.</p> : null}
    {error ? <p role="alert" className={styles.error}>{error}</p> : null}
    {eventError ? <p role="alert" className={styles.error}>{eventError}</p> : null}
    {run ? <EngineeringSummary run={run} events={events} onSelect={seq => { setSelectedSeq(seq); document.getElementById('exhibition-evidence')?.setAttribute('open', ''); }} /> : null}
    <details id="exhibition-evidence" className={styles.evidence}><summary>실행 기록 · Replay · 공개 근거 ({events.length})</summary>
    <div className={styles.history}><label>현재 실행 / 완료된 Replay<select value={selectedRun} onChange={e => { setSelectedRun(e.target.value); setViewHistory(false); }}>{!state?.runs.length ? <option value="">아직 실행 없음</option> : state.runs.map(r => <option key={r.runId} value={r.runId}>{isTerminal(r) ? 'Replay' : '현재'} · {r.runId} · {r.state}</option>)}</select></label>{run ? <span>{historical ? 'REPLAY · 종료된 실행 기록' : 'LIVE · 현재 run'} · {sourceLabel(run.source)}</span> : <span>실제 실행 이벤트를 기다립니다. 시간만으로 단계를 진행하지 않습니다.</span>}</div>
    {run ? <><RunSummary run={run} />{historical ? <button onClick={() => setViewHistory(value => !value)}>{viewHistory ? '현재 Live 관측으로 돌아가기' : '이 실행 Replay 보기'}</button> : null}<PublicArtifacts key={run.runId} runId={run.runId} /></> : null}
    {events.length ? <div className={styles.stream}>
      <ol aria-label="run 이벤트 순서">{events.map(event => <li key={event.seq}><button aria-current={selected?.seq === event.seq ? 'step' : undefined} onClick={() => setSelectedSeq(event.seq)}><small>#{event.seq} · {event.actor} · {event.at}</small><strong>{eventLabel(event.kind)}</strong></button></li>)}</ol>
      <div className={styles.event} aria-live="polite"><button onClick={() => setSelectedSeq(null)} disabled={selectedSeq === null}>최신 이벤트 따라가기</button>{selected ? <><p>{historical ? '기록 Replay' : sourceLabel(selected.source)} · {selected.actor}</p><h2>{eventLabel(selected.kind)}</h2><EventFacts event={selected} /><details><summary>공개 가능한 이벤트 근거 · 로그 · diff</summary><pre>{JSON.stringify(selected.data, null, 2)}</pre></details></> : null}</div>
    </div> : <p className={styles.empty}>확인된 이벤트 없음 · 성공이나 복구 상태를 추정하지 않습니다.</p>}
    </details>
  </section>;
}
function RunSummary({run}: {run: ExhibitionRun}) {
  return <div className={styles.runSummary}><strong>{run.state}</strong><span>종료 확인: {run.stopConfirmed ? '확인됨' : '미확인'}</span><span>회차 정리: {lifecycleComplete(run) ? '보존 · 복원 · 정리 확인됨' : '추가 확인 필요'}</span><span>PR 생성은 배포·서비스 회복과 별도입니다.</span><details><summary>현재 run · UID · 정리 근거</summary><pre>{JSON.stringify({binding: run.binding, lifecycleReceiptSHA256: run.lifecycleReceiptSHA256 ?? null}, null, 2)}</pre></details></div>;
}
export function LifecycleStatus({run}: {run: ExhibitionRun}) {
  const checks = [['stopConfirmed', 'Policy 정지'], ['modelStopped', '모델 · 실행 프로세스 종료'], ['baselineRestored', 'baseline 복원'], ['evidenceArchived', '증거 보존 · 무결성'], ['objectsCleaned', '임시 객체 정리']] as const;
  return <div className={styles.lifecycle} role="status"><strong>{lifecycleComplete(run) ? '회차 종료 절차 확인됨' : '이전 회차 확인 전 다음 시연 차단'}</strong><ul>{checks.map(([key, label]) => <li key={key} data-confirmed={run.lifecycle?.[key] === true}><span>{label}</span><b>{run.lifecycle?.[key] === true ? '확인됨' : '미확인'}</b></li>)}</ul>{run.lifecycle?.reasonCode ? <details><summary>확인 상태 근거</summary><code>{run.lifecycle.reasonCode}</code></details> : null}</div>;
}
function EventFacts({event}: {event: ExhibitionEvent}) {
  const keys = ['status', 'latencyMs', 'podUID', 'pod', 'node', 'selectedNode', 'httpStatus', 'passed', 'total', 'iteration', 'model', 'resourceCondition'];
  return <><InputComparison data={event.data} /><dl className={styles.facts}>{keys.filter(key => key in event.data).map(key => <div key={key}><dt>{key === 'resourceCondition' ? '실험 입력 · 실측과 적용값' : key}</dt><dd>{typeof event.data[key] === 'object' ? JSON.stringify(event.data[key]) : String(event.data[key])}</dd></div>)}</dl></>;
}

export function ArtifactContent({artifacts}: {artifacts: PublicArtifact[]}) {
  return <>{artifacts.length ? artifacts.map((artifact, index) => <article key={`${artifact.sha256}:${index}`}><h3>{artifact.kind === 'diff' ? '검토된 코드 diff' : '동일 입력의 baseline / 후보 검증 결과'}</h3><code>SHA-256 {artifact.sha256}</code><pre>{artifact.content}</pre></article>) : <p>공개 승인된 정확한 후보 근거가 아직 없습니다. 검토·공개 승인 대기와 검증 실패는 별개입니다.</p>}</>;
}
function PublicArtifacts({runId}: {runId: string}) {
  const [artifacts, setArtifacts] = useState<PublicArtifact[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  const busy = useRef(false);
  async function load() {
    if (busy.current) return;
    busy.current = true; setLoading(true); setFailed(false);
    try {
      const result = parseArtifacts(await getJSON(`/api/exhibition/artifacts?runId=${encodeURIComponent(runId)}`));
      // The server authorizes disclosure; verify the received public bytes independently.
      for (const artifact of result) {
        const actual = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(artifact.content));
        const digest = Array.from(new Uint8Array(actual), byte => byte.toString(16).padStart(2, '0')).join('');
        if (digest !== artifact.sha256) throw Error('ARTIFACT_HASH_MISMATCH');
      }
      setArtifacts(result);
    } catch { setArtifacts(null); setFailed(true); }
    finally { busy.current = false; setLoading(false); }
  }
  return <details><summary>공개 승인된 코드 diff · 테스트 결과</summary><p>서버에서 정확한 해시의 공개가 승인된 자료만 표시합니다. 인증정보·비공개 원문 로그는 제공하지 않습니다.</p><button disabled={loading} onClick={() => void load()}>{loading ? '근거 확인 중' : '이 run의 공개 근거 조회'}</button>{failed ? <p role="alert">공개 근거를 검증하지 못했습니다. 내용은 표시하지 않습니다.</p> : null}{artifacts ? <ArtifactContent artifacts={artifacts} /> : null}</details>;
}

export function EngineeringSummary({run, events, onSelect}: {run: ExhibitionRun; events: ExhibitionEvent[]; onSelect?: (seq: number) => void}) {
  const summary = engineeringSummary(run, events);
  if (!summary || !events.length) return null;
  return <section className={styles.engineeringSummary} aria-label="이번 run의 Runtime·Engineering 결과">
    <div className={styles.resultHeading}><strong>{isTerminal(run) ? '저장 기록 · 이 회차의 결과' : '현재 실행 · 확인된 단계'}</strong><code>{run.runId}</code></div>
    <p>Runtime: {summary.runtimeExhausted ? 'FAILED / RUNTIME_BUDGET_EXHAUSTED · 복구 한계 확인' : '적격 Runtime 종료 근거 확인 전'} · 전체 회차: {summary.executionState}</p>
    <p>{summary.pendingReview ? 'VERIFIED_PENDING_REVIEW · 후보 검증 완료 · 검토 대기 · PR 미게시 · 앱 미배포' : '모델 수정·후보 검증 완료 여부는 이 회차의 근거로만 판단합니다.'}</p>
    <ol className={styles.milestones}>{summary.milestones.map(step => <li key={step.label} data-confirmed={!!step.event}>
      <button type="button" disabled={!step.event || !onSelect} onClick={() => step.event && onSelect?.(step.event.seq)}>
        <strong>{step.label}</strong><span>{step.event ? `#${step.event.seq} · ${step.event.actor}` : '근거 확인 전'}</span>
      </button>
    </li>)}</ol>
    <p className={styles.notice}>단계별 근거 요약입니다. #번호는 실제 원장 기록 순서이며, 동일 결함 근거의 수집·검증은 정지 후 확정될 수 있습니다.</p>
    {summary.comparison && summary.verification ? <InputComparison data={summary.verification.data} /> : null}
    <p className={styles.notice}>Runtime 한계는 전체 회차 실패와 별개입니다. 정리 완료는 모델 검증 성공이나 앱 배포를 뜻하지 않습니다.</p>
  </section>;
}

export function InputComparison({data}: {data: Record<string, unknown>}) {
  if (!Array.isArray(data.baseline) || !Array.isArray(data.candidate)) return null;
  const facts = comparisonFacts(data);
  if (!facts) return <p>동일 입력 연결 미확인 · 검증 자료 누락 또는 형식 오류</p>;
  const {baseline: before, candidate: after, sameInput: same} = facts;
  const names = [...new Set([...before, ...after].map(c => c.name))];
  const show = (c?: typeof before[number]) => c ? `${c.passed ? 'PASS' : 'FAIL'}${typeof c.httpStatus === 'number' ? ` · HTTP ${c.httpStatus}` : ''}` : '미확인';
  return <section aria-label="원본과 검증 후보 비교"><h3>원본 / 검증 후보</h3><p>{same ? '동일 입력 SHA-256 확인됨' : '동일 입력 연결 미확인 · 비교 성공을 선언하지 않습니다'}</p>
    {same ? <p>후보 검사 {facts.candidatePassed}/{facts.candidateTotal} PASS · {facts.repaired ? '원본 FAIL(500) → 후보 PASS(200)' : '수정 성공 근거 확인 전'}</p> : null}
    <details className={styles.comparisonDetails}><summary>검사별 원본·후보 결과 {names.length}개</summary><div style={{overflowX: 'auto'}}><table><thead><tr><th scope="col">검사</th><th scope="col">원본</th><th scope="col">후보</th></tr></thead><tbody>{names.map(name => <tr key={name}><th scope="row">{name}</th><td>{show(before.find(c => c.name === name))}</td><td>{show(after.find(c => c.name === name))}</td></tr>)}</tbody></table></div></details></section>;
}
