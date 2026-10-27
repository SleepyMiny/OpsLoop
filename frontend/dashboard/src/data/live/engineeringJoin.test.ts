import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import wire from '../../../contracts/bff-snapshot.json';
import { EngineeringTrack } from '../../components/engineering/EngineeringTrack';
import { engineeringIncident, runtimeAlongside } from '../../components/engineering/runtimeState';
import { parseSnapshot } from './parseSnapshot';

function fixture() {
  const body = structuredClone(wire);
  const workload = body.workload;
  const incident = {
    apiVersion: 'opsloop.io/v1alpha1', kind: 'RecoveryIncident',
    runtimeBinding: { runID: 'runtime-run', epoch: 'epoch-1', policyUID: 'policy-uid', workloadUID: workload.uid, clusterUID: 'cluster-uid', namespaceUID: 'namespace-uid', grantSha256: 'd'.repeat(64) },
    metadata: { name: 'failed-incident', namespace: workload.namespace, uid: 'exact-uid' },
    spec: { workload: { uid: workload.uid }, policy: body.policy.spec, policyRef: { name: body.policy.metadata.name, uid: 'policy-uid' }, policyHash: 'a'.repeat(64), baseline: {}, detectedAt: '2026-10-27T00:00:00Z', trigger: { class: 'APPLICATION_FAILURE', reason: 'POD_NOT_READY', ruleVersion: 'v1', observedAt: '2026-10-27T00:00:00Z' } },
    status: { runtime: { phase: 'FAILED', reason: 'RUNTIME_BUDGET_EXHAUSTED' } },
  };
  const run = { runtimeRunID: 'runtime-run', epoch: 'epoch-1', policyUID: 'policy-uid', grantSha256: 'd'.repeat(64), incidentUID: 'exact-uid', namespace: workload.namespace, incidentName: 'failed-incident', workloadUID: workload.uid, status: {
    phase: 'PR_CREATED', jobId: 'real-run', iteration: 1, maxIterations: 1, baseCommit: 'a'.repeat(40), branch: 'repair',
    stages: ['build', 'unit', 'regression-fixture', 'independent-review'].map((stage) => ({ stage, iteration: 1, outcome: 'pass', summary: 'local verification passed' })),
    pullRequest: { number: 6, title: 'Actual repair', url: 'https://github.com/owner/repo/pull/6', headSha: 'b'.repeat(40) },
    evidence: { bundleSha256: 'a'.repeat(64), resultSha256: 'b'.repeat(64), prReceiptSha256: 'c'.repeat(64), remoteCI: 'NOT_RUN', model: 'approved-model', observedAt: '2026-10-27T00:00:00Z', executionMode: 'DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR' },
  } };
  return { ...body, policy: { ...body.policy, spec: { ...body.policy.spec, enabled: false } }, activeIncident: null, recentIncidents: [incident], engineeringRuns: [run] };
}

describe('hash-verified Engineering projection join', () => {
  it('preserves a matching recent terminal incident without opening an active incident', () => {
    const wire = fixture(); const snapshot = parseSnapshot(wire);
    expect(snapshot.activeIncident).toBeNull();
    expect(snapshot.recentIncidents[0]?.status.engineering?.phase).toBe('PR_CREATED');
    expect(snapshot).not.toHaveProperty('engineeringRuns');
    expect(wire.recentIncidents[0]?.status).not.toHaveProperty('engineering');
    const selected = engineeringIncident(snapshot);
    expect(selected?.status.runtime.phase).toBe('FAILED');
    expect(runtimeAlongside(snapshot, selected).tone).toBe('off');
    const html = renderToStaticMarkup(createElement(EngineeringTrack, { snapshot }));
    expect(html).toContain('종료된 Incident 이력');
    expect(html).toContain('원격 CI 미실행');
    expect(html).toContain('href="https://github.com/owner/repo/pull/6"');
    expect(html).not.toContain('미구현');
  });

  it.each(['incidentUID', 'namespace', 'incidentName', 'workloadUID', 'runtimeRunID', 'epoch', 'policyUID', 'grantSha256'] as const)('does not join another %s', (key) => {
    const wire = fixture(); wire.engineeringRuns[0]![key] = 'different';
    expect(parseSnapshot(wire).recentIncidents[0]?.status.engineering).toBeUndefined();
  });

  it('rejects another incident policy UID even with a matching projection', () => {
    const wire = fixture(); wire.recentIncidents[0]!.spec.policyRef.uid = 'other-policy';
    expect(parseSnapshot(wire).recentIncidents[0]?.status.engineering).toBeUndefined();
  });

  it('rejects duplicate projections, unsafe links and missing hashes', () => {
    const duplicate = fixture(); duplicate.engineeringRuns.push(duplicate.engineeringRuns[0]!);
    expect(parseSnapshot(duplicate).recentIncidents[0]?.status.engineering).toBeUndefined();
    const link = fixture(); link.engineeringRuns[0]!.status.pullRequest.url = 'javascript:alert(1)';
    expect(parseSnapshot(link).recentIncidents[0]?.status.engineering).toBeUndefined();
    const missing = fixture(); missing.engineeringRuns[0]!.status.evidence.prReceiptSha256 = '';
    expect(parseSnapshot(missing).recentIncidents[0]?.status.engineering).toBeUndefined();
  });

  it('rejects a missing Runtime binding despite matching incident UID', () => {
    const wire = fixture();
    Reflect.deleteProperty(wire.recentIncidents[0]!, 'runtimeBinding');
    expect(parseSnapshot(wire).recentIncidents[0]?.status.engineering).toBeUndefined();
  });

  it('drops replay metadata and unverified normalized engineering', () => {
    const wire = fixture();
    const normalized = parseSnapshot(wire);
    expect(normalized.recentIncidents[0]?.status.engineering).toBeDefined();
    const parsed = parseSnapshot({ ...normalized, replay: { title: 'untrusted history' } });
    expect(parsed.replay).toBeUndefined();
    expect(parsed.recentIncidents[0]?.status.engineering).toBeUndefined();
    expect(normalized.recentIncidents[0]?.status.engineering).toBeDefined();
  });

  it('keeps an unjoined failed incident out of the engineering history', () => {
    const wire = fixture(); wire.engineeringRuns = [];
    const snapshot = parseSnapshot(wire);
    expect(engineeringIncident(snapshot)).toBeNull();
    expect(snapshot.activeIncident).toBeNull();
  });
});
