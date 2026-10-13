import { evaluateCandidates, failuresFromAttempts, type EvaluationResult } from '../../domain/evaluate';
import type {
  ActionAttempt,
  ClusterNode,
  DashboardSnapshot,
  EngineeringPhase,
  FailureClass,
  HttpProbe,
  IncidentEngineeringStatus,
  IncidentEventKind,
  IncidentTrigger,
  Narration,
  PodReason,
  PodSummary,
  QuarantineState,
  RecoveryIncident,
  RecoveryPolicy,
  RuntimePhase,
  StageResult,
  WaitingReason,
  WorkloadStatus,
} from '../../domain/types';
import {
  BASE_REQUESTED_BY_NODE,
  DEMO_EPOCH,
  DEMO_NAMESPACE,
  MEM_TOTAL_BY_NODE,
  NODE_SEEDS,
  baselinePolicy,
  baselineWorkload,
  seedNode,
} from './baseline';
import { createRng, hexId } from './rng';

/**
 * Deterministic Demo Mode simulation. A scenario is a list of cues at simulated
 * seconds; between cues the engine integrates metrics, scrapes, HTTP probes and
 * Pod lifecycle. Decisions are computed with domain/evaluate.ts from the simulated
 * metrics, so changing a load cue changes the outcome exactly as the policy says.
 */

export const TICK_MS = 500;
const SCRAPE_EVERY_MS = 10_000; // architecture §11 initial scrape interval assumption
const PROBE_EVERY_MS = 2_000;
const HISTORY_POINTS = 36;
const PROBE_POINTS = 45;

export interface Cue {
  /** Simulated seconds from scenario start. */
  at: number;
  /** Starts a named chapter in the presenter timeline. */
  chapter?: string;
  run: (ctx: ScenarioContext) => void;
}

export interface ScenarioScript {
  seed: number;
  durationSec: number;
  cues: Cue[];
}

interface DemoPod extends PodSummary {
  nodeName: string;
  /** Sim ms when a Pending pod becomes Running. */
  startsAt?: number;
  /** Sim ms when a Terminating pod disappears. */
  removeAt?: number;
}

interface NodeSim {
  power: 'on' | 'off';
  exporterUp: boolean;
  target: { cpu: number; memory: number };
  /** Latest true value before scraping. */
  live: { cpu: number; memory: number };
}

export interface ScenarioContext {
  readonly nowMs: number;
  /** Name of the open RecoveryIncident, e.g. demo-api-recovery-0007. */
  readonly incidentName: string | null;
  narrate(narration: Narration): void;
  /** Demo Driver fault injection (power-off, fixture replay): §18 measurement start. */
  injectFault(): void;
  pace(multiplier: number): void;
  setLoad(nodeName: string, load: { cpu?: number; memory?: number }): void;
  powerOff(nodeName: string): void;
  setExporterUp(nodeName: string, up: boolean): void;
  setReadiness(nodeName: string, readiness: ClusterNode['readiness']): void;
  openIncident(trigger: IncidentTrigger): void;
  phase(phase: RuntimePhase, message: string, kind?: IncidentEventKind): void;
  classify(classification: FailureClass, message: string, signature?: string): void;
  wait(reason: WaitingReason, message: string): void;
  /** Runs the §8 hard filter + score on the current simulated world. */
  evaluate(message?: string): EvaluationResult;
  /** Patches nodeSelector to the decided target and creates the surge Pod. */
  execute(startDelaySec: number): void;
  crash(reason?: PodReason): void;
  /** Marks the current attempt failed and records a workload–node cooldown. */
  failAttempt(reason: string): void;
  recovered(message: string): void;
  fail(message: string): void;
  evidence(bundleRef: string, message: string): void;
  quarantine(state: QuarantineState, message: string): void;
  engineering(patch: Partial<IncidentEngineeringStatus> & { phase: EngineeringPhase }, message: string): void;
  stage(result: StageResult): void;
  condition(type: string, status: 'True' | 'False' | 'Unknown', reason: string, message: string): void;
}

