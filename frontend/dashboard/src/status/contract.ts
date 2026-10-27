import schema from '../../../../shared/public-status.schema.json';

export interface Sample { value: number | null; observedAt: number | null }
export interface Source { state: 'ok' | 'partial' | 'unavailable'; observedAt: number | null; errorCode: string | null }
export interface PublicNode { id: 'control' | 'physical' | 'cloud'; site: string; os: 'linux' | null; arch: 'amd64' | 'arm64' | null; readiness: 'Ready' | 'NotReady' | 'Unknown' | null; readyObservedAt: number | null; cpu: Sample; memory: Sample; exporter: Sample }
export interface PublicWorkload { id: 'controller' | 'demo-api'; desired: number | null; ready: number | null; observedAt: number | null }
export interface PublicStatus { schemaVersion: 1; source: 'observed'; collectedAt: number; sources: {kubernetes: Source; prometheus: Source}; nodes: PublicNode[]; workloads: PublicWorkload[]; policy: {mode: 'ObserveOnly' | 'Enforce' | null; enabled: boolean | null; monitoring: boolean | null; incidentCount: number | null; activeIncidentCount: number | null; observedAt: number | null} }
export type DataState = 'current' | 'partial' | 'stale' | 'unavailable';
export interface StatusEnvelope {schemaVersion: 1; serverTime: number; receivedAt: number; dataState: DataState; health: 'healthy' | 'degraded' | 'unknown'; snapshot: PublicStatus}

/** Bounded JSON parser, including duplicate-key rejection before JSON.parse loses keys. */
export function parseStrictJSON(text: string): unknown {
  if (new TextEncoder().encode(text).length > 16384) throw Error('BODY_LIMIT');
  let i = 0;
  const ws = () => { while (/[ \t\r\n]/.test(text[i] ?? '') && i < text.length) i++; };
  function string(): string {
    const start = i++;
    while (i < text.length) { const c = text[i++]; if (c === '\\') i++; else if (c === '"') return JSON.parse(text.slice(start, i)) as string; }
    throw Error('INVALID_JSON');
  }
  function value(depth: number): unknown {
    if (depth > 24) throw Error('JSON_DEPTH');
    ws(); const c = text[i];
    if (c === '"') return string();
    if (c === '{') {
      i++; ws(); const out = Object.create(null) as Record<string, unknown>; const seen = new Set<string>();
      if (text[i] === '}') { i++; return out; }
      for (;;) {
        ws(); if (text[i] !== '"') throw Error('INVALID_JSON');
        const key = string(); if (seen.has(key)) throw Error('DUPLICATE_KEY'); seen.add(key);
        ws(); if (text[i++] !== ':') throw Error('INVALID_JSON'); out[key] = value(depth + 1); ws();
        const end = text[i++]; if (end === '}') return out; if (end !== ',') throw Error('INVALID_JSON');
      }
    }
    if (c === '[') {
      i++; ws(); const out: unknown[] = []; if (text[i] === ']') { i++; return out; }
      for (;;) { out.push(value(depth + 1)); ws(); const end = text[i++]; if (end === ']') return out; if (end !== ',') throw Error('INVALID_JSON'); }
    }
    const literal = /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(text.slice(i))?.[0];
    if (!literal) throw Error('INVALID_JSON'); i += literal.length; const out: unknown = JSON.parse(literal);
    if (typeof out === 'number' && !Number.isFinite(out)) throw Error('NONFINITE'); return out;
  }
  const out = value(0); ws(); if (i !== text.length) throw Error('INVALID_JSON'); return out;
}

