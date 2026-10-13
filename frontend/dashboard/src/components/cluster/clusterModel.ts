import { REJECT_REASON_LABEL, SITE_LABEL } from '../../domain/labels';
import type {
  ActionType,
  Arch,
  ClusterNode,
  DashboardSnapshot,
  HttpAvailability,
  HttpProbe,
  MetricSample,
  NodeSite,
  PodRole,
  PodSummary,
  PressureCondition,
  QuarantineState,
  RecoveryDecision,
  RuntimePhase,
} from '../../domain/types';
import { percent } from '../shared/format';
import type { LampState } from '../shared/primitives';

/**
 * Pure view-model for the cluster mimic. Everything the hero shows is derived
 * here from one DashboardSnapshot so the components stay thin and the rules
 * (freshness, route endpoints, availability) are unit-tested.
 */

// ---------------------------------------------------------------------------
// Node order and identity
// ---------------------------------------------------------------------------

const SITE_ORDER: Record<NodeSite, number> = { 'laptop-vm': 0, 'physical-server': 1, 'azure-vm': 2 };

/** Station order on the mimic: by site, control plane first, then name. Never by hard-coded node name. */
export function orderNodes(nodes: readonly ClusterNode[]): ClusterNode[] {
  return [...nodes].sort(
    (a, b) =>
      (SITE_ORDER[a.site] ?? 9) - (SITE_ORDER[b.site] ?? 9) ||
      (a.role === 'control-plane' ? 0 : 1) - (b.role === 'control-plane' ? 0 : 1) ||
      a.name.localeCompare(b.name),
  );
}

/** Contract values may name a node by name, hostname label (§10) or uid. */
export function findNode(nodes: readonly ClusterNode[], key: string | null | undefined): ClusterNode | undefined {
  if (!key) return undefined;
  return nodes.find((n) => n.name === key) ?? nodes.find((n) => n.hostnameLabel === key || n.uid === key);
}

export const ROLE_LABEL: Record<ClusterNode['role'], string> = {
  'control-plane': 'Control plane',
  worker: 'Worker',
};

export interface ReadinessView {
  tone: LampState;
  label: string;
  code?: string;
}

export function readinessView(node: ClusterNode): ReadinessView {
  if (node.readiness === 'Ready') return { tone: 'ok', label: 'Ready' };
  return { tone: 'fault', label: '응답 없음', code: node.readiness === 'Unknown' ? 'Ready=Unknown' : 'NotReady' };
}

const PRESSURE_LABEL: Record<PressureCondition, string> = {
  MemoryPressure: REJECT_REASON_LABEL.MEMORY_PRESSURE.short,
  DiskPressure: REJECT_REASON_LABEL.DISK_PRESSURE.short,
  PIDPressure: REJECT_REASON_LABEL.PID_PRESSURE.short,
};

/** Kubernetes conditions that make a node a poor home, shown as caution flags. */
export function nodeFlags(node: ClusterNode): string[] {
  const flags = node.pressure.map((p) => PRESSURE_LABEL[p]);
  if (node.unschedulable) flags.unshift('배치 금지(cordon)');
  return flags;
}

// ---------------------------------------------------------------------------
// Metrics: freshness and thresholds (architecture §11)
// ---------------------------------------------------------------------------

export type SampleStatus =
  | { kind: 'fresh'; value: number; ageSec: number }
  | { kind: 'stale'; value: number; ageSec: number }
  | { kind: 'missing' };

/** Missing/NaN is never 0%; an old sample keeps its value but is not used for decisions. */
export function sampleStatus(sample: MetricSample, now: number, maxAgeSec: number): SampleStatus {
  if (sample.value === null || Number.isNaN(sample.value)) return { kind: 'missing' };
  const ageSec = Math.max(0, (now - sample.observedAt) / 1000);
  return ageSec > maxAgeSec ? { kind: 'stale', value: sample.value, ageSec } : { kind: 'fresh', value: sample.value, ageSec };
}

export interface MetricNotice {
  kind: 'stale' | 'missing';
  /** Always shown. */
  lead: string;
  /** Dropped first when the recorder is small. */
  rest: string | null;
  text: string;
}