export class DemoSimulation {
  private readonly script: ScenarioScript;
  private rng!: () => number;
  private elapsedMs = 0;
  private cueIndex = 0;
  private paceValue = 1;
  private nodes: ClusterNode[] = [];
  private sim = new Map<string, NodeSim>();
  private demoPods: DemoPod[] = [];
  private workload!: WorkloadStatus;
  private policy!: RecoveryPolicy;
  private probes: HttpProbe[] = [];
  private incident: RecoveryIncident | null = null;
  private narration: Narration | undefined;
  private crashing = false;
  private faultInjectedAt: number | undefined;
  /** Start of the current consecutive-success streak per attempt (stability window). */
  private streakSince = new Map<string, number>();
  private cached: DashboardSnapshot | null = null;
  /** Cue failures in this run; tests assert zero, the booth keeps playing. */
  cueErrors = 0;

  private readonly incidentSequence: number;

  constructor(script: ScenarioScript, options: { incidentSequence?: number } = {}) {
    this.script = script;
    this.incidentSequence = options.incidentSequence ?? 6;
    this.reset();
  }

  get durationMs(): number {
    return this.script.durationSec * 1000;
  }

  get elapsed(): number {
    return this.elapsedMs;
  }

  get pace(): number {
    return this.paceValue;
  }

  reset(): void {
    this.rng = createRng(this.script.seed);
    this.elapsedMs = 0;
    this.cueIndex = 0;
    this.paceValue = 1;
    this.crashing = false;
    this.cueErrors = 0;
    this.faultInjectedAt = undefined;
    this.streakSince = new Map();
    this.incident = null;
    this.narration = undefined;
    this.probes = [];
    this.workload = baselineWorkload();
    this.policy = baselinePolicy();
    this.policy.status.incidentSequence = this.incidentSequence;
    this.nodes = NODE_SEEDS.map((seed) =>
      seedNode(seed, `node-${hexId(this.rng, 8)}`, () => `pod-${hexId(this.rng, 8)}`),
    );
    this.sim = new Map(
      NODE_SEEDS.map((seed) => [
        seed.name,
        { power: 'on', exporterUp: true, target: { ...seed.baseLoad }, live: { ...seed.baseLoad } },
      ]),
    );
    this.demoPods = [
      {
        uid: `pod-${hexId(this.rng, 8)}`,
        name: 'demo-api-7c9d4-x2kq8',
        namespace: DEMO_NAMESPACE,
        role: 'demo-api',
        phase: 'Running',
        ready: true,
        restarts: 0,
        nodeName: 'sarmubuntuserver',
      },
    ];
    // Warm up history so sparklines are not empty on first paint.
    for (let i = HISTORY_POINTS; i > 0; i--) this.scrape(this.nowMs - i * SCRAPE_EVERY_MS);
    this.scrape(this.nowMs);
    for (let i = PROBE_POINTS; i > 0; i--) this.probe(this.nowMs - i * PROBE_EVERY_MS);
    this.fireCues();
    this.cached = null;
  }

  private get nowMs(): number {
    return DEMO_EPOCH + this.elapsedMs;
  }

  /** Advance (or rewind by replaying) to the given simulated time. */
  advanceTo(targetMs: number): void {
    const target = Math.max(0, Math.min(targetMs, this.durationMs));
    if (target < this.elapsedMs) this.reset();
    let changed = false;
    while (this.elapsedMs + TICK_MS <= target) {
      this.elapsedMs += TICK_MS;
      this.fireCues();
      this.step();
      changed = true;
    }
    if (changed) this.cached = null;
  }

  get finished(): boolean {
    return this.elapsedMs >= this.durationMs;
  }

  snapshot(): DashboardSnapshot {
    if (this.cached) return this.cached;
    const now = this.nowMs;
    const nodes: ClusterNode[] = this.nodes.map((node) => {
      const demo = this.demoPods
        .filter((p) => p.nodeName === node.name)
        .map(({ nodeName: _n, startsAt: _s, removeAt: _r, ...pod }) => ({ ...pod }));
      return {
        ...node,
        pressure: [...node.pressure],
        allocatable: { ...node.allocatable },
        requested: { ...node.requested },
        usage: {
          cpu: { ...node.usage.cpu },
          memory: { ...node.usage.memory },
          memAvailableBytes: { ...node.usage.memAvailableBytes },
        },
        usageHistory: [...(node.usageHistory ?? [])],
        pods: [...node.pods.map((p) => ({ ...p })), ...demo],
      };
    });
    this.cached = {
      source: 'demo',
      observedAt: now,
      cluster: { name: 'opsloop-testbed', k3sVersion: 'v1.35.9+k3s1', nodes },
      workload: structuredClone(this.workload),
      http: { endpoint: 'http://opsloop-control:30080/', probes: this.probes.map((p) => ({ ...p })) },
      policy: structuredClone(this.policy),
      activeIncident: this.incident ? structuredClone(this.incident) : null,
      recentIncidents: [],
      ...(this.narration ? { narration: { ...this.narration } } : {}),
    };
    return this.cached;
  }

