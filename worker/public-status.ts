import {parseStrictJSON, parseStatus, statusState} from '../frontend/dashboard/src/status/contract';

interface Statement {bind(...args: unknown[]): Statement; first<T = Record<string,unknown>>(): Promise<T | null>; all<T>(): Promise<{results:T[]}>}
interface Env {
  STATUS_DB: {prepare(sql:string):Statement};
  PUBLIC_ORIGIN: string;
  INGEST_KEYS: string;
  PUBLIC_RATE: {limit(x:{key:string}): Promise<{success:boolean}>};
  INGEST_RATE: {limit(x:{key:string}): Promise<{success:boolean}>};
  AUTH_RATE: {limit(x:{key:string}): Promise<{success:boolean}>};
}
const path = '/api/collect/status';
const encoder = new TextEncoder();
const hex = (x:ArrayBuffer) => [...new Uint8Array(x)].map(b => b.toString(16).padStart(2,'0')).join('');
export const digest = async (raw:Uint8Array) => hex(await crypto.subtle.digest('SHA-256',raw as BufferSource));
export async function canonical(origin:string, collector:string, kid:string, seq:string, sent:string, raw:Uint8Array) {
  return ['OPSLOOP-STATUS-V1',origin,'POST',path,collector,kid,seq,sent,await digest(raw)].join('\n');
}
function response(code:number, body:unknown, headers:Record<string,string> = {}) {
  return new Response(JSON.stringify(body), {status:code,headers:{'Content-Type':'application/json; charset=utf-8','Cache-Control':'no-store','X-Content-Type-Options':'nosniff','Content-Security-Policy':"default-src 'none'; frame-ancestors 'none'",...headers}});
}
const failure = (code:number, error:string, headers?:Record<string,string>) => response(code,{schemaVersion:1,error,snapshot:null},headers);
async function limitedBody(r:Request) {
  if (r.headers.has('Content-Encoding') || r.headers.get('Content-Type') !== 'application/json') throw Error('BODY_FORMAT');
  if (r.headers.has('Content-Length') && (!/^\d+$/.test(r.headers.get('Content-Length')!) || Number(r.headers.get('Content-Length')) > 16384)) throw Error('BODY_LIMIT');
  if (!r.body) throw Error('BODY_FORMAT');
  const reader=r.body.getReader(); const chunks:Uint8Array[]=[]; let size=0;
  let timer:ReturnType<typeof setTimeout>;
  const deadline=new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(Error('BODY_TIMEOUT')),3000);});
  try { for (;;) { const {value,done}=await Promise.race([reader.read(),deadline]); if (done) break; size+=value.length; if(size>16384) throw Error('BODY_LIMIT'); chunks.push(value); } }
  finally { clearTimeout(timer!); void reader.cancel().catch(()=>{}); }
  const raw=new Uint8Array(size); let at=0; for(const x of chunks){raw.set(x,at);at+=x.length;} return raw;
}
export default {
  async fetch(r:Request, env:Env):Promise<Response> {
    const u=new URL(r.url); const now=Date.now();
    if (u.pathname !== '/api/status' && u.pathname !== path) return failure(404,'NOT_FOUND');
    if (u.search) return failure(400,'QUERY_NOT_ALLOWED');
    const method=u.pathname === '/api/status' ? 'GET' : 'POST';
    if (r.method !== method) return failure(405,'METHOD_NOT_ALLOWED',{'Allow':method});
    try {
      const ip=r.headers.get('CF-Connecting-IP') || 'local';
      const limiter=u.pathname === '/api/status' ? env.PUBLIC_RATE : env.AUTH_RATE;
      if (!limiter || !(await limiter.limit({key:ip})).success) return failure(429,'RATE_LIMIT',{'Retry-After':'30'});
      if (u.pathname === '/api/status') {
        const row=await env.STATUS_DB.prepare('SELECT received_at_ms, snapshot_json FROM collector_current WHERE collector_id=?').bind('home-v1').first<{received_at_ms:number;snapshot_json:string|null}>();
        if (!row?.snapshot_json) return failure(503,'NO_OBSERVATION');
        const snapshot=parseStatus(parseStrictJSON(row.snapshot_json));
        return response(200,{schemaVersion:1,serverTime:now,receivedAt:row.received_at_ms,...statusState(snapshot,now),snapshot});
      }
      // Never trust forwarded host/origin headers. Only the fixed deployed origin.
      if (u.origin !== env.PUBLIC_ORIGIN || u.protocol !== 'https:') return failure(401,'UNAUTHORIZED');
      const collector=r.headers.get('X-OpsLoop-Collector'),kid=r.headers.get('X-OpsLoop-Key-Id');
      const seq=r.headers.get('X-OpsLoop-Sequence'),sent=r.headers.get('X-OpsLoop-Sent-At'),signature=r.headers.get('X-OpsLoop-Signature');
      if (collector !== 'home-v1' || !kid || !/^[a-zA-Z0-9_-]{1,40}$/.test(kid) || !seq || !sent || !/^[1-9]\d{0,15}$/.test(seq) || !/^[1-9]\d{0,15}$/.test(sent) || !signature || !/^[0-9a-f]{64}$/.test(signature)) return failure(401,'UNAUTHORIZED');
      if (!Number.isSafeInteger(Number(seq)) || !Number.isSafeInteger(Number(sent)) || now-Number(sent)>120000 || now-Number(sent)<-5000) return failure(401,'UNAUTHORIZED');
      let keys:Record<string,unknown>; try { keys=JSON.parse(env.INGEST_KEYS);if(!keys||typeof keys!=='object'||Array.isArray(keys)||Object.keys(keys).length<1||Object.keys(keys).length>2)throw Error('KEY_CONFIG'); } catch { return failure(503,'AUTH_UNAVAILABLE'); }
      const secret=Object.hasOwn(keys,kid) ? keys[kid] : undefined;
      if (typeof secret !== 'string' || !/^[!-~]{32,128}$/.test(secret)) return failure(401,'UNAUTHORIZED');
      let raw:Uint8Array; try { raw=await limitedBody(r); } catch(e) { const code=(e as Error).message;return failure(code==='BODY_LIMIT'?413:code==='BODY_TIMEOUT'?408:415,'INVALID_BODY'); }
      const key=await crypto.subtle.importKey('raw',encoder.encode(secret),{name:'HMAC',hash:'SHA-256'},false,['verify']);
      const sig=new Uint8Array(signature.match(/../g)!.map(h=>parseInt(h,16)));
      if (!await crypto.subtle.verify('HMAC',key,sig,encoder.encode(await canonical(env.PUBLIC_ORIGIN,collector,kid,seq,sent,raw)))) return failure(401,'UNAUTHORIZED');
      if (!env.INGEST_RATE || !(await env.INGEST_RATE.limit({key:collector})).success) return failure(429,'RATE_LIMIT',{'Retry-After':'30'});
      let snapshot; try { snapshot=parseStatus(parseStrictJSON(new TextDecoder('utf-8',{fatal:true}).decode(raw))); } catch { return failure(400,'INVALID_SNAPSHOT'); }
      if (Number(sent)-snapshot.collectedAt>30000 || Number(sent)-snapshot.collectedAt < -5000) return failure(400,'INVALID_COLLECTION_TIME');
      const row=await env.STATUS_DB.prepare(`UPDATE collector_current SET seq=?,sent_at_ms=?,collected_at_ms=?,received_at_ms=?,snapshot_json=? WHERE collector_id=? AND seq<? AND received_at_ms<=? AND collected_at_ms<=? RETURNING seq`).bind(Number(seq),Number(sent),snapshot.collectedAt,now,JSON.stringify(snapshot),collector,Number(seq),now-15000,snapshot.collectedAt).first<{seq:number}>();
      if (!row) return failure(409,'REPLAY_ORDER_OR_RATE');
      return response(202,{accepted:true});
    } catch { return failure(503,'STORE_UNAVAILABLE'); }
  }
};