/** One caption per node: missing beats stale, the oldest sample decides the age. */
export function metricNotice(statuses: SampleStatus[]): MetricNotice | null {
  if (statuses.some((s) => s.kind === 'missing')) {
    return { kind: 'missing', lead: '관측값 없음', rest: null, text: '관측값 없음' };
  }
  const stale = statuses.filter((s): s is Extract<SampleStatus, { kind: 'stale' }> => s.kind === 'stale');
  if (stale.length === 0) return null;
  const age = Math.round(Math.max(...stale.map((s) => s.ageSec)));
  const lead = `관측값 ${age}초 전`;
  const rest = '판단에 쓰지 않음';
  return { kind: 'stale', lead, rest, text: `${lead} · ${rest}` };
}

export type GaugeLevel = 'normal' | 'over' | 'stale' | 'missing';

export interface GaugeView {
  label: string;
  value: number | null;
  level: GaugeLevel;
  threshold: number;
  /** requested / allocatable from the Kubernetes API. */
  requested: number | null;
  history: Array<number | null>;
}

function gauge(
  label: string,
  status: SampleStatus,
  threshold: number,
  requested: number | null,
  history: Array<number | null>,
): GaugeView {
  if (status.kind === 'missing') return { label, value: null, level: 'missing', threshold, requested, history };
  const level: GaugeLevel = status.kind === 'stale' ? 'stale' : status.value > threshold ? 'over' : 'normal';
  return { label, value: status.value, level, threshold, requested, history };
}

function ratio(part: number, whole: number): number | null {
  return whole > 0 ? part / whole : null;
}

// ---------------------------------------------------------------------------
// Pods
// ---------------------------------------------------------------------------

export interface PodView {
  uid: string;
  name: string;
  tone: LampState;
  label: string;
  code?: string;
  /** Terminating pods fade; NodeLost pods are ghosts on a dead node. */
  presence: 'solid' | 'fading' | 'ghost';
  attemptId?: string;
}

export function demoPodView(pod: PodSummary, oldPodsOutstanding: boolean): PodView {
  const base = { uid: pod.uid, name: pod.name, ...(pod.attemptId ? { attemptId: pod.attemptId } : {}) };
  switch (pod.reason) {
    case 'NodeLost':
      return oldPodsOutstanding
        ? { ...base, tone: 'off', label: '이전 Pod 추적 중', code: 'oldPodsOutstanding', presence: 'ghost' }
        : { ...base, tone: 'off', label: '노드 연결 끊김', code: 'NodeLost', presence: 'ghost' };
    case 'Terminating':
      return { ...base, tone: 'off', label: '종료 중', code: 'Terminating', presence: 'fading' };
    case 'CrashLoopBackOff':
      return { ...base, tone: 'fault', label: `반복 종료, 재시작 ${pod.restarts}회`, code: 'CrashLoopBackOff', presence: 'solid' };
    case 'Error':
      return { ...base, tone: 'fault', label: `오류 종료, 재시작 ${pod.restarts}회`, code: 'Error', presence: 'solid' };
    case 'ImagePullBackOff':
      return { ...base, tone: 'fault', label: '이미지 받기 실패', code: 'ImagePullBackOff', presence: 'solid' };
    case 'ContainerCreating':
      return { ...base, tone: 'caution', label: '생성 중', code: 'ContainerCreating', presence: 'solid' };
    default:
      break;
  }
  switch (pod.phase) {
    case 'Pending':
      return { ...base, tone: 'caution', label: '생성 중', code: 'Pending', presence: 'solid' };
    case 'Running':
      return pod.ready
        ? { ...base, tone: 'ok', label: '실행 중', presence: 'solid' }
        : { ...base, tone: 'caution', label: '준비 안 됨', code: 'NotReady', presence: 'solid' };
    case 'Failed':
      return { ...base, tone: 'fault', label: '실패', code: 'Failed', presence: 'solid' };
    case 'Succeeded':
      return { ...base, tone: 'off', label: '종료됨', code: 'Succeeded', presence: 'fading' };
    default:
      return { ...base, tone: 'caution', label: '상태 모름', code: 'Unknown', presence: 'solid' };
  }
}

export type QuietRole = Exclude<PodRole, 'demo-api'>;

export const QUIET_ROLE_LABEL: Record<QuietRole, string> = {
  system: '시스템',
  observability: '관측',
  opsloop: 'OpsLoop',
};

const QUIET_ORDER: QuietRole[] = ['system', 'observability', 'opsloop'];

export interface QuietPod {
  uid: string;
  name: string;
  tone: 'neutral' | 'caution' | 'fault';
}

export interface QuietGroup {
  role: QuietRole;
  label: string;
  pods: QuietPod[];
}

