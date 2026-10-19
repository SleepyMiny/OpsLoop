import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {describe, expect, it} from 'vitest';
import evidence from './evidence.json';
import fixture from '../../../contracts/bff-snapshot.json';
import {parseReplay, ReplayDataSource} from './replaySource';
import {HttpDataSource, parseSnapshot} from '../live/httpSource';
import {createDataSource} from '../createDataSource';
import {displayIncident} from '../../domain/displayIncident';
import {buildBoardModel} from '../../components/candidates/model';
import {Header} from '../../app/Header';
import {CandidateBoard} from '../../components/candidates/CandidateBoard';
import {IncidentPanel} from '../../components/incident/IncidentPanel';
import {PipelineLadder} from '../../components/incident/PipelineLadder';
import {ClusterMimic} from '../../components/cluster/ClusterMimic';
import {EngineeringTrack} from '../../components/engineering/EngineeringTrack';
import {PolicyPanel} from '../../components/policy/PolicyPanel';
import {buildLadder, isInFlight, ladderStatus} from '../../components/incident/ladderModel';
import {describeTrigger} from '../../components/incident/incidentView';

const copy = () => structuredClone(evidence);
describe('Observed evidence is historical, not live recovery', () => {
  for (const id of ['image-pull', 'node-not-ready']) {
    it(`${id}: preserves real terminal phase, attempts and stored Decision`, () => {
      const source = new ReplayDataSource(); source.select(id);
      expect(source.getSnapshot().policy.spec.enabled).toBe(false);
      expect(displayIncident(source.getSnapshot())).toBeNull();
      source.setFrame(1);
      const snapshot = source.getSnapshot();
      const record = displayIncident(snapshot)!;
      expect(snapshot.source).toBe('replay');
      expect(snapshot.activeIncident).toBeNull();
      expect(snapshot.cluster.nodes).toEqual([]);
      expect(snapshot.http.probes).toEqual([]);
      expect(record.status.runtime.phase).toBe(id === 'image-pull' ? 'MANUAL_REQUIRED' : 'OBSERVED');
      expect(record.status.runtime.attempts).toEqual([]);
      expect(isInFlight(record.status.runtime.phase)).toBe(false);
      expect(buildLadder(record.status.runtime).inFlight).toBe(false);
      expect(ladderStatus(record.status.runtime,snapshot.policy.spec).pulse).toBe(false);
      expect(record.status.runtime.classification).toBe(id === 'image-pull' ? 'CONFIGURATION_FAILURE' : 'INFRASTRUCTURE_FAILURE');
      expect(record.spec.trigger.type).toBe(id === 'image-pull' ? 'ImagePull' : 'NodeNotReady');
      expect(describeTrigger(record.spec.trigger).code).toBe(id === 'image-pull' ? 'IMAGE_PULL' : 'NODE_NOT_READY');
      const before = structuredClone(record.status.runtime.decision);
      const board = buildBoardModel(snapshot);
      if (id === 'node-not-ready') {
        expect(board.kind).toBe('decision');
        expect(board.attempt).toBeNull();
        expect(board.winner?.actionLabel).toContain('배치 변경 없음');
        expect(record.status.runtime.decision?.selectedHostname).toBe('opsloop-control');
        expect(record.status.runtime.timings.decidedAt).toBeDefined();
        expect(board.rows.length).toBeGreaterThan(0);
      }
      expect(record.status.runtime.decision).toEqual(before);
      // Empty current nodes/unknown image platforms must not re-evaluate the stored decision.
      for (const Component of [CandidateBoard, IncidentPanel, PipelineLadder, ClusterMimic, EngineeringTrack, PolicyPanel]) {
        expect(() => renderToStaticMarkup(createElement(Component, {snapshot}))).not.toThrow();
      }
      expect(displayIncident({...snapshot, source: 'live'})).toBeNull();
      expect(snapshot.replay?.provenance.every((p) => /^[0-9a-f]{64}$/.test(p.sha256))).toBe(true);
    });
  }
  it('rejects Enforce, nonzero attempts, unsupported version and invented active incidents', () => {
    const enforce = copy(); enforce.experiments[0]!.frames[0]!.snapshot.policy.spec.mode = 'Enforce';
    expect(() => parseReplay(enforce)).toThrow(/ObserveOnly/);
    const active = copy(); (active.experiments[0]!.frames[1]!.snapshot as Record<string, unknown>).activeIncident = active.experiments[0]!.frames[1]!.snapshot.recentIncidents[0];
    expect(() => parseReplay(active)).toThrow(/ObserveOnly/);
    const attempts = copy(); (attempts.experiments[0]!.frames[1]!.snapshot.recentIncidents[0]!.status.runtime as Record<string, unknown>).attempts = [{}];
    expect(() => parseReplay(attempts)).toThrow();
    expect(() => parseReplay({...copy(), schemaVersion: 'future'})).toThrow();
  });
  it('rejects malformed provenance and milestone timestamps before rendering', () => {
    const time = copy(); (time.experiments[0]!.milestones[0] as unknown as Record<string, unknown>).at = 'not-a-date';
    expect(() => parseReplay(time)).toThrow(/milestone/);
    const traversal = copy(); traversal.experiments[0]!.provenance[0]!.file = '../credential';
    expect(() => parseReplay(traversal)).toThrow(/출처/);
  });
  it('only changes discrete recorded frames; subscribers see stable snapshots', () => {
    const source = new ReplayDataSource(); const first = source.getSnapshot();
    source.start(); expect(source.getSnapshot()).toBe(first);
    let calls = 0; const unsub = source.subscribe(() => calls++);
    source.setFrame(1); expect(calls).toBe(1); expect(source.getSnapshot()).not.toBe(first);
    expect(() => source.setFrame(2)).toThrow(); expect(() => source.select('invented')).toThrow(); unsub(); source.stop();
  });
  it('uses explicit mode selection and distinct headers', () => {
    expect(createDataSource({search:'?source=replay', env:{}}).kind).toBe('replay');
    expect(() => createDataSource({search:'?source=bogus', env:{}})).toThrow();
    const source = new ReplayDataSource();
    const html = renderToStaticMarkup(createElement(Header,{source,status:source.getStatus(),playback:null,theme:'auto',onCycleTheme:()=>{},onOpenReality:()=>{}}));
    expect(html).toContain('Evidence Replay'); expect(html).toContain('현재 상태 아님');
    expect(html).toContain('보존 시점'); expect(html).not.toContain('연결됨');
  });
});

