import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import {describe, expect, it} from 'vitest';
const testEnv = (globalThis as typeof globalThis & {process?: {env: Record<string, string | undefined>}}).process?.env ?? {};
import {HttpDataSource} from './httpSource';
import {CandidateBoard} from '../../components/candidates/CandidateBoard';
import {ClusterMimic} from '../../components/cluster/ClusterMimic';
import {IncidentPanel} from '../../components/incident/IncidentPanel';
import {PipelineLadder} from '../../components/incident/PipelineLadder';
import {EngineeringTrack} from '../../components/engineering/EngineeringTrack';
import {PolicyPanel} from '../../components/policy/PolicyPanel';

/** Explicit opt-in: never starts servers, kubectl, faults or cluster mutations. */
describe.skipIf(!testEnv.OPSLOOP_LIVE_BFF_URL)('Actual read-only BFF integration', () => {
  it('joins current Kubernetes/Prometheus and renders Live without invented Incidents', async () => {
    const source = new HttpDataSource({baseUrl:testEnv.OPSLOOP_LIVE_BFF_URL, pollMs:60_000});
    try {
      const update = new Promise<void>((resolve) => source.subscribe(()=>resolve()));
      source.start(); await update;
      expect(source.getStatus().state).toBe('ok');
      const snapshot = source.getSnapshot()!;
      expect(snapshot.source).toBe('live');
      expect(Math.abs(Date.now()-snapshot.observedAt)).toBeLessThan(15_000);
      expect(snapshot.cluster.nodes).toHaveLength(3);
      expect(snapshot.cluster.nodes.every((n)=>n.readiness==='Ready')).toBe(true);
      expect(snapshot.cluster.nodes.map((n)=>n.arch)).toContain('arm64');
      expect(snapshot.policy.spec.mode).toBe('ObserveOnly'); expect(snapshot.policy.spec.enabled).toBe(true);
      expect(snapshot.policy.status.conditions).toContainEqual(expect.objectContaining({type:'Monitoring',status:'True',reason:'Active'}));
      expect(snapshot.activeIncident).toBeNull(); expect(snapshot.recentIncidents).toEqual([]);
      expect(snapshot.diagnostics?.readOnly).toBe(true);
      expect(snapshot.diagnostics?.metricsAvailable).toBe(true);
      for (const node of snapshot.cluster.nodes) {
        expect(node.usage.cpu.value).not.toBeNull(); expect(node.usage.memAvailableBytes.value).not.toBeNull();
        expect(snapshot.observedAt-node.usage.cpu.observedAt).toBeLessThanOrEqual(90_000);
        expect(snapshot.observedAt-node.usage.memory.observedAt).toBeLessThanOrEqual(90_000);
      }
      for (const Component of [CandidateBoard,ClusterMimic,IncidentPanel,PipelineLadder,EngineeringTrack,PolicyPanel]) {
        const html = renderToStaticMarkup(createElement(Component,{snapshot}));
        expect(html).not.toContain('PR_CREATED'); expect(html).not.toContain('NaN');
      }
    } finally {source.stop();}
  });
});