function quietTone(pod: PodSummary): QuietPod['tone'] {
  if (pod.phase === 'Running' && pod.ready) return 'neutral';
  if (pod.phase === 'Failed' || pod.reason === 'CrashLoopBackOff' || pod.reason === 'Error' || pod.reason === 'ImagePullBackOff') {
    return 'fault';
  }
  return 'caution';
}

export function quietGroups(pods: readonly PodSummary[]): QuietGroup[] {
  return QUIET_ORDER.map((role) => ({
    role,
    label: QUIET_ROLE_LABEL[role],
    pods: pods.filter((p) => p.role === role).map((p) => ({ uid: p.uid, name: p.name, tone: quietTone(p) })),
  })).filter((g) => g.pods.length > 0);
}

// ---------------------------------------------------------------------------
// Recovery route (architecture §7, §10)
// ---------------------------------------------------------------------------

export type RouteStage = 'selected' | 'executing' | 'verifying' | 'verified';

const ROUTE_STAGE: Partial<Record<RuntimePhase, RouteStage>> = {
  TARGET_SELECTED: 'selected',
  EXECUTING: 'executing',
  VERIFYING: 'verifying',
  RECOVERED: 'verified',
};

export interface RouteView {
  /** Identifies one action of one incident; changes when a new action starts. */
  key: string;
  /** node.name of the node the workload leaves; null when it cannot be told from the snapshot. */
  fromName: string | null;
  toName: string;
  actionType: ActionType;
  stage: RouteStage;
  actionId: string;
  httpSuccesses: number | null;
  requiredSuccesses: number;
}

/**
 * The node the workload leaves. The decision records it (sourceNodeName); when a
 * source leaves it null, fall back to the previous attempt's node, the node that
 * failed (NodeNotReady), or the node where the crashing Pod ran.
 */
function routeOrigin(snapshot: DashboardSnapshot, decision: RecoveryDecision): ClusterNode | undefined {
  const incident = snapshot.activeIncident;
  if (!incident) return undefined;
  const nodes = snapshot.cluster.nodes;
  const recorded = findNode(nodes, decision.sourceNodeName);
  if (recorded) return recorded;

  const currentActionId = decision.actionId;
  const { attempts } = incident.status.runtime;
  const index = attempts.findIndex((a) => a.actionId === currentActionId);
  const previous = index >= 0 ? attempts[index - 1] : attempts.at(-1);
  if (previous) return findNode(nodes, previous.targetNode);

  const trigger = incident.spec.trigger;
  if (trigger.type === 'NodeNotReady') return findNode(nodes, trigger.nodeName);
  const byPod = nodes.find((n) => n.pods.some((p) => p.name === trigger.podName));
  if (byPod) return byPod;
  return nodes.find((n) => n.pods.some((p) => p.role === 'demo-api' && p.attemptId !== currentActionId));
}

export function deriveRoute(snapshot: DashboardSnapshot): RouteView | null {
  const incident = snapshot.activeIncident;
  const runtime = incident?.status.runtime;
  if (!incident || !runtime) return null;
  const stage = ROUTE_STAGE[runtime.phase];
  const decision = runtime.decision;
  if (!stage || !decision?.selectedHostname) return null;

  const nodes = snapshot.cluster.nodes;
  const to = findNode(nodes, decision.selectedHostname) ?? findNode(nodes, decision.selectedNodeUid);
  if (!to) return null;

  const key = `${incident.name}@${incident.createdAt}/${decision.actionId}`;
  const attempt = runtime.attempts.find((a) => a.actionId === decision.actionId);
  const origin = routeOrigin(snapshot, decision);
  const actionType: ActionType =
    decision.actionType ?? attempt?.type ?? (origin && origin.name === to.name ? 'restart' : 'relocation');
  const fromName = actionType === 'restart' ? to.name : (origin?.name ?? null);

  return {
    key,
    fromName,
    toName: to.name,
    actionType,
    stage,
    actionId: decision.actionId,
    httpSuccesses: attempt ? attempt.httpSuccesses : null,
    requiredSuccesses: snapshot.policy.spec.verification.consecutiveSuccesses,
  };
}

export const ACTION_VERB: Record<ActionType, string> = { relocation: '이동', restart: '재시작' };

export function routeStageText(route: RouteView): string {
  switch (route.stage) {
    case 'selected':
      return '대상 선택';
    case 'executing':
      return '배치 변경 중';
    case 'verifying':
      return `HTTP 검증 ${Math.min(route.httpSuccesses ?? 0, route.requiredSuccesses)}/${route.requiredSuccesses}`;
    case 'verified':
      return '검증 완료';
  }
}