  /** Chapters declared by cues, for the presenter timeline. */
  chapters(): Array<{ atMs: number; label: string }> {
    return this.script.cues
      .filter((c): c is Cue & { chapter: string } => typeof c.chapter === 'string')
      .map((c) => ({ atMs: c.at * 1000, label: c.chapter }));
  }

  // -------------------------------------------------------------------------
  // Simulation internals
  // -------------------------------------------------------------------------

  private fireCues(): void {
    const cues = this.script.cues;
    while (this.cueIndex < cues.length && cues[this.cueIndex]!.at * 1000 <= this.elapsedMs) {
      const cue = cues[this.cueIndex]!;
      // Advance first so a failing cue is skipped rather than retried every frame.
      this.cueIndex++;
      try {
        cue.run(this.context());
      } catch (err) {
        this.cueErrors += 1;
        console.error(`[OpsLoop demo] cue at ${cue.at}s failed`, err);
      }
    }
  }

  private step(): void {
    const now = this.nowMs;
    for (const node of this.nodes) {
      const s = this.sim.get(node.name)!;
      if (s.power === 'off') continue;
      s.live.cpu = clamp(s.live.cpu + (s.target.cpu - s.live.cpu) * 0.18 + (this.rng() - 0.5) * 0.03, 0.02, 0.99);
      s.live.memory = clamp(
        s.live.memory + (s.target.memory - s.live.memory) * 0.15 + (this.rng() - 0.5) * 0.01,
        0.05,
        0.98,
      );
    }
    if (this.elapsedMs % SCRAPE_EVERY_MS === 0) this.scrape(now);
    this.advancePods(now);
    this.recomputeRequests();
    if (this.elapsedMs % PROBE_EVERY_MS === 0) this.probe(now);
  }

  private scrape(at: number): void {
    for (const node of this.nodes) {
      const s = this.sim.get(node.name)!;
      if (s.power === 'off' || !s.exporterUp) {
        // No new sample: values keep their original timestamp and become stale.
        node.usageHistory = pushBounded(node.usageHistory ?? [], { at, cpu: null, memory: null }, HISTORY_POINTS);
        continue;
      }
      const memTotal = MEM_TOTAL_BY_NODE[node.name] ?? node.allocatable.memoryBytes;
      node.usage = {
        cpu: { value: round(s.live.cpu), observedAt: at },
        memory: { value: round(s.live.memory), observedAt: at },
        memAvailableBytes: { value: Math.round((1 - s.live.memory) * memTotal), observedAt: at },
      };
      node.usageHistory = pushBounded(
        node.usageHistory ?? [],
        { at, cpu: round(s.live.cpu), memory: round(s.live.memory) },
        HISTORY_POINTS,
      );
    }
  }

  private advancePods(now: number): void {
    for (const pod of this.demoPods) {
      if (pod.startsAt !== undefined && now >= pod.startsAt) {
        pod.phase = 'Running';
        pod.ready = true;
        delete pod.reason;
        delete pod.startsAt;
        const attempt = this.currentAttempt();
        if (attempt && attempt.podUid === pod.uid && attempt.result === 'pending') {
          attempt.result = 'placed';
          this.runtime().timings.placedAt ??= now;
          this.pushEvent('act', this.runtime().phase, `새 Pod가 ${pod.nodeName}에서 Running/Ready`);
        }
      }
    }
    this.demoPods = this.demoPods.filter((p) => p.removeAt === undefined || now < p.removeAt);
    // Pods on unreachable nodes lose readiness once the API reports the node as not Ready.
    for (const pod of this.demoPods) {
      const node = this.node(pod.nodeName);
      if (node.readiness !== 'Ready' && pod.phase === 'Running') {
        pod.ready = false;
        pod.reason = 'NodeLost';
      }
    }
  }

  private recomputeRequests(): void {
    for (const node of this.nodes) {
      const base = BASE_REQUESTED_BY_NODE[node.name]!;
      const demo = this.demoPods.filter((p) => p.nodeName === node.name).length;
      node.requested = {
        cpuMillicores: base.cpuMillicores + demo * this.workload.requests.cpuMillicores,
        memoryBytes: base.memoryBytes + demo * this.workload.requests.memoryBytes,
      };
    }
  }