type Rule = {type?: string; properties?: Record<string, Rule>; required?: string[]; additionalProperties?: boolean; items?: Rule; minItems?: number; maxItems?: number; minimum?: number; maximum?: number; enum?: unknown[]; const?: unknown; anyOf?: Rule[]};
function assertSupportedRule(r:Rule):void {
  const allowed=['$schema','type','properties','required','additionalProperties','items','minItems','maxItems','minimum','maximum','enum','const','anyOf'];
  if(Object.keys(r).some(k=>!allowed.includes(k)) || r.type!==undefined&&!['object','array','null','boolean','integer','number'].includes(r.type) || !r.type&&!r.anyOf&&!r.enum&&!('const' in r))throw Error('UNSUPPORTED_PUBLIC_SCHEMA');
  if(r.type==='object'&&(r.additionalProperties!==false||!r.properties||!r.required||Object.keys(r.properties).sort().join()!==[...r.required].sort().join()))throw Error('OPEN_PUBLIC_SCHEMA');
  if(r.type==='array'&&(!r.items||!Number.isSafeInteger(r.minItems)||!Number.isSafeInteger(r.maxItems)))throw Error('UNBOUNDED_PUBLIC_SCHEMA');
  for(const child of [...Object.values(r.properties??{}),...(r.anyOf??[]),...(r.items?[r.items]:[])])assertSupportedRule(child);
}
assertSupportedRule(schema as Rule);
/** Apply the closed, checked-in schema keyword subset. Never a permissive fallback. */
function check(v: unknown, r: Rule): void {
  if (r.anyOf) { for (const a of r.anyOf) { try { check(v, a); return; } catch { /* next union branch */ } } throw Error('SCHEMA_UNION'); }
  if ('const' in r && v !== r.const) throw Error('SCHEMA_CONST');
  if (r.enum && !r.enum.includes(v)) throw Error('SCHEMA_ENUM');
  if (r.type === 'null' && v !== null || r.type === 'boolean' && typeof v !== 'boolean') throw Error('SCHEMA_TYPE');
  if (r.type === 'integer' && !Number.isSafeInteger(v) || r.type === 'number' && (typeof v !== 'number' || !Number.isFinite(v))) throw Error('SCHEMA_NUMBER');
  if (typeof v === 'number' && (r.minimum !== undefined && v < r.minimum || r.maximum !== undefined && v > r.maximum)) throw Error('SCHEMA_RANGE');
  if (r.type === 'array') {
    if (!Array.isArray(v) || v.length < r.minItems! || v.length > r.maxItems!) throw Error('SCHEMA_ARRAY');
    for (const x of v) check(x, r.items!);
  }
  if (r.type === 'object') {
    if (v === null || typeof v !== 'object' || Array.isArray(v)) throw Error('SCHEMA_OBJECT');
    const o = v as Record<string, unknown>; const keys = Object.keys(o);
    if (keys.length !== r.required!.length || keys.some(k => !Object.hasOwn(r.properties!, k)) || r.required!.some(k => !Object.hasOwn(o, k))) throw Error('SCHEMA_FIELDS');
    for (const k of keys) check(o[k], r.properties![k]!);
  }
}
export function parseStatus(v: unknown): PublicStatus {
  check(v, schema as Rule); const s = v as PublicStatus;
  const stamp = (t: number | null) => { if (t !== null && t > s.collectedAt + 5000) throw Error('FUTURE_SAMPLE'); };
  for (const source of Object.values(s.sources)) {
    stamp(source.observedAt);
    if (source.state === 'ok' && (source.observedAt === null || source.errorCode !== null) || source.state !== 'ok' && source.errorCode === null || source.state === 'unavailable' && source.observedAt !== null) throw Error('SOURCE_STATE');
  }
  const sites = {control:'laptop-vm', physical:'physical-server', cloud:'azure-vm'};
  if (new Set(s.nodes.map(n => n.id)).size !== 3 || new Set(s.workloads.map(w => w.id)).size !== 2) throw Error('DUPLICATE_ALIAS');
  for (const n of s.nodes) {
    if (sites[n.id] !== n.site) throw Error('SITE'); stamp(n.readyObservedAt);
    if ((n.readiness === null) !== (n.readyObservedAt === null)) throw Error('CONDITION_TIME');
    for (const sample of [n.cpu,n.memory,n.exporter]) { stamp(sample.observedAt); if ((sample.value === null) !== (sample.observedAt === null)) throw Error('SAMPLE_TIME'); }
  }
  for (const w of s.workloads) { stamp(w.observedAt); if ((w.observedAt === null) !== (w.ready === null && w.desired === null)) throw Error('WORKLOAD_TIME'); }
  const p = s.policy; stamp(p.observedAt);
  if (p.activeIncidentCount !== null && p.incidentCount !== null && p.activeIncidentCount > p.incidentCount) throw Error('INCIDENT_COUNT');
  if (p.observedAt === null && Object.entries(p).some(([k,x]) => k !== 'observedAt' && x !== null)) throw Error('POLICY_TIME');
  if (s.sources.kubernetes.state === 'unavailable' && (s.nodes.some(n => [n.os,n.arch,n.readiness,n.readyObservedAt].some(x => x !== null)) || s.workloads.some(w => w.observedAt !== null) || p.observedAt !== null)) throw Error('KUBERNETES_FAILURE');
  if (s.sources.prometheus.state === 'unavailable' && s.nodes.some(n => [n.cpu,n.memory,n.exporter].some(x => x.value !== null))) throw Error('METRICS_FAILURE');
  return s;
}
export function statusState(s: PublicStatus, now: number): Pick<StatusEnvelope, 'dataState' | 'health'> {
  if (now < s.collectedAt - 5000) return {dataState:'unavailable',health:'unknown'};
  if (now - s.collectedAt > 75000) return {dataState:'stale',health:'unknown'};
  const fresh = (t: number | null, age: number) => t !== null && now-t >= -5000 && now-t <= age;
  const p = s.policy;
  const complete = Object.values(s.sources).every(x => x.state === 'ok' && fresh(x.observedAt,75000)) &&
    s.nodes.every(n => n.os === 'linux' && n.arch !== null && fresh(n.readyObservedAt,90000) && [n.cpu,n.memory,n.exporter].every(x => x.value !== null && fresh(x.observedAt,90000))) &&
    s.workloads.every(w => w.ready !== null && w.desired !== null && fresh(w.observedAt,75000)) &&
    [p.mode,p.enabled,p.monitoring,p.incidentCount,p.activeIncidentCount].every(x => x !== null) && fresh(p.observedAt,75000);
  if (!complete) return {dataState:'partial',health:'unknown'};
  const healthy = s.nodes.every(n => n.readiness === 'Ready' && n.exporter.value === 1) && s.workloads.every(w => w.desired! > 0 && w.ready === w.desired) && p.mode === 'ObserveOnly' && p.enabled === true && p.monitoring === true && p.activeIncidentCount === 0;
  return {dataState:'current',health:healthy ? 'healthy' : 'degraded'};
}