/** Latin letters and digits whose Korean reading ends in a final consonant other than ㄹ. */
const CLOSED_LATIN = new Set(['M', 'N', '0', '3', '6']);
const RIEUL_LATIN = new Set(['L', 'R', '1', '7', '8']);

/** Appends the directional particle: "물리 서버로", "노트북 VM으로", "서울로". */
export function withRo(word: string): string {
  const last = word.trim().at(-1) ?? '';
  const code = last.charCodeAt(0);
  if (code >= 0xac00 && code <= 0xd7a3) {
    const final = (code - 0xac00) % 28;
    return `${word}${final === 0 || final === 8 ? '로' : '으로'}`;
  }
  const upper = last.toUpperCase();
  if (RIEUL_LATIN.has(upper)) return `${word}로`;
  return `${word}${CLOSED_LATIN.has(upper) ? '으로' : '로'}`;
}

/** "물리 서버에서 노트북 VM으로 이동" — used where the drawn route is not shown (mobile, screen readers). */
export function routeSentence(route: RouteView, nodes: readonly ClusterNode[]): string {
  const name = (n: string | null) => (n ? (findNode(nodes, n)?.displayName ?? n) : null);
  const to = name(route.toName) ?? route.toName;
  if (route.actionType === 'restart') return `${to}에서 다시 시작`;
  const from = name(route.fromName);
  return from ? `${from}에서 ${withRo(to)} 이동` : `${withRo(to)} 이동`;
}

// ---------------------------------------------------------------------------
// Workload and HTTP
// ---------------------------------------------------------------------------

/**
 * Architecture answering right now. The HTTP probe is the authority (§11): if the
 * last request failed nobody is serving, even while Kubernetes still says Ready.
 * Without probe metadata, fall back to the Ready demo Pod's node.
 */
export function servingArch(snapshot: DashboardSnapshot): Arch | null {
  const last = snapshot.http.probes.at(-1);
  if (last && !last.ok) return null;
  if (last?.arch) return last.arch;
  for (const node of snapshot.cluster.nodes) {
    if (node.readiness !== 'Ready') continue;
    if (node.pods.some((p) => p.role === 'demo-api' && p.phase === 'Running' && p.ready && !p.reason)) return node.arch;
  }
  return null;
}

export interface QuarantineView {
  state: QuarantineState;
  label: string;
  tone: LampState;
}

export function quarantineView(snapshot: DashboardSnapshot): QuarantineView | null {
  const state = snapshot.activeIncident?.status.runtime.quarantine;
  if (!state) return null;
  const replicas = `replicas=${snapshot.workload.replicas}`;
  switch (state) {
    case 'QuarantineRequested':
      return { state, label: `격리 요청 · ${replicas}`, tone: 'act' };
    case 'QuarantineConfirmed':
      return { state, label: `격리됨 · ${replicas}`, tone: 'act' };
    case 'QuarantineUnconfirmed':
      return { state, label: `격리 미확인 · ${replicas}`, tone: 'caution' };
  }
}

export interface HttpView {
  endpoint: string;
  probes: HttpProbe[];
  okCount: number;
  total: number;
  availability: number | null;
  /** Seconds covered by the probe window, oldest probe to snapshot time. */
  windowSec: number | null;
  last: HttpProbe | null;
  lastLatencyMs: number | null;
  /** Seconds since the current run of failures began; null while answering. */
  downForSec: number | null;
  /** True when every probe in the window failed, so the outage is at least windowSec long. */
  downWholeWindow: boolean;
}

export function httpView(http: HttpAvailability, now: number, slots: number): HttpView {
  const probes = http.probes.slice(-slots);
  const total = probes.length;
  const okCount = probes.reduce((n, p) => n + (p.ok ? 1 : 0), 0);
  const first = probes[0];
  const last = probes.at(-1) ?? null;
  let downForSec: number | null = null;
  let downWholeWindow = false;
  if (last && !last.ok) {
    let i = probes.length - 1;
    while (i > 0 && !probes[i - 1]!.ok) i--;
    downForSec = Math.max(0, Math.round((now - probes[i]!.at) / 1000));
    downWholeWindow = i === 0;
  }
  return {
    endpoint: http.endpoint,
    probes,
    okCount,
    total,
    availability: total > 0 ? okCount / total : null,
    windowSec: first ? Math.max(0, Math.round((now - first.at) / 1000)) : null,
    last,
    lastLatencyMs: last?.ok && last.latencyMs !== undefined ? last.latencyMs : null,
    downForSec,
    downWholeWindow,
  };
}

