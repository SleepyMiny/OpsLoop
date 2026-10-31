import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { ArtifactContent, ExhibitionConsole, LifecycleStatus, eventLabel } from './ExhibitionConsole';
import { canPrepareNext, canReconcile, eventFocus, isTerminal, lifecycleComplete, mergeEvents, parseArtifacts, parseCapabilities, parseState, type ExhibitionEvent, type ExhibitionRun, type RunLifecycle } from './exhibition';
const binding = {runID: 'run-1', epoch: 'epoch-1', policyUID: 'p-1', deploymentUID: 'd-1'};
const run: ExhibitionRun = {runId: 'run-1', planId: 'plan-1', track: 'runtime', source: 'measured', binding, state: 'RUNNING', lastSeq: 0, stopConfirmed: false};
const event = (seq: number): ExhibitionEvent => ({runId: 'run-1', seq, at: '2026-10-28T00:00:00Z', actor: 'coordinator', kind: 'CANDIDATES', source: 'measured', data: {status: 'observed'}});
describe('exhibition execution boundary', () => {
  it('keeps failed B2 separate from successful B1 and cleanup permission', () => {
    const b1 = {...run, runId: 'b1', planId: 'b1', binding: {...binding, runID:'b1'}, state:'COMPLETED', executionState:'COMPLETED', stopConfirmed:true};
    const b2 = {...run, runId: 'b2', planId: 'b2', binding: {...binding, runID:'b2'}, state:'STOPPED', executionState:'UNCONFIRMED_TERMINATION', stopConfirmed:true};
    const state=parseState({active:null,runs:[b2,b1],plans:[],nextRunAllowed:false});
    expect(state.runs[0]?.state).toBe('STOPPED');
    expect(canPrepareNext(state)).toBe(false);
    const successful={...event(1),runId:'b1',kind:'VERIFIED_PENDING_REVIEW'};
    expect(()=>mergeEvents([],{events:[successful]},'b2')).toThrow('INVALID_EVENT_BINDING');
    const stopped=mergeEvents([],{events:[{...event(1),runId:'b2',kind:'UNCONFIRMED_TERMINATION'},{...event(2),runId:'b2',kind:'NEXT_RUN_ALLOWED'}]},'b2');
    expect(stopped.some(e=>e.kind==='VERIFIED_PENDING_REVIEW'||e.kind==='COMPLETED')).toBe(false);
    expect(eventFocus(stopped[1])).not.toBe('engineering');
  });

  it('labels explicit circuit replay without claiming operational recovery', () => {
    expect(eventLabel('EXHIBITION_REPLAY_APPROVED')).toBe('승인된 전시 재실행 · 이번 회차 circuit 예외 · 운영 회복 선언 아님');
    expect(eventLabel('SERVICE_RECOVERED')).toBe('서비스 회복 확인');
  });
  it('only accepts bounded approved artifact kinds and renders markup as plain text', () => {
    const artifacts = parseArtifacts({artifacts: [{kind: 'diff', sha256: 'a'.repeat(64), content: '<script>secret()</script>'}]});
    const html = renderToStaticMarkup(createElement(ArtifactContent, {artifacts}));
    expect(html).toContain('&lt;script&gt;');
    expect(html).not.toContain('<script>');
    expect(() => parseArtifacts({artifacts: [{kind: 'rawlog', sha256: 'a'.repeat(64), content: 'private'}]})).toThrow();
    expect(() => parseArtifacts({artifacts: [{kind: 'diff', sha256: 'bad', content: 'text'}]})).toThrow();
  });
  it('requires server capabilities and CSRF before control', () => {
    expect(() => parseCapabilities({live: true, control: true, mode: 'live'})).toThrow();
    expect(parseCapabilities({live: false, control: false, mode: 'replay'}).live).toBe(false);
  });
  it('rejects incomplete identity and source distinctions', () => {
    const state = {active: run, runs: [run], plans: []};
    expect(parseState(state)).toEqual(state);
    expect(() => parseState({...state, active: {...run, source: 'historical'}})).toThrow();
    expect(() => parseState({...state, active: {...run, binding: {runID: 'run-1'}}})).toThrow();
  });
  it('reconnects with ordered events, exact duplicates only', () => {
    expect(mergeEvents([event(1)], {events: [event(1), event(2)]}, 'run-1')).toHaveLength(2);
    expect(() => mergeEvents([event(1)], {events: [{...event(1), kind: 'RECOVERED'}]}, 'run-1')).toThrow('EVENT_CHANGED');
    expect(() => mergeEvents([], {events: [event(2)]}, 'run-1')).toThrow('EVENT_SEQUENCE_GAP');
    expect(() => mergeEvents([], {events: [{...event(1), runId: 'older-run'}]}, 'run-1')).toThrow('INVALID_EVENT_BINDING');
    expect(() => mergeEvents([], {events: [{...event(1), seq: 1.5}]}, 'run-1')).toThrow();
  });
  it('never releases an unconfirmed run and distinguishes PR from recovery', () => {
    expect(isTerminal({...run, state: 'COMPLETED'})).toBe(false);
    expect(isTerminal({...run, state: 'UNCONFIRMED_TERMINATION', stopConfirmed: true})).toBe(false);
    expect(isTerminal({...run, state: 'PR_CREATED', stopConfirmed: true})).toBe(false);
    expect(isTerminal({...run, state: 'STOPPED', stopConfirmed: true})).toBe(true);
  });
  it('focus is derived solely from actual events', () => {
    expect(eventFocus()).toBeUndefined();
    expect(eventFocus(event(1))).toBe('candidates');
    expect(eventFocus({...event(1), kind: 'ENDPOINT'})).toBe('cluster');
    expect(eventFocus({...event(1), kind: 'PATCHING'})).toBe('engineering');
  });
  it('renders all operator controls disabled before a server state arrives', () => {
    const html = renderToStaticMarkup(createElement(ExhibitionConsole, {capabilities: {live: true, control: false, mode: 'live'}, onFocus: () => {}, onHistory: () => {}, onTrack: () => {}}));
    expect(html).toContain('실행 승인 대기');
    expect(html).toContain('아직 실행 없음');
    expect(html.match(/<button[^>]*disabled/g)).toHaveLength(3);
    expect(html).not.toContain('RECOVERED');
  });
  it('requires independently confirmed lifecycle flags and receipt before next preparation', () => {
    const flags: RunLifecycle = {stopConfirmed: true, baselineRestored: true, evidenceArchived: true, objectsCleaned: true, modelStopped: true, reasonCode: 'COMPLETE'};
    const complete: ExhibitionRun = {...run, state: 'COMPLETED', stopConfirmed: true, lifecycleComplete: true, lifecycle: flags, lifecycleReceiptSHA256: 'a'.repeat(64)};
    const state = {active: null, runs: [complete], plans: [], nextRunAllowed: true};
    expect(canPrepareNext(parseState(state))).toBe(true);
    for (const key of ['stopConfirmed', 'baselineRestored', 'evidenceArchived', 'objectsCleaned', 'modelStopped'] as const) {
      expect(canPrepareNext({...state, runs: [{...complete, lifecycle: {...flags, [key]: false}}]})).toBe(false);
    }
    expect(canPrepareNext({...state, nextRunAllowed: false})).toBe(false);
    expect(canPrepareNext({...state, active: complete})).toBe(false);
    expect(canPrepareNext({...state, runs: [{...complete, lifecycleReceiptSHA256: null}]})).toBe(false);
    expect(canPrepareNext({...state, runs: [complete, {...run, stopConfirmed: true, state: 'STOPPED'}]})).toBe(false);
    expect(canPrepareNext({active: null, runs: [], plans: []})).toBe(false);
  });
  it('keeps legacy and pending cleanup histories readable without granting a new run', () => {
    const pending = {...run, state: 'RECONCILIATION_REQUIRED', stopConfirmed: true};
    expect(parseState({active: pending, runs: [pending], plans: []}).runs).toHaveLength(1);
    expect(isTerminal(pending)).toBe(true);
    expect(lifecycleComplete(pending)).toBe(false);
    expect(canReconcile(pending)).toBe(true);
    expect(canReconcile({...pending, state: 'UNCONFIRMED_TERMINATION', stopConfirmed: false})).toBe(true);
    for (const state of ['RUNNING', 'STOPPING', 'RECONCILING', 'UNKNOWN']) expect(canReconcile({...pending, state})).toBe(false);
  });
  it('rejects malformed completion flags and hashes from the server', () => {
    for (const invalid of [{lifecycleComplete: 'true'}, {lifecycle: {stopConfirmed: true}}, {lifecycleReceiptSHA256: 'unverified'}]) {
      expect(() => parseState({active: null, runs: [{...run, ...invalid}], plans: [], nextRunAllowed: true})).toThrow('INVALID_STATE');
    }
    expect(() => parseState({active: null, runs: [], plans: [], nextRunAllowed: 'true'})).toThrow('INVALID_STATE');
  });
  it('renders missing lifecycle evidence as unconfirmed, never a successful cleanup', () => {
    const html = renderToStaticMarkup(createElement(LifecycleStatus, {run: {...run, stopConfirmed: true}}));
    expect(html).toContain('이전 회차 확인 전 다음 시연 차단');
    expect(html.match(/<b>미확인<\/b>/g)).toHaveLength(5);
    expect(html).not.toContain('회차 종료 절차 확인됨');
    expect(html).toContain('증거 보존 · 무결성');
  });
});
