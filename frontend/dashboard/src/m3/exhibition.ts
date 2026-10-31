/** Public, server-projected exhibition contract. No credential or command inputs. */
export interface Capabilities { live: boolean; observationConfigured?: boolean; lastObservationAvailable?: boolean; control: boolean; mode: 'live' | 'replay'; csrfToken?: string }
export interface RunBinding { runID: string; epoch: string; policyUID: string; deploymentUID: string }
export interface RunLifecycle {
  stopConfirmed: boolean; baselineRestored: boolean; evidenceArchived: boolean;
  objectsCleaned: boolean; modelStopped: boolean; reasonCode: string;
}
export interface ExhibitionRun {
  runId: string; planId: string; track: 'runtime' | 'engineering'; state: string;
  source: 'measured' | 'synthetic'; binding: RunBinding; lastSeq: number; stopConfirmed: boolean; executionState?: string;
  // Missing receipts from an older server/history remain readable, never reusable.
  lifecycleComplete?: boolean; lifecycle?: RunLifecycle; lifecycleReceiptSHA256?: string | null;
}
export interface PublicPlan {
  planId: string; track: 'runtime' | 'engineering'; expiresAt: string; source: 'measured' | 'synthetic';
  binding: RunBinding; ready: boolean; model?: string; maxModelCalls?: number; totalSeconds: number;
}
export interface ExhibitionState { active: ExhibitionRun | null; runs: ExhibitionRun[]; plans: PublicPlan[]; nextRunAllowed?: boolean }
export interface ExhibitionEvent { runId: string; seq: number; at: string; actor: string; kind: string; source: 'measured' | 'synthetic'; data: Record<string, unknown> }
const object = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const nonempty = (v: unknown): v is string => typeof v === 'string' && v.length > 0;
const integer = (v: unknown) => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0;
const source = (v: unknown) => v === 'measured' || v === 'synthetic';
const track = (v: unknown) => v === 'runtime' || v === 'engineering';
const binding = (v: unknown) => object(v) && ['runID', 'epoch', 'policyUID', 'deploymentUID'].every(k => nonempty(v[k]));
export function parseCapabilities(v: unknown): Capabilities {
  if (!object(v) || typeof v.live !== 'boolean' || typeof v.control !== 'boolean' || !['live', 'replay'].includes(String(v.mode)) || v.control && !nonempty(v.csrfToken)) throw Error('INVALID_CAPABILITIES');
  return v as unknown as Capabilities;
}
export function parseState(v: unknown): ExhibitionState {
  const lifecycle = (r: unknown) => object(r) && ['stopConfirmed', 'baselineRestored', 'evidenceArchived', 'objectsCleaned', 'modelStopped'].every(k => typeof r[k] === 'boolean') && typeof r.reasonCode === 'string';
  const run = (r: unknown) => object(r) && nonempty(r.runId) && nonempty(r.planId) && track(r.track) && source(r.source) && binding(r.binding) && nonempty(r.state) && integer(r.lastSeq) && typeof r.stopConfirmed === 'boolean'
    && object(r.binding) && r.binding.runID === r.runId && (r.executionState === undefined || nonempty(r.executionState))
    && (r.lifecycleComplete === undefined || typeof r.lifecycleComplete === 'boolean')
    && (r.lifecycle === undefined || lifecycle(r.lifecycle))
    && (r.lifecycleReceiptSHA256 === undefined || r.lifecycleReceiptSHA256 === null || typeof r.lifecycleReceiptSHA256 === 'string' && /^[a-f0-9]{64}$/.test(r.lifecycleReceiptSHA256));
  const plan = (p: unknown) => object(p) && nonempty(p.planId) && track(p.track) && source(p.source) && binding(p.binding) && typeof p.expiresAt === 'string' && Number.isFinite(Date.parse(p.expiresAt)) && typeof p.ready === 'boolean' && integer(p.totalSeconds);
  if (!object(v) || !Array.isArray(v.runs) || !v.runs.every(run) || !Array.isArray(v.plans) || !v.plans.every(plan) || v.active !== null && !run(v.active) || v.nextRunAllowed !== undefined && typeof v.nextRunAllowed !== 'boolean') throw Error('INVALID_STATE');
  return v as unknown as ExhibitionState;
}
/** Fail closed on cross-run, out-of-order, gaps or conflicting duplicate events. */
export function mergeEvents(previous: ExhibitionEvent[], input: unknown, runId: string): ExhibitionEvent[] {
  if (!object(input) || !Array.isArray(input.events)) throw Error('INVALID_EVENTS');
  const result = [...previous];
  for (const item of input.events) {
    if (!object(item) || item.runId !== runId || !integer(item.seq) || Number(item.seq) < 1 || !nonempty(item.at) || !Number.isFinite(Date.parse(item.at)) || !nonempty(item.actor) || !nonempty(item.kind) || !source(item.source) || !object(item.data)) throw Error('INVALID_EVENT_BINDING');
    const event = item as unknown as ExhibitionEvent;
    const old = result.find(e => e.seq === event.seq);
    if (old) { if (JSON.stringify(old) !== JSON.stringify(event)) throw Error('EVENT_CHANGED'); continue; }
    if (event.seq !== (result.at(-1)?.seq ?? 0) + 1) throw Error('EVENT_SEQUENCE_GAP');
    result.push(event);
  }
  return result;
}
// Replay availability is separate from permission to prepare another run.
export const isTerminal = (run: ExhibitionRun) => ['COMPLETED', 'FAILED', 'STOPPED', 'RECONCILIATION_REQUIRED', 'RECONCILING'].includes(run.state) && run.stopConfirmed;
export const lifecycleComplete = (run: ExhibitionRun) => run.lifecycleComplete === true && run.stopConfirmed
  && !!run.lifecycle && ['stopConfirmed', 'baselineRestored', 'evidenceArchived', 'objectsCleaned', 'modelStopped'].every(k => run.lifecycle?.[k as keyof RunLifecycle] === true)
  && typeof run.lifecycleReceiptSHA256 === 'string' && /^[a-f0-9]{64}$/.test(run.lifecycleReceiptSHA256);
