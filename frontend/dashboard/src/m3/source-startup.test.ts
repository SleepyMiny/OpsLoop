import {describe, it, expect} from 'vitest';
import {eventLabel} from './ExhibitionConsole';
import {eventFocus, mergeEvents, type ExhibitionEvent} from './exhibition';
describe('current source startup presentation', () => {
 it('keeps pending, identity bound and rejected separate from success', () => {
  const labels=['SOURCE_PENDING','SOURCE_BOUND','SOURCE_REJECTED'].map(eventLabel);
  expect(labels[0]).toContain('기동 대기'); expect(labels[1]).toContain('복구 성공 아님'); expect(labels[2]).toContain('검증 거부');
 });
 it('focuses current source and rejects another run event', () => {
  const e: ExhibitionEvent={runId:'current-b',seq:1,at:'2026-10-30T01:00:00Z',actor:'controller',kind:'SOURCE_PENDING',source:'measured',data:{phase:'PENDING'}};
  expect(eventFocus(e,'engineering')).toBe('cluster');
  expect(mergeEvents([],{events:[e]},'current-b')).toHaveLength(1);
  expect(()=>mergeEvents([],{events:[{...e,runId:'old-b1'}]},'current-b')).toThrow();
 });
});