// ---------------------------------------------------------------------------
// Whole view
// ---------------------------------------------------------------------------

export interface StationView {
  node: ClusterNode;
  siteLabel: string;
  roleLabel: string;
  readiness: ReadinessView;
  flags: string[];
  gauges: [GaugeView, GaugeView];
  notice: MetricNotice | null;
  demoPods: PodView[];
  quiet: QuietGroup[];
  /** Part this node plays in the current recovery route. */
  routeRole: 'origin' | 'target' | 'loop' | null;
}

export interface ClusterView {
  stations: StationView[];
  route: RouteView | null;
  routeText: string | null;
  http: HttpView;
  servingArch: Arch | null;
  quarantine: QuarantineView | null;
  summary: string;
}

export const PROBE_SLOTS = 45;

export function buildClusterView(snapshot: DashboardSnapshot): ClusterView {
  const now = snapshot.observedAt;
  const { filter } = snapshot.policy.spec;
  const oldPodsOutstanding = snapshot.activeIncident?.status.runtime.oldPodsOutstanding ?? false;
  const route = deriveRoute(snapshot);

  const stations = orderNodes(snapshot.cluster.nodes).map((node): StationView => {
    const cpu = sampleStatus(node.usage.cpu, now, filter.maxSampleAgeSeconds);
    const mem = sampleStatus(node.usage.memory, now, filter.maxSampleAgeSeconds);
    const history = node.usageHistory ?? [];
    const routeRole: StationView['routeRole'] = !route
      ? null
      : route.fromName === node.name && route.toName === node.name
        ? 'loop'
        : route.toName === node.name
          ? 'target'
          : route.fromName === node.name
            ? 'origin'
            : null;
    return {
      node,
      siteLabel: SITE_LABEL[node.site] ?? node.site,
      roleLabel: ROLE_LABEL[node.role] ?? node.role,
      readiness: readinessView(node),
      flags: nodeFlags(node),
      gauges: [
        gauge(
          'CPU',
          cpu,
          filter.maxCPUUtilization,
          ratio(node.requested.cpuMillicores, node.allocatable.cpuMillicores),
          history.map((p) => p.cpu),
        ),
        gauge(
          '메모리',
          mem,
          filter.maxMemoryUtilization,
          ratio(node.requested.memoryBytes, node.allocatable.memoryBytes),
          history.map((p) => p.memory),
        ),
      ],
      notice: metricNotice([cpu, mem]),
      demoPods: node.pods.filter((p) => p.role === 'demo-api').map((p) => demoPodView(p, oldPodsOutstanding)),
      quiet: quietGroups(node.pods.filter((p) => p.role !== 'demo-api')),
      routeRole,
    };
  });

  const http = httpView(snapshot.http, now, PROBE_SLOTS);
  const routeText = route ? `${routeSentence(route, snapshot.cluster.nodes)} · ${routeStageText(route)}` : null;
  const view: Omit<ClusterView, 'summary'> = {
    stations,
    route,
    routeText,
    http,
    servingArch: servingArch(snapshot),
    quarantine: quarantineView(snapshot),
  };
  return { ...view, summary: describeCluster(view) };
}

/** Screen-reader summary of the mimic; the drawn diagram is not the only carrier. */
export function describeCluster(view: Omit<ClusterView, 'summary'>): string {
  const parts: string[] = [`클러스터 노드 ${view.stations.length}개.`];
  for (const s of view.stations) {
    const metrics = s.notice
      ? s.notice.text
      : s.gauges.map((g) => `${g.label} ${percent(g.value)}${g.level === 'over' ? ' 상한 초과' : ''}`).join(', ');
    const pods = s.demoPods.map((p) => `Demo API Pod ${p.label}`).join(', ');
    parts.push(
      `${s.node.displayName}(${s.node.arch.toUpperCase()}, ${s.roleLabel}): ${s.readiness.label}, ${metrics}${pods ? `, ${pods}` : ''}.`,
    );
  }
  if (view.routeText) parts.push(`복구 경로: ${view.routeText}.`);
  if (view.quarantine) parts.push(`Deployment ${view.quarantine.label}.`);
  const h = view.http;
  if (h.total > 0) {
    parts.push(
      `HTTP 최근 ${h.windowSec}초 성공률 ${percent(h.availability)}, 마지막 요청 ${h.last?.ok ? '성공' : '실패'}.`,
    );
  }
  return parts.join(' ');
}
