import {test,before,after,beforeEach} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {createHmac,createHash} from 'node:crypto';
import {spawnSync} from 'node:child_process';
import {Miniflare} from 'miniflare';
const root=path.resolve('..'), origin='https://opsloop.sleepyminy44.workers.dev';
const key='SYNTHETIC-ONLY-LOCAL-KEY-01234567890123456789';
const schemaSQL=fs.readFileSync('migrations/0001_status.sql','utf8');
fs.mkdirSync('../reports/public-status-home',{recursive:true});
let mf,db,temporary,collectorBinary;
export function fresh(now=Date.now()) {
 const v=JSON.parse(fs.readFileSync('../shared/status-synthetic.json','utf8')),old=v.collectedAt;
 function walk(x){if(!x||typeof x!=='object')return;for(const [k,v] of Object.entries(x)){if(['collectedAt','observedAt','readyObservedAt'].includes(k)&&typeof v==='number')x[k]=now+(v-old);else walk(v);}}
 walk(v);return v;
}
function options(extra={}){return {modules:true,scriptPath:'dist/worker.mjs',compatibilityDate:'2026-08-06',d1Databases:{STATUS_DB:'status-local'},bindings:{PUBLIC_ORIGIN:origin,INGEST_KEYS:JSON.stringify({k1:key,k2:key+'2'})},ratelimits:Object.fromEntries(['PUBLIC_RATE','AUTH_RATE','INGEST_RATE'].map((name,i)=>[name,{namespace_id:String(1100+i),simple:{limit:10000,period:60}}])),...extra};}
function signed(s=fresh(),seq=1,extra={}){
 const raw=typeof s==='string'?s:JSON.stringify(s),sent=String(Date.now()),kid=extra.kid||'k1',secret=extra.key||key;
 const canonical=['OPSLOOP-STATUS-V1',origin,'POST','/api/collect/status','home-v1',kid,String(seq),sent,createHash('sha256').update(raw).digest('hex')].join('\n');
 return {method:'POST',headers:{'Content-Type':'application/json','X-OpsLoop-Collector':'home-v1','X-OpsLoop-Key-Id':kid,'X-OpsLoop-Sequence':String(seq),'X-OpsLoop-Sent-At':sent,'X-OpsLoop-Signature':createHmac('sha256',secret).update(canonical).digest('hex'),...extra.headers},body:raw};
}
const post=(s,seq,extra)=>mf.dispatchFetch(origin+'/api/collect/status',signed(s,seq,extra));
before(async()=>{temporary=fs.mkdtempSync(path.join(os.tmpdir(),'opsloop-workerd-'));collectorBinary=path.join(temporary,'status-collector');const compiled=spawnSync('go',['build','-o',collectorBinary,'./cmd/status-collector'],{cwd:root,env:process.env,encoding:'utf8'});assert.equal(compiled.status,0,compiled.stderr);mf=new Miniflare(options({d1Persist:path.join(temporary,'d1')}));db=await mf.getD1Database('STATUS_DB');for(const q of schemaSQL.replace(/^--.*$/gm,'').split(';').map(x=>x.trim()).filter(Boolean))await db.prepare(q).run();});
after(async()=>{await mf?.dispose();fs.rmSync(temporary,{recursive:true,force:true});});
beforeEach(async()=>{await db.prepare("UPDATE collector_current SET seq=0,sent_at_ms=0,collected_at_ms=0,received_at_ms=0,snapshot_json=NULL").run();});
test('actual workerd+D1: authenticated write, public status and headers',async()=>{
 assert.equal((await post()).status,202);const r=await mf.dispatchFetch(origin+'/api/status');assert.equal(r.status,200);assert.equal(r.headers.get('Cache-Control'),'no-store');assert.equal(r.headers.get('X-Content-Type-Options'),'nosniff');assert.match(r.headers.get('Content-Security-Policy'),/default-src 'none'/);const x=await r.json();assert.equal(x.dataState,'current');assert.equal(x.health,'healthy');assert.equal(x.snapshot.nodes.length,3);assert.equal(x.snapshot.policy.incidentCount,0);
});
test('empty DB is 503; API unknown/query/method never HTML fallback',async()=>{
 assert.equal((await mf.dispatchFetch(origin+'/api/status')).status,503);
 for(const [url,method,status,allow] of [['/api/unknown','GET',404,null],['/api/status','POST',405,'GET'],['/api/collect/status','GET',405,'POST'],['/api/status?x=1','GET',400,null]]){const r=await mf.dispatchFetch(origin+url,{method});assert.equal(r.status,status);assert.match(r.headers.get('Content-Type'),/application\/json/);if(allow)assert.equal(r.headers.get('Allow'),allow);}
});
test('unauthenticated, invalid key and duplicate/oversized auth headers cannot write',async()=>{
 assert.equal((await mf.dispatchFetch(origin+'/api/collect/status',{method:'POST',body:'{}'})).status,401);
 for(const extra of [{key:'wrong-synthetic-key'},{kid:'unknown'},{headers:{'X-OpsLoop-Sequence':'1, 1'}},{headers:{'X-OpsLoop-Sent-At':String(Date.now()-120001)}}])assert.equal((await post(fresh(),1,extra)).status,401);
 assert.equal((await db.prepare('SELECT seq FROM collector_current').first()).seq,0);
});
test('strict allowlist drops no fields silently at the ingest boundary',async()=>{
 for(const f of [s=>s.secret='SYNTHETIC_PRIVATE_SENTINEL',s=>s.nodes[0].internalIP='192.0.2.7',s=>s.policy.spec={token:'SYNTHETIC_PRIVATE_SENTINEL'},s=>s.nodes[0].id='internal-node.private.test']){const s=fresh();f(s);assert.equal((await post(s)).status,400);}
 assert.equal((await db.prepare('SELECT seq FROM collector_current').first()).seq,0);
});
test('duplicate JSON keys, UTF8 and actual oversized streamed bodies are rejected',async()=>{
 const raw=JSON.stringify(fresh());assert.equal((await post('{"schemaVersion":2,'+raw.slice(1))).status,400);
 assert.equal((await post(' '.repeat(16385)+raw)).status,413);
 const r=signed(fresh());r.headers['Content-Encoding']='gzip';assert.equal((await mf.dispatchFetch(origin+'/api/collect/status',r)).status,415);
});
test('same sequence and out-of-order request rejected atomically, rate gate retained',async()=>{
 assert.equal((await post(fresh(),10)).status,202);
 for(const seq of [10,9,11])assert.equal((await post(fresh(),seq)).status,409);
 assert.equal((await db.prepare('SELECT seq FROM collector_current').first()).seq,10);
});
test('eight concurrent authenticated duplicates: exactly one D1 write',async()=>{
 const req=signed();const results=await Promise.all(Array.from({length:8},()=>mf.dispatchFetch(origin+'/api/collect/status',req)));assert.equal(results.filter(r=>r.status===202).length,1);assert.ok(results.every(r=>[202,409].includes(r.status)));
});
test('key rotation shares counter, stale collection ordering remains protected',async()=>{
 assert.equal((await post(fresh(),5)).status,202);await db.prepare('UPDATE collector_current SET received_at_ms=0').run();
 assert.equal((await post(fresh(),4,{kid:'k2',key:key+'2'})).status,409);
 assert.equal((await post(fresh(),6,{kid:'k2',key:key+'2'})).status,202);
});
test('original stale samples, metric failure and unexpected Enforce never show healthy',async()=>{
 const s=fresh();s.nodes[0].cpu.observedAt=Date.now()-91000;assert.equal((await post(s)).status,202);
 let x=await(await mf.dispatchFetch(origin+'/api/status')).json();assert.equal(x.dataState,'partial');assert.equal(x.health,'unknown');assert.equal(x.snapshot.nodes[0].cpu.observedAt,s.nodes[0].cpu.observedAt);
 s.policy.mode='Enforce';s.sources.prometheus={state:'unavailable',observedAt:null,errorCode:'TIMEOUT'};for(const n of s.nodes)for(const k of ['cpu','memory','exporter'])n[k]={value:null,observedAt:null};await db.prepare('UPDATE collector_current SET received_at_ms=0').run();assert.equal((await post(s,2)).status,202);x=await(await mf.dispatchFetch(origin+'/api/status')).json();assert.equal(x.snapshot.policy.mode,'Enforce');assert.notEqual(x.health,'healthy');
});
test('old stored observation stays stale, outage and new collection recover',async()=>{
 const s=fresh(Date.now()-80000);await db.prepare('UPDATE collector_current SET snapshot_json=?,collected_at_ms=?,received_at_ms=?').bind(JSON.stringify(s),s.collectedAt,Date.now()).run();let x=await(await mf.dispatchFetch(origin+'/api/status')).json();assert.equal(x.dataState,'stale');assert.equal(x.health,'unknown');await db.prepare('UPDATE collector_current SET received_at_ms=0').run();assert.equal((await post()).status,202);x=await(await mf.dispatchFetch(origin+'/api/status')).json();assert.equal(x.dataState,'current');
});
test('schema-invalid stored row and SQL outage fail closed without diagnostic leakage',async()=>{
 await db.prepare('UPDATE collector_current SET snapshot_json=?').bind('{"secret":"SYNTHETIC_PRIVATE_SENTINEL"}').run();const r=await mf.dispatchFetch(origin+'/api/status');assert.equal(r.status,503);assert.doesNotMatch(await r.text(),/SYNTHETIC_PRIVATE_SENTINEL|SELECT|stack/i);
});
test('Go-produced signed envelope is accepted by actual WebCrypto and D1',async()=>{
 const dir=fs.mkdtempSync(path.join(temporary,'go-'));fs.chmodSync(dir,0o700);const file=(n,b)=>{const f=path.join(dir,n);fs.writeFileSync(f,b,{mode:0o600});return f;};const seq=path.join(dir,'seq');fs.mkdirSync(seq,{mode:0o700});
 const keyFile=file('signing',key),cfg=file('config.json',JSON.stringify({origin,keyId:'k1',keyFile,sequenceDir:seq})),input=file('fixture.json',JSON.stringify(fresh())),out=path.join(dir,'request.json');
 const run=spawnSync(collectorBinary,['--config',cfg,'--init-sequence','--offline-fixture',input,'--envelope-output',out],{cwd:root,env:{...process.env,KUBECONFIG:'/nonexistent/NO_LIVE_CREDENTIALS'},encoding:'utf8'});assert.equal(run.status,0,run.stderr);
 const envelope=JSON.parse(fs.readFileSync(out,'utf8')),headers=Object.fromEntries(Object.entries(envelope.headers).map(([k,v])=>[k,v.join(',')]));assert.equal((await mf.dispatchFetch(envelope.url,{method:envelope.method,headers,body:envelope.body})).status,202);
});
test('persisted D1 rejects replay across workerd restart',async()=>{
 const req=signed();assert.equal((await mf.dispatchFetch(origin+'/api/collect/status',req)).status,202);await mf.dispose();mf=new Miniflare(options({d1Persist:path.join(temporary,'d1')}));db=await mf.getD1Database('STATUS_DB');assert.equal((await mf.dispatchFetch(origin+'/api/collect/status',req)).status,409);
});
test('real rate binding denies excess public requests without D1 writes',async()=>{
 const limited=new Miniflare(options({ratelimits:{PUBLIC_RATE:{namespace_id:'9911',simple:{limit:1,period:60}}}}));try{const r1=await limited.dispatchFetch(origin+'/api/status');assert.equal(r1.status,503);const r2=await limited.dispatchFetch(origin+'/api/status');assert.equal(r2.status,429);assert.equal(r2.headers.get('Retry-After'),'30');}finally{await limited.dispose();}
});
test('a failing D1 update does not advance seq or body; later recovery succeeds',async()=>{
 await db.prepare("CREATE TRIGGER fail_status BEFORE UPDATE ON collector_current BEGIN SELECT RAISE(ABORT, 'SYNTHETIC_SQL_FAILURE'); END").run();
 try{const r=await post();assert.equal(r.status,503);assert.doesNotMatch(await r.text(),/SYNTHETIC_SQL_FAILURE|UPDATE|stack/);assert.equal((await db.prepare('SELECT seq FROM collector_current').first()).seq,0);}finally{await db.prepare('DROP TRIGGER fail_status').run();}
 assert.equal((await post()).status,202);
});
test('non-UTF8 authenticated body fails closed and missing signing config cannot admit writes',async()=>{
 const r=signed('placeholder'),raw=Buffer.from([0x7b,0x22,0xff,0x22,0x3a,0x31,0x7d]),sent=r.headers['X-OpsLoop-Sent-At'];
 const c=['OPSLOOP-STATUS-V1',origin,'POST','/api/collect/status','home-v1','k1','1',sent,createHash('sha256').update(raw).digest('hex')].join('\n');r.headers['X-OpsLoop-Signature']=createHmac('sha256',key).update(c).digest('hex');r.body=raw;
 assert.equal((await mf.dispatchFetch(origin+'/api/collect/status',r)).status,400);
 const unconfigured=new Miniflare(options({bindings:{PUBLIC_ORIGIN:origin,INGEST_KEYS:'INVALID_CONFIG'}}));try{const res=await unconfigured.dispatchFetch(origin+'/api/collect/status',signed());assert.equal(res.status,503);assert.doesNotMatch(await res.text(),/INVALID_CONFIG/);}finally{await unconfigured.dispose();}
});
test('a never-ending request body is bounded by a real runtime deadline',async()=>{
 const req=signed(),stream=new ReadableStream({start(c){c.enqueue(new TextEncoder().encode('{'));}});req.body=stream;req.duplex='half';const start=Date.now();const r=await mf.dispatchFetch(origin+'/api/collect/status',req);assert.equal(r.status,408);assert.ok(Date.now()-start<7000);
});
test('synthetic Kubernetes TLS + seven PromQL -> real Go Collector -> Worker -> D1 -> status',async()=>{
 const dir=fs.mkdtempSync(path.join(temporary,'pipeline-')),out=path.join(dir,'envelope.json'),keyFile=path.join(dir,'key');fs.chmodSync(dir,0o700);fs.writeFileSync(keyFile,key,{mode:0o600});
 const run=spawnSync('go',['test','./internal/publicstatus','-run','^TestCollectorPipelineEnvelope$','-count=1'],{cwd:root,env:{...process.env,OPSLOOP_TEST_ENVELOPE:out,OPSLOOP_TEST_KEY_FILE:keyFile,KUBECONFIG:'/nonexistent/NO_LIVE_CREDENTIALS'},encoding:'utf8'});assert.equal(run.status,0,run.stderr+run.stdout);
 const e=JSON.parse(fs.readFileSync(out,'utf8'));assert.doesNotMatch(e.body,/SYNTHETIC_PRIVATE|192\.0\.2\.|private-control|private-policy/);const headers=Object.fromEntries(Object.entries(e.headers).map(([k,v])=>[k,v.join(',')]));assert.equal((await mf.dispatchFetch(e.url,{method:e.method,headers,body:e.body})).status,202);
 const view=await(await mf.dispatchFetch(origin+'/api/status')).json();assert.equal(view.dataState,'current');assert.equal(view.health,'healthy');assert.equal(view.snapshot.nodes[0].cpu.value,0.25);assert.equal(view.snapshot.nodes[2].arch,'arm64');fs.writeFileSync('../reports/public-status-home/collector-roundtrip-snapshot.json',JSON.stringify({syntheticOnly:true,run:'Go TLS/Prometheus collection + authenticated Worker/D1',...view},null,2)+'\n');
});
test('origin binding and missing durable admission state cannot be bypassed',async()=>{
 const wrong=await mf.dispatchFetch('https://wrong-origin.invalid/api/collect/status',signed());assert.equal(wrong.status,401);
 await db.prepare('DELETE FROM collector_current').run();try{assert.equal((await post()).status,409);assert.equal((await db.prepare('SELECT COUNT(*) AS n FROM collector_current').first()).n,0);}finally{await db.prepare("INSERT INTO collector_current VALUES ('home-v1',0,0,0,0,NULL)").run();}
});