  private probe(at: number): void {
    const serving = this.demoPods.find(
      (p) => p.ready && p.phase === 'Running' && this.sim.get(p.nodeName)?.power === 'on',
    );
    const ok = serving !== undefined && !this.crashing;
    const probe: HttpProbe = { at, ok };
    if (ok && serving) {
      const node = this.node(serving.nodeName);
      probe.servedBy = node.name;
      probe.arch = node.arch;
      probe.latencyMs = Math.round(6 + this.rng() * 9 + (node.site === 'azure-vm' ? 38 : 0));
      const attempt = this.currentAttempt();
      if (attempt && attempt.podUid === serving.uid && attempt.result === 'placed') {
        attempt.httpSuccesses = Math.min(attempt.httpSuccesses + 1, 99);
        if (!this.streakSince.has(attempt.actionId)) this.streakSince.set(attempt.actionId, at);
      }
    } else {
      // "Consecutive" successes: any failed probe during verification resets the count.
      const attempt = this.currentAttempt();
      if (attempt && attempt.result === 'placed') {
        attempt.httpSuccesses = 0;
        this.streakSince.delete(attempt.actionId);
      }
    }
    this.probes = pushBounded(this.probes, probe, PROBE_POINTS);
  }

  private node(name: string): ClusterNode {
    const node = this.nodes.find((n) => n.name === name);
    if (!node) throw new Error(`Unknown demo node: ${name}`);
    return node;
  }

  private runtime() {
    if (!this.incident) throw new Error('Scenario cue requires an open incident');
    return this.incident.status.runtime;
  }

  private currentAttempt(): ActionAttempt | undefined {
    return this.incident?.status.runtime.attempts.at(-1);
  }

  private pushEvent(kind: IncidentEventKind, phase: RuntimePhase | EngineeringPhase, message: string): void {
    if (!this.incident) return;
    this.incident.status.events = pushBounded(
      this.incident.status.events,
      { at: this.nowMs, kind, phase, message },
      40,
    );
  }

