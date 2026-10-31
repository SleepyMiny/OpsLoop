import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {describe, expect, it} from 'vitest';
import {EngineeringSummary} from './ExhibitionConsole';
import {comparisonFacts, engineeringSummary, parseState, type ExhibitionEvent, type ExhibitionRun} from './exhibition';
// Synthetic public-contract fixtures; no production identities, credentials or evidence bytes.
const run: ExhibitionRun = {runId:'current-run', planId:'current-plan', track:'engineering', source:'measured', state:'COMPLETED', executionState:'COMPLETED',
 binding:{runID:'current-run',epoch:'current-epoch',policyUID:'test-policy',deploymentUID:'test-deployment'},lastSeq:10,stopConfirmed:true,
 lifecycleComplete:true,lifecycleReceiptSHA256:'a'.repeat(64),lifecycle:{baselineRestored:true,evidenceArchived:true,modelStopped:true,objectsCleaned:true,stopConfirmed:true,reasonCode:'LIFECYCLE_COMPLETE'}};
const checks = (passed: boolean) => [{name:'empty-order-regression',passed,httpStatus:passed?200:500,inputSHA256:'b'.repeat(64)},
 ...Array.from({length:14},(_,i)=>({name:`synthetic-check-${i}`,passed:true}))];
const compare = {accepted:true,baseline:checks(false),candidate:checks(true)};
const steps: [string, Record<string,unknown>][] = [
 ['CONTROLLER_ACTION',{phase:'FAILED',reasonCode:'RUNTIME_BUDGET_EXHAUSTED',incidentUID:'test-incident'}],
 ['RUNTIME_STAGE',{phase:'ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED'}],
 ['EVIDENCE_SEALED',{phase:'CURRENT_RUN_RUNTIME_EVIDENCE',stopConfirmed:true,incidentUID:'test-incident',sourceSHA256:'c'.repeat(64)}],
 ['EVIDENCE_COLLECTED',{phase:'CURRENT_RUN_RUNTIME_EVIDENCE',runID:run.runId,epoch:run.binding.epoch,incidentUID:'test-incident',sourceSHA256:'c'.repeat(64)}],
 ['RUNTIME_LIMIT_CONFIRMED',{phase:'CURRENT_RUN_RUNTIME_EVIDENCE',state:'EXHAUSTED',attempt:1,serviceRecovered:false}],
 ['MODEL_REQUESTED',{attempt:1}],['PROPOSAL_RECEIVED',{attempt:1}],['VERIFICATION_RESULT',compare],
 ['VERIFIED_PENDING_REVIEW',{deployed:false,serviceRecovered:false,sourceSHA256:'d'.repeat(64),patchSHA256:'e'.repeat(64)}],
 ['LIFECYCLE_COMPLETE',{baselineRestored:true,evidenceArchived:true,objectsCleaned:true,modelStopped:true}],
];
const events = (): ExhibitionEvent[] => steps.map(([kind,data],i)=>({runId:run.runId,seq:i+1,at:`2026-01-01T00:00:${String(i).padStart(2,'0')}Z`,actor:'test-actor',kind,source:'measured',data:structuredClone(data)}));
const change = (kind:string,data:Record<string,unknown>) => events().map(e=>e.kind===kind?{...e,data:{...e.data,...data}}:e);
describe('same-run engineering display',()=>{
 it('separates a Runtime limit from the completed Engineering and lifecycle result',()=>{
  const summary=engineeringSummary(run,events());
  expect(summary?.runtimeExhausted).toBe(true);expect(summary?.pendingReview).toBe(true);expect(summary?.cleanupConfirmed).toBe(true);
  expect(summary?.milestones.every(s=>!!s.event)).toBe(true);
  const html=renderToStaticMarkup(createElement(EngineeringSummary,{run,events:events()}));
  for(const text of ['저장 기록','FAILED / RUNTIME_BUDGET_EXHAUSTED','COMPLETED','후보 검사 15/15 PASS','원본 FAIL(500) → 후보 PASS(200)','PR 미게시 · 앱 미배포']) expect(html).toContain(text);
 });
 it('does not derive a model result from terminal state, completion or cleanup alone',()=>{
  const cleanup=events().filter(e=>e.kind==='LIFECYCLE_COMPLETE').map((e,i)=>({...e,seq:i+1}));
  for(const state of ['COMPLETED','STOPPED','FAILED']) {
   const summary=engineeringSummary({...run,state,executionState:state},cleanup);
   expect(summary?.cleanupConfirmed).toBe(true);expect(summary?.pendingReview).toBe(false);expect(summary?.runtimeExhausted).toBe(false);
  }
 });
 it('never combines prior successes, another epoch, changed evidence or a partial event stream',()=>{
  expect(engineeringSummary(run,events().map(e=>({...e,runId:'old-run'})))).toBeNull();
  expect(engineeringSummary({...run,binding:{...run.binding,runID:'old-run'}},events())).toBeNull();
  expect(engineeringSummary(run,events().slice(1))).toBeNull();
  expect(engineeringSummary(run,events().map(e=>({...e,source:'synthetic'})))).toBeNull();
  for(const data of [{epoch:'old-epoch'},{runID:'old-run'},{incidentUID:'other-incident'},{sourceSHA256:'f'.repeat(64)},{phase:'BOUND_PRIOR_RUNTIME_EVIDENCE'}]) {
   expect(engineeringSummary(run,change('EVIDENCE_COLLECTED',data))?.pendingReview).toBe(false);
  }
 });
 it('requires the actual stop, seal, model and verification events for review-ready status',()=>{
  for(const kind of ['RUNTIME_STAGE','EVIDENCE_SEALED','MODEL_REQUESTED','PROPOSAL_RECEIVED','VERIFICATION_RESULT']) {
   const partial=events().map(e=>e.kind===kind?{...e,kind:'OTHER_EVENT'}:e);
   expect(engineeringSummary(run,partial)?.pendingReview).toBe(false);
  }
  expect(engineeringSummary(run,change('CONTROLLER_ACTION',{phase:'ABORTED',reasonCode:'POLICY_DISABLED'}))?.pendingReview).toBe(false);
  expect(engineeringSummary(run,change('VERIFIED_PENDING_REVIEW',{deployed:true}))?.pendingReview).toBe(false);
  expect(engineeringSummary(run,[...events(),{...events()[0]!,seq:11,kind:'PR_CREATED'}])?.pendingReview).toBe(false);
 });
 it('does not render unknown lifecycle evidence as completed cleanup',()=>{
  expect(engineeringSummary({...run,lifecycleComplete:false},events())?.cleanupConfirmed).toBe(false);
 });
 it('keeps the run identity and experiment outcome in the public state contract',()=>{
  expect(parseState({active:null,runs:[run],plans:[]}).runs[0]?.executionState).toBe('COMPLETED');
  expect(()=>parseState({active:null,runs:[{...run,binding:{...run.binding,runID:'another'}}],plans:[]})).toThrow('INVALID_STATE');
 });
});
describe('comparison display facts',()=>{
 it('requires matching inputs and complete named checks before displaying a repaired candidate',()=>{
  expect(comparisonFacts(compare)?.repaired).toBe(true);
  expect(comparisonFacts({...compare,candidate:checks(true).slice(0,14)})?.repaired).toBe(false);
  expect(comparisonFacts({...compare,candidate:[{...checks(true)[0],inputSHA256:'f'.repeat(64)},...checks(true).slice(1)]})?.repaired).toBe(false);
  expect(comparisonFacts({...compare,candidate:checks(true).map((c,i)=>i===3?{...c,passed:false}:c)})?.repaired).toBe(false);
 });
 it('rejects malformed, duplicate or empty checks instead of discarding them and claiming PASS',()=>{
  for(const candidate of [[],[...checks(true),checks(true)[0]],[{name:'bad',passed:'true'}],[{name:'bad',passed:true,httpStatus:200.5}]]) {
   expect(comparisonFacts({...compare,candidate})).toBeNull();
  }
 });
});
