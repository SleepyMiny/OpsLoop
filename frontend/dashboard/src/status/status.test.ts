import {describe,it,expect,vi,afterEach} from 'vitest';
import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
import synthetic from '../../../../shared/status-synthetic.json';
import {parseStatus,parseStrictJSON,statusState} from './contract';
import {parseEnvelope,StatusClient} from './client';
import {Home,statusMode} from './StatusApp';
import {DemoSession} from './session';
import {DEMO_SCENARIOS} from '../data/demo/scenarios';
const clone=()=>structuredClone(synthetic);
afterEach(()=>{vi.useRealTimers();vi.unstubAllGlobals();});
describe('public schema and freshness',()=>{
  it('preserves original clocks and expires old data instead of refreshing on receipt',()=>{const s=parseStatus(clone());expect(statusState(s,s.collectedAt).dataState).toBe('current');expect(statusState(s,s.collectedAt+75001)).toEqual({dataState:'stale',health:'unknown'});s.nodes[0]!.cpu.observedAt=s.collectedAt-90001;expect(statusState(s,s.collectedAt).dataState).toBe('partial');});
  it('rejects duplicate JSON, nonfinite, invalid whitespace and unknown fields',()=>{for(const s of ['{"a":1,"a":2}','{"a":1e999}','\u00a0{}','{"x":NaN}'])expect(()=>parseStrictJSON(s)).toThrow();expect(()=>parseStatus({...clone(),internalIP:'192.0.2.5'})).toThrow();});
  it('rejects raw identities and incorrect measurement timestamps',()=>{const s=clone();s.nodes[0]!.id='private-node';expect(()=>parseStatus(s)).toThrow();const n=clone();const m=n.nodes[0]!.cpu as {value:number|null;observedAt:number|null};m.observedAt=null;expect(()=>parseStatus(n)).toThrow();});
  it('separates historical count, active failure and missing platform',()=>{const s=parseStatus(clone());s.policy.incidentCount=3;expect(statusState(s,s.collectedAt).health).toBe('healthy');s.policy.activeIncidentCount=1;expect(statusState(s,s.collectedAt).health).toBe('degraded');s.nodes[0]!.arch=null;expect(statusState(s,s.collectedAt).health).toBe('unknown');});
  it('refuses an envelope whose healthy badge contradicts its timestamp',()=>{const s=parseStatus(clone());const e={schemaVersion:1,serverTime:s.collectedAt,receivedAt:s.collectedAt,...statusState(s,s.collectedAt),snapshot:s};expect(parseEnvelope(e).snapshot.source).toBe('observed');expect(()=>parseEnvelope({...e,serverTime:s.collectedAt+80000})).toThrow();});
});
describe('Home lifecycle without fallback',()=>{
  it('preserves the browser global receiver when calling the default native fetch',async()=>{
    vi.useFakeTimers();let called=false;
    vi.stubGlobal('fetch',function(this:unknown){if(this!==globalThis)throw Error('Illegal invocation');called=true;return Promise.resolve(new Response('blocked',{status:503}));});
    const c=new StatusClient();c.start();await vi.advanceTimersByTimeAsync(1);c.stop();expect(called).toBe(true);
  });
  it('initial Home has no invented nodes or Demo playback',()=>{const html=renderToStaticMarkup(createElement(Home));expect(html).toContain('관측 데이터가 없습니다');expect(html).not.toContain('Ready 3');expect(html).not.toContain('시뮬레이션 데이터');});
  it.each(['','?source=home','?source=demo','?source=replay','?source=history'])('accepts supported mode %s',s=>expect(statusMode(s)).toBe(s?s.split('=')[1]:'home'));
  it.each(['?source=live','?source=unknown','?source=','?source=demo&source=home'])('rejects unsupported mode %s',s=>expect(()=>statusMode(s)).toThrow());
  it('polls exact same-origin GET; monotonic elapsed time drives stale between polls',async()=>{
    vi.useFakeTimers();const s=parseStatus(clone());let mono=0;const e={schemaVersion:1,serverTime:s.collectedAt,receivedAt:s.collectedAt,...statusState(s,s.collectedAt),snapshot:s};
    const fetcher=vi.fn(async()=>new Response(JSON.stringify(e),{headers:{'Content-Type':'application/json'}}));const c=new StatusClient(fetcher as typeof fetch,()=>mono,300000);c.start();await vi.advanceTimersByTimeAsync(1);expect(c.getSnapshot().error).toBeNull();mono=80000;await vi.advanceTimersByTimeAsync(1000);expect(statusState(c.getSnapshot().envelope!.snapshot,c.getSnapshot().now).dataState).toBe('stale');expect(fetcher).toHaveBeenCalledWith('/api/status',expect.objectContaining({method:'GET',credentials:'omit',redirect:'error'}));c.stop();const calls=fetcher.mock.calls.length;await vi.advanceTimersByTimeAsync(400000);expect(fetcher).toHaveBeenCalledTimes(calls);
  });
  it('403 and HTML200 never create a Demo snapshot',async()=>{
    vi.useFakeTimers();for(const r of [new Response('blocked',{status:403}),new Response('<html/>',{headers:{'Content-Type':'text/html'}})]){const c=new StatusClient(vi.fn(async()=>r) as typeof fetch);c.start();await vi.advanceTimersByTimeAsync(1);expect(c.getSnapshot().envelope).toBeNull();expect(c.getSnapshot().error).not.toBeNull();c.stop();}
  });
});
describe('finite opt-in Demo sessions',()=>{
  it('finalizes once for concurrent stop requests and labels data simulation',async()=>{const begin=vi.fn(async()=>{}),finish=vi.fn(async(_r:unknown)=>{});const s=new DemoSession(DEMO_SCENARIOS[0]!.id,()=>{},begin,finish);await Promise.all([s.finalize('stopped'),s.finalize('stopped')]);expect(begin).toHaveBeenCalledOnce();expect(finish).toHaveBeenCalledOnce();expect(finish.mock.calls[0]![0]).toMatchObject({source:'simulation',completion:'stopped',engineVersion:1});});
  it('seek-to-end records its provenance and never loops',async()=>{const finish=vi.fn(async(_r:unknown)=>{}),s=new DemoSession(DEMO_SCENARIOS[0]!.id,()=>{},async()=>{},finish);s.seek(s.getPlayback().durationMs);await Promise.resolve();await Promise.resolve();expect(finish).toHaveBeenCalledWith(expect.objectContaining({source:'simulation',completion:'completed',seekUsed:true}));expect(s.getPlayback().autoLoop).toBe(false);});
  it('storage rejection is visible and explicit retry can succeed',async()=>{const finish=vi.fn().mockRejectedValueOnce(Error('synthetic quota')).mockResolvedValue(undefined),s=new DemoSession(DEMO_SCENARIOS[0]!.id,()=>{},async()=>{},finish);await expect(s.finalize('stopped')).rejects.toThrow();await s.finalize('stopped');expect(finish).toHaveBeenCalledTimes(2);});
});