describe('Go BFF fixture → Live adapter contract', () => {
  it('parses actual Go serialization without inventing incidents, platforms or probes', () => {
    const snapshot = parseSnapshot(fixture);
    expect(snapshot.source).toBe('live'); expect(snapshot.policy.spec.mode).toBe('ObserveOnly');
    expect(snapshot.activeIncident).toBeNull(); expect(snapshot.recentIncidents).toEqual([]);
    expect(snapshot.workload.platforms).toEqual([]); expect(snapshot.http.probes).toEqual([]);
    expect(snapshot.cluster.nodes[0]!.requested.cpuMillicores).toBe(50);
    expect(snapshot.cluster.nodes[0]!.usage.cpu.observedAt).toBe(fixture.observedAt - 20_000);
    expect(snapshot.cluster.nodes[0]!.usage.memory.observedAt).toBe(fixture.observedAt - 30_000);
    expect(snapshot.policy.policyHash).toMatch(/^[a-f0-9]{64}$/);
  });
  it('polls same-origin API and reports failures without Demo fallback', async () => {
    const urls: string[] = [];
    const source = new HttpDataSource({baseUrl:'/',now:()=>fixture.observedAt, pollMs:60_000,fetchImpl: (async (url) => {urls.push(String(url)); return new Response(JSON.stringify(fixture));}) as typeof fetch});
    const done = new Promise<void>((resolve) => source.subscribe(()=>resolve()));
    source.start(); await done; source.stop();
    expect(urls).toEqual(['/api/v1/snapshot']); expect(source.getStatus().state).toBe('ok');
    expect(source.getSnapshot()?.recentIncidents).toEqual([]);
    const failed = new HttpDataSource({baseUrl:'/',fetchImpl: (async()=>new Response('{}',{status:503})) as typeof fetch});
    const error = new Promise<void>((resolve)=>failed.subscribe(()=>resolve())); failed.start(); await error; failed.stop();
    expect(failed.getStatus().state).toBe('error'); expect(failed.getSnapshot()).toBeNull();
  });
});
