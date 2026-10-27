import type { IncidentEngineeringStatus } from '../../domain/types';
import type { CrdRecoveryIncident } from './crd';

const record = (x: unknown): x is Record<string, unknown> => typeof x === 'object' && x !== null && !Array.isArray(x);
const text = (x: unknown): x is string => typeof x === 'string' && x.length > 0 && x.length <= 512;
const sha = (x: unknown) => typeof x === 'string' && /^[a-f0-9]{64}$/.test(x);

/** The localhost bridge verifies hashes; this joins only its bounded projection,
 * never arbitrary status.engineering from raw CRDs or a name-only match. */
export function engineeringJoin(crd: CrdRecoveryIncident, workload: Record<string, unknown>, runs: unknown): IncidentEngineeringStatus | undefined {
  if (!Array.isArray(runs) || runs.length !== 1) return undefined;
  const run: unknown = runs[0];
  if (!record(run) || !text(crd.metadata.uid) || run.incidentUID !== crd.metadata.uid || run.namespace !== crd.metadata.namespace || run.incidentName !== crd.metadata.name || run.workloadUID !== crd.spec.workload.uid || run.workloadUID !== workload.uid || run.namespace !== workload.namespace) return undefined;
  const binding = crd.runtimeBinding;
  if (!record(binding) || !text(binding.runID) || !text(binding.epoch) || !text(binding.policyUID) || !sha(binding.grantSha256) || binding.runID !== run.runtimeRunID || binding.epoch !== run.epoch || binding.policyUID !== run.policyUID || binding.policyUID !== crd.spec.policyRef?.uid || binding.workloadUID !== run.workloadUID || binding.grantSha256 !== run.grantSha256) return undefined;
  const status = run.status;
  if (!record(status) || status.phase !== 'PR_CREATED' || status.iteration !== 1 || status.maxIterations !== 1 || !text(status.jobId) || !text(status.baseCommit) || !/^[a-f0-9]{40}$/.test(status.baseCommit) || !text(status.branch)) return undefined;
  const pr = status.pullRequest;
  const evidence = status.evidence;
  if (!record(pr) || !Number.isSafeInteger(pr.number) || Number(pr.number) <= 0 || !text(pr.title) || !text(pr.headSha) || !/^[a-f0-9]{40}$/.test(pr.headSha) || !text(pr.url) || !new RegExp(`^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/${pr.number}$`).test(pr.url)) return undefined;
  if (!record(evidence) || !sha(evidence.bundleSha256) || !sha(evidence.resultSha256) || !sha(evidence.prReceiptSha256) || evidence.remoteCI !== 'NOT_RUN' || !text(evidence.model) || !text(evidence.observedAt) || !Number.isFinite(Date.parse(evidence.observedAt)) || evidence.executionMode !== 'DIRECT_MODEL_CODE_PROPOSAL_APPLIED_BY_COORDINATOR') return undefined;
  const stages = status.stages;
  const names = ['build', 'unit', 'regression-fixture', 'independent-review'];
  if (!Array.isArray(stages) || stages.length !== names.length || stages.some((s, i) => !record(s) || s.stage !== names[i] || s.iteration !== 1 || s.outcome !== 'pass' || !text(s.summary))) return undefined;
  // Construct an explicit view; raw reports, logs and arbitrary fields are omitted.
  return {
    phase: 'PR_CREATED', jobId: status.jobId, iteration: 1, maxIterations: 1,
    baseCommit: status.baseCommit, branch: status.branch,
    stages: stages.map((s) => ({ stage: s.stage, iteration: 1, outcome: 'pass', summary: s.summary })),
    pullRequest: { number: Number(pr.number), title: pr.title, headSha: pr.headSha, url: pr.url },
    evidence: { bundleSha256: String(evidence.bundleSha256), resultSha256: String(evidence.resultSha256), prReceiptSha256: String(evidence.prReceiptSha256), remoteCI: 'NOT_RUN', model: evidence.model, observedAt: evidence.observedAt, executionMode: evidence.executionMode, runtimeRunID: binding.runID, epoch: binding.epoch, policyUID: binding.policyUID, grantSha256: String(binding.grantSha256) },
  };
}