/** Server permission and every retained receipt must agree; stop alone never releases the next run. */
export const canPrepareNext = (state: ExhibitionState | null) => state?.nextRunAllowed === true
  && state.active === null && state.runs.every(lifecycleComplete);
export const canReconcile = (run?: ExhibitionRun | null) => !!run && !lifecycleComplete(run)
  && ['COMPLETED', 'FAILED', 'STOPPED', 'UNCONFIRMED_TERMINATION', 'RECONCILIATION_REQUIRED'].includes(run.state);
export const sourceLabel = (value: string) => value === 'synthetic' ? '개발용 합성 · 실환경 성공 증거 아님' : '실행 관측';
export function eventFocus(event?: ExhibitionEvent, track?: 'runtime' | 'engineering'): 'cluster' | 'pipeline' | 'candidates' | 'incident' | 'engineering' | undefined {
  if (!event) return undefined;
  if (/CANDIDATE|RESOURCE_CONDITION/.test(event.kind)) return 'candidates';
  if (/SOURCE_|POD|ENDPOINT|SERVICE_PROBE|FAULT/.test(event.kind)) return 'cluster';
  if (/MODEL|ANALYZ|PATCH|BASELINE|VERIFY|REVIEW|PR_/.test(event.kind)) return track === 'runtime' ? 'pipeline' : 'engineering';
  if (/CONTROLLER|RECOVER|STOP/.test(event.kind)) return 'pipeline';
  return 'incident';
}
export async function getJSON(path: string, signal?: AbortSignal): Promise<unknown> {
  const response = await fetch(path, { signal, credentials: 'same-origin', cache: 'no-store', redirect: 'error' });
  if (!response.ok) throw Error(`HTTP_${response.status}`);
  return response.json();
}
export interface PublicArtifact { kind: 'diff' | 'checks'; sha256: string; content: string }
export function parseArtifacts(value: unknown): PublicArtifact[] {
  if (!object(value) || !Array.isArray(value.artifacts) || value.artifacts.length > 16) throw Error('INVALID_ARTIFACTS');
  for (const artifact of value.artifacts) {
    if (!object(artifact) || !['diff', 'checks'].includes(String(artifact.kind)) || typeof artifact.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(artifact.sha256) || typeof artifact.content !== 'string' || artifact.content.length > 1_048_576 || Object.keys(artifact).some(k => !['kind', 'sha256', 'content'].includes(k))) throw Error('INVALID_ARTIFACT');
  }
  return value.artifacts as PublicArtifact[];
}


export interface ComparisonCheck { name: string; passed: boolean; httpStatus?: number; inputSHA256?: string }
/** These are display facts from the server's same-run verifier event, never an execution gate. */
export function comparisonFacts(data: Record<string, unknown>) {
  const checks = (value: unknown): ComparisonCheck[] | null => {
    if (!Array.isArray(value) || !value.length || value.some(v => !object(v) || !nonempty(v.name) || typeof v.passed !== 'boolean'
      || v.httpStatus !== undefined && (!Number.isSafeInteger(v.httpStatus) || Number(v.httpStatus) < 100 || Number(v.httpStatus) > 599)
      || v.inputSHA256 !== undefined && (typeof v.inputSHA256 !== 'string' || !/^[a-f0-9]{64}$/.test(v.inputSHA256)))) return null;
    if (new Set(value.map(v => v.name)).size !== value.length) return null;
    return value as ComparisonCheck[];
  };
  const baseline = checks(data.baseline), candidate = checks(data.candidate);
  if (!baseline || !candidate) return null;
  const before = baseline.find(c => c.name === 'empty-order-regression'), after = candidate.find(c => c.name === 'empty-order-regression');
  const sameInput = data.accepted === true && typeof before?.inputSHA256 === 'string' && before.inputSHA256 === after?.inputSHA256;
  return {baseline, candidate, sameInput, candidatePassed: candidate.filter(c => c.passed).length,
    candidateTotal: candidate.length, repaired: sameInput && baseline.length === candidate.length && baseline.every(c => candidate.some(v => v.name === c.name)) && before?.passed === false && before.httpStatus === 500
      && after?.passed === true && after.httpStatus === 200 && candidate.every(c => c.passed)};
}