describe('review regression: History completion is a durable barrier',()=>{
  it('all concurrent finalizers wait for the same pending storage result',async()=>{
    let release!:()=>void;const persisted=new Promise<void>(r=>{release=r;});
    const finish=vi.fn(()=>persisted),s=new DemoSession(DEMO_SCENARIOS[0]!.id,()=>{},async()=>{},finish);
    const first=s.finalize('completed');await Promise.resolve();await Promise.resolve();
    let secondSettled=false;const second=s.finalize('stopped').then(()=>{secondSettled=true;});
    await Promise.resolve();await Promise.resolve();const settledBeforeCommit=secondSettled;
    release();await Promise.all([first,second]);
    expect(settledBeforeCommit).toBe(false);expect(finish).toHaveBeenCalledOnce();
  });
  it('concurrent callers observe storage failure and can explicitly retry',async()=>{
    let reject!:(e:Error)=>void;const failed=new Promise<void>((_,r)=>{reject=r;});
    const finish=vi.fn().mockReturnValueOnce(failed).mockResolvedValue(undefined),s=new DemoSession(DEMO_SCENARIOS[0]!.id,()=>{},async()=>{},finish);
    const first=s.finalize('stopped'),second=s.finalize('stopped');const outcomes=Promise.allSettled([first,second]);
    reject(Error('synthetic persistence failure'));const result=await outcomes;
    expect(result.map(r=>r.status)).toEqual(['rejected','rejected']);await s.finalize('stopped');expect(finish).toHaveBeenCalledTimes(2);
  });
});