  private context(): ScenarioContext {
    const self = this;
    return {
      get nowMs() {
        return self.nowMs;
      },
      get incidentName() {
        return self.incident?.name ?? null;
      },
      narrate(narration) {
        self.narration = narration;
      },
      injectFault() {
        self.faultInjectedAt ??= self.nowMs;
      },
      pace(multiplier) {
        self.paceValue = multiplier;
      },
      setLoad(nodeName, load) {
        const s = self.sim.get(nodeName)!;
        s.target = { cpu: load.cpu ?? s.target.cpu, memory: load.memory ?? s.target.memory };
      },
      powerOff(nodeName) {
        self.sim.get(nodeName)!.power = 'off';
      },
      setExporterUp(nodeName, up) {
        self.sim.get(nodeName)!.exporterUp = up;
      },
      setReadiness(nodeName, readiness) {
        self.node(nodeName).readiness = readiness;
      },
      openIncident(trigger) {
        const seq = self.policy.status.incidentSequence + 1;
        const name = `${self.policy.name}-${String(seq).padStart(4, '0')}`;
        self.policy.status.incidentSequence = seq;
        self.policy.status.activeIncidentRef = name;
        self.incident = {
          name,
          namespace: DEMO_NAMESPACE,
          createdAt: self.nowMs,
          spec: {
            workloadUid: self.workload.uid,
            trigger,
            policyRef: self.policy.name,
            policyHash: self.policy.policyHash,
          },
          status: {
            runtime: {
              phase: 'DETECTED',
              attempts: [],
              budget: { used: 0, max: self.policy.spec.budget.maxActionsPerIncident },
              timings: {
                ...(self.faultInjectedAt !== undefined ? { faultInjectedAt: self.faultInjectedAt } : {}),
                triggeredAt: trigger.observedAt,
                detectedAt: self.nowMs,
              },
              oldPodsOutstanding: false,
            },
            conditions: [],
            events: [],
          },
        };
        const what =
          trigger.type === 'NodeNotReady'
            ? `${trigger.nodeName} NotReady 지속 → Incident ${name} 예약`
            : `${trigger.podName} 재시작 ${trigger.restarts}회 → Incident ${name} 예약`;
        self.pushEvent('trigger', 'DETECTED', what);
        self.cached = null;
      },
      phase(phase, message, kind = 'evaluate') {
        const rt = self.runtime();
        rt.phase = phase;
        // T_decide ends when the first target is actually selected (§18).
        if (phase === 'TARGET_SELECTED' && rt.decision?.selectedHostname) rt.timings.decidedAt ??= self.nowMs;
        self.pushEvent(kind, phase, message);
      },
      classify(classification, message, signature) {
        const rt = self.runtime();
        rt.phase = 'CLASSIFIED';
        rt.classification = classification;
        if (signature) rt.signature = signature;
        self.pushEvent('classify', 'CLASSIFIED', message);
      },
      wait(reason, message) {
        const rt = self.runtime();
        rt.phase = 'WAITING';
        rt.waitingReason = reason;
        self.pushEvent('wait', 'WAITING', message);
      },
      evaluate(message) {
        const rt = self.runtime();
        const trigger = self.incident!.spec.trigger;
        const current = self.demoPods.find((p) => p.phase !== 'Pending')?.nodeName;
        const recentFailures = failuresFromAttempts(rt.attempts);
        const result = evaluateCandidates({
          nodes: self.nodes,
          workload: self.workload,
          policy: self.policy.spec,
          now: self.nowMs,
          ...(trigger.type === 'NodeNotReady' ? { failedNodeName: trigger.nodeName } : {}),
          ...(current ? { currentNodeName: current } : {}),
          cooldowns: self.policy.status.cooldowns,
          recentFailures,
        });
        const actionId = `${self.incident!.name}-a${rt.attempts.length + 1}`;
        rt.decision = {
          incidentId: self.incident!.name,
          actionId,
          policyGeneration: self.policy.generation,
          policyHash: self.policy.policyHash,
          workloadUid: self.workload.uid,
          baselineTemplateHash: 'tmpl-6f2a91',
          imageIndexDigest: self.workload.imageIndexDigest,
          snapshotObservedAt: self.nowMs,
          snapshotExpiresAt: self.nowMs + self.policy.spec.filter.maxSampleAgeSeconds * 1000,
          candidates: result.candidates,
          sourceNodeName: current ?? null,
          selectedNodeUid: result.selected?.nodeUid ?? null,
          selectedHostname: result.selected?.hostname ?? null,
          actionType: result.actionType,
          decisionReason: result.selected
            ? `${result.selected.nodeName} 최고 점수 ${result.selected.score?.total.toFixed(1)}`
            : '적격 후보 없음',
        };
        const eligible = result.candidates.filter((c) => c.score).length;
        self.pushEvent(
          'decide',
          rt.phase,
          message ?? `후보 ${result.candidates.length}개 평가, 적격 ${eligible}개`,
        );
        return result;
      },
      execute(startDelaySec) {
        const rt = self.runtime();
        const decision = rt.decision;
        if (!decision?.selectedHostname || !decision.actionType) throw new Error('execute() needs a selected target');
        const target = self.node(decision.selectedHostname);
        rt.phase = 'EXECUTING';
        rt.budget.used += 1;
        const podUid = `pod-${hexId(self.rng, 8)}`;
        const attempt: ActionAttempt = {
          actionId: decision.actionId,
          type: decision.actionType,
          targetNode: target.name,
          targetArch: target.arch,
          startedAt: self.nowMs,
          result: 'pending',
          podUid,
          httpSuccesses: 0,
        };
        rt.attempts.push(attempt);
        self.workload.nodeSelectorHostname = decision.selectedHostname;
        // Old Pods: on an unreachable node they stay outstanding; on a live node they terminate.
        for (const old of self.demoPods) {
          if (self.sim.get(old.nodeName)?.power === 'off') {
            rt.oldPodsOutstanding = true;
          } else {
            old.phase = 'Running';
            old.ready = false;
            old.reason = 'Terminating';
            old.removeAt = self.nowMs + 4000;
          }
        }
        self.demoPods.push({
          uid: podUid,
          name: `demo-api-${hexId(self.rng, 5)}-${hexId(self.rng, 5)}`,
          namespace: DEMO_NAMESPACE,
          role: 'demo-api',
          phase: 'Pending',
          ready: false,
          restarts: 0,
          reason: 'ContainerCreating',
          attemptId: decision.actionId,
          nodeName: target.name,
          startsAt: self.nowMs + startDelaySec * 1000,
        });
        self.crashing = false;
        self.pushEvent(
          'act',
          'EXECUTING',
          `nodeSelector → ${decision.selectedHostname} (${decision.actionType}, ${target.arch}), attempt ${decision.actionId}`,
        );
      },
      crash(reason = 'CrashLoopBackOff') {
        self.crashing = true;
        for (const pod of self.demoPods) {
          if (pod.phase === 'Running' && pod.reason !== 'Terminating') {
            pod.restarts += 1;
            pod.ready = false;
            pod.reason = reason;
          }
        }
      },
      failAttempt(reason) {
        const rt = self.runtime();
        const attempt = rt.attempts.at(-1);
        if (!attempt) return;
        attempt.result = 'failed';
        attempt.finishedAt = self.nowMs;
        attempt.failureReason = reason;
        self.policy.status.cooldowns.push({
          workloadUid: self.workload.uid,
          nodeName: attempt.targetNode,
          until: self.nowMs + self.policy.spec.budget.cooldownSeconds * 1000,
        });
        self.pushEvent('verify', 'VERIFYING', `attempt ${attempt.actionId} 실패: ${reason}`);
      },
      recovered(message) {
        const rt = self.runtime();
        const attempt = rt.attempts.at(-1);
        // A script may only declare recovery the policy would accept (§7, §8).
        const required = self.policy.spec.verification.consecutiveSuccesses;
        const stableSince = attempt ? self.streakSince.get(attempt.actionId) : undefined;
        const stabilityMs = self.policy.spec.verification.stabilityWindowSeconds * 1000;
        if (!attempt || attempt.httpSuccesses < required || stableSince === undefined || self.nowMs - stableSince < stabilityMs) {
          throw new Error(`recovered() before verification passed (${attempt?.httpSuccesses ?? 0}/${required})`);
        }
        if (attempt) {
          attempt.result = 'verified';
          attempt.finishedAt = self.nowMs;
        }
        rt.phase = 'RECOVERED';
        rt.timings.recoveredAt = self.nowMs;
        self.policy.status.activeIncidentRef = null;
        self.pushEvent('recovered', 'RECOVERED', message);
      },
      fail(message) {
        self.runtime().phase = 'FAILED';
        self.policy.status.activeIncidentRef = null;
        self.pushEvent('fail', 'FAILED', message);
      },
      evidence(bundleRef, message) {
        const rt = self.runtime();
        rt.phase = 'EVIDENCE_FINALIZING';
        rt.bundleRef = bundleRef;
        self.pushEvent('evidence', 'EVIDENCE_FINALIZING', message);
      },
      quarantine(state, message) {
        const rt = self.runtime();
        rt.quarantine = state;
        if (state === 'QuarantineRequested') {
          self.workload.replicas = 0;
          for (const pod of self.demoPods) {
            pod.reason = 'Terminating';
            pod.ready = false;
            pod.removeAt = self.nowMs + 3000;
          }
        }
        self.pushEvent('quarantine', rt.phase, message);
      },
      engineering(patch, message) {
        const inc = self.incident!;
        const prev = inc.status.engineering;
        inc.status.engineering = {
          jobId: prev?.jobId ?? `job-${inc.name.split('-').at(-1)}`,
          iteration: prev?.iteration ?? 0,
          maxIterations: prev?.maxIterations ?? 3,
          baseCommit: prev?.baseCommit ?? 'demo-base',
          stages: prev?.stages ?? [],
          ...prev,
          ...patch,
        };
        self.pushEvent('engineering', patch.phase, message);
      },
      stage(result) {
        const eng = self.incident!.status.engineering;
        if (!eng) return;
        const idx = eng.stages.findIndex((s) => s.stage === result.stage && s.iteration === result.iteration);
        if (idx >= 0) eng.stages[idx] = result;
        else eng.stages.push(result);
      },
      condition(type, status, reason, message) {
        const conds = self.incident!.status.conditions;
        const next = { type, status, reason, message, lastTransitionTime: self.nowMs };
        const idx = conds.findIndex((c) => c.type === type);
        if (idx >= 0) conds[idx] = next;
        else conds.push(next);
      },
    };
  }
}

function clamp(x: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, x));
}

function round(x: number): number {
  return Math.round(x * 1000) / 1000;
}

function pushBounded<T>(list: T[], item: T, max: number): T[] {
  const next = [...list, item];
  return next.length > max ? next.slice(next.length - max) : next;
}