export interface EngineeringMilestone { label: string; event?: ExhibitionEvent }
/** No timer, no inference from COMPLETED/cleanup: use only ordered events of this exact run. */
export function engineeringSummary(run: ExhibitionRun, input: ExhibitionEvent[]) {
  if (run.track !== 'engineering' || run.binding.runID !== run.runId) return null;
  let events: ExhibitionEvent[];
  try { events = mergeEvents([], {events: input}, run.runId); } catch { return null; }
  if (events.some(e => e.source !== run.source)) return null;
  const find = (kind: string, check: (e: ExhibitionEvent) => boolean = () => true) => events.find(e => e.kind === kind && check(e));
  const terminal = find('CONTROLLER_ACTION', e => e.data.phase === 'FAILED' && e.data.reasonCode === 'RUNTIME_BUDGET_EXHAUSTED' && nonempty(e.data.incidentUID));
  const collected = find('EVIDENCE_COLLECTED', e => e.data.phase === 'CURRENT_RUN_RUNTIME_EVIDENCE'
    && e.data.runID === run.runId && e.data.epoch === run.binding.epoch && e.data.incidentUID === terminal?.data.incidentUID
    && typeof e.data.sourceSHA256 === 'string' && /^[a-f0-9]{64}$/.test(e.data.sourceSHA256));
  const sealed = find('EVIDENCE_SEALED', e => e.data.phase === 'CURRENT_RUN_RUNTIME_EVIDENCE' && e.data.stopConfirmed === true
    && !!collected && e.data.incidentUID === collected.data.incidentUID && e.data.sourceSHA256 === collected.data.sourceSHA256);
  const limit = find('RUNTIME_LIMIT_CONFIRMED', e => !!collected && e.seq > collected.seq && e.data.phase === 'CURRENT_RUN_RUNTIME_EVIDENCE'
    && e.data.state === 'EXHAUSTED' && e.data.serviceRecovered === false && integer(e.data.attempt) && Number(e.data.attempt) > 0);
  const stop = find('RUNTIME_STAGE', e => e.data.phase === 'ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED')
    ?? find('STOP_CONFIRMED', e => e.data.stopConfirmed === true);
  const model = find('MODEL_REQUESTED', e => !!sealed && e.seq > sealed.seq && !!limit && e.seq > limit.seq);
  const proposal = find('PROPOSAL_RECEIVED', e => !!model && e.seq > model.seq);
  const verification = [...events].reverse().find(e => e.kind === 'VERIFICATION_RESULT' && !!proposal && e.seq > proposal.seq);
  const comparison = verification ? comparisonFacts(verification.data) : null;
  const verified = [...events].reverse().find(e => e.kind === 'VERIFIED_PENDING_REVIEW' && !!verification && e.seq > verification.seq
    && e.data.deployed === false && e.data.serviceRecovered === false && typeof e.data.patchSHA256 === 'string'
    && /^[a-f0-9]{64}$/.test(e.data.patchSHA256) && typeof e.data.sourceSHA256 === 'string' && /^[a-f0-9]{64}$/.test(e.data.sourceSHA256));
  const pendingReview = !!verified && !!stop && !!sealed && !!terminal && terminal.seq < stop.seq && stop.seq < sealed.seq && comparison?.repaired === true && !events.some(e => ['PR_CREATED', 'DEPLOYED', 'RECOVERED', 'SERVICE_RECOVERED'].includes(e.kind));
  const cleanup = find('LIFECYCLE_COMPLETE', e => lifecycleComplete(run) && ['baselineRestored', 'evidenceArchived', 'objectsCleaned', 'modelStopped'].every(k => e.data[k] === true));
  const milestones: EngineeringMilestone[] = [
    {label: 'Runtime 조치·한계 확인', event: limit ? terminal : undefined},
    {label: '조치 후 동일 결함 잔존', event: limit},
    {label: '강한 정지', event: stop}, {label: '증거 봉인', event: sealed},
    {label: '실제 모델 요청', event: model}, {label: '수정안 수신', event: proposal},
    {label: '독립 검증', event: verification}, {label: '복원·보존·정리', event: cleanup},
  ];
  return {milestones, runtimeExhausted: !!terminal && !!limit, pendingReview, comparison, verification,
    executionState: run.executionState ?? run.state, cleanupConfirmed: !!cleanup};
}