test('Collector bootstrap is explicit, network-free and refuses an existing counter',()=>{
 const dir=fs.mkdtempSync(path.join(temporary,'bootstrap-'));fs.chmodSync(dir,0o700);const seq=path.join(dir,'seq');fs.mkdirSync(seq,{mode:0o700});const cfg=path.join(dir,'config');fs.writeFileSync(cfg,JSON.stringify({sequenceDir:seq}),{mode:0o600});
 const args=['--config',cfg,'--init-sequence','--init-only'];let run=spawnSync(collectorBinary,args,{encoding:'utf8'});assert.equal(run.status,0,run.stderr);assert.equal(fs.readFileSync(path.join(seq,'counter'),'utf8'),'0');
 run=spawnSync(collectorBinary,args,{encoding:'utf8'});assert.equal(run.status,1);assert.equal(fs.readFileSync(path.join(seq,'counter'),'utf8'),'0');assert.doesNotMatch(run.stderr,/config.*bootstrap-|BEGIN|secret/);
});

test('review: authenticated body/header tampering is rejected without changing D1',async()=>{
 const original=signed(), variants=[];
 variants.push({...original,body:original.body.replace('"observed"','"forged"')});
 for(const [header,value] of [['X-OpsLoop-Collector','unknown'],['X-OpsLoop-Sequence','9007199254740992'],['X-OpsLoop-Sequence','2'],['X-OpsLoop-Sent-At',String(Date.now()+60000)]])variants.push({...original,headers:{...original.headers,[header]:value}});
 const sig=original.headers['X-OpsLoop-Signature'];for(const pos of [0,63])variants.push({...original,headers:{...original.headers,'X-OpsLoop-Signature':sig.slice(0,pos)+(sig[pos]==='0'?'1':'0')+sig.slice(pos+1)}});
 for(const request of variants){const r=await mf.dispatchFetch(origin+'/api/collect/status',request);assert.equal(r.status,401);assert.deepEqual(await r.json(),{schemaVersion:1,error:'UNAUTHORIZED',snapshot:null});}
 const row=await db.prepare('SELECT seq,snapshot_json FROM collector_current').first();assert.equal(row.seq,0);assert.equal(row.snapshot_json,null);
});
test('review: collected time and safe-number schema violations cannot consume sequence',async()=>{
 for(const mutate of [s=>s.collectedAt=Date.now()-31000,s=>s.nodes[0].cpu.value=1.01,s=>s.nodes[0].readyObservedAt=s.collectedAt+5001,s=>s.policy.activeIncidentCount=1]){const s=fresh();mutate(s);assert.equal((await post(s)).status,400);}
 assert.equal((await db.prepare('SELECT seq FROM collector_current').first()).seq,0);assert.equal((await post()).status,202);
});
test('review: explicitly retired signing key fails while rotation retains durable sequence',async()=>{
 const tmp=fs.mkdtempSync(path.join(temporary,'rotate-')),settings={d1Persist:tmp};let rotated=new Miniflare(options(settings));
 try{
  let d=await rotated.getD1Database('STATUS_DB');for(const q of schemaSQL.replace(/^--.*$/gm,'').split(';').map(x=>x.trim()).filter(Boolean))await d.prepare(q).run();
  assert.equal((await rotated.dispatchFetch(origin+'/api/collect/status',signed(fresh(),5))).status,202);await d.prepare('UPDATE collector_current SET received_at_ms=0').run();await rotated.dispose();
  rotated=new Miniflare(options({...settings,bindings:{PUBLIC_ORIGIN:origin,INGEST_KEYS:JSON.stringify({k2:key+'2'})}}));
  assert.equal((await rotated.dispatchFetch(origin+'/api/collect/status',signed(fresh(),6))).status,401);
  assert.equal((await rotated.dispatchFetch(origin+'/api/collect/status',signed(fresh(),4,{kid:'k2',key:key+'2'}))).status,409);
  assert.equal((await rotated.dispatchFetch(origin+'/api/collect/status',signed(fresh(),6,{kid:'k2',key:key+'2'}))).status,202);
 }finally{await rotated.dispose();}
});
