// Local-only workerd + D1 harness. No Cloudflare API calls, credentials or deployment.
import {Miniflare} from 'miniflare';
import fs from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const here=path.dirname(fileURLToPath(import.meta.url));
export function synthetic(now=Date.now()){
 const v=JSON.parse(fs.readFileSync(path.join(here,'../shared/status-synthetic.json'),'utf8')),old=v.collectedAt;
 const walk=x=>{if(!x||typeof x!=='object')return;for(const [k,v]of Object.entries(x)){if(['collectedAt','observedAt','readyObservedAt'].includes(k)&&typeof v==='number')x[k]=now+v-old;else walk(v);}};walk(v);return v;
}
export async function localServer({port=8787,persist,seed=false}={}){
 const config=JSON.parse(fs.readFileSync(path.join(here,'wrangler.status.json')));
 const mf=new Miniflare({host:'127.0.0.1',port,modules:true,scriptPath:path.join(here,'dist/worker.mjs'),compatibilityDate:config.compatibility_date,
  d1Databases:{STATUS_DB:'status-local'},...(persist?{d1Persist:persist}:{}),
  bindings:{PUBLIC_ORIGIN:config.vars.PUBLIC_ORIGIN,INGEST_KEYS:JSON.stringify({local:'SYNTHETIC-ONLY-NO-PRODUCTION-CREDENTIALS-0123456789'})},
  ratelimits:Object.fromEntries(['PUBLIC_RATE','AUTH_RATE','INGEST_RATE'].map((n,i)=>[n,{namespace_id:String(5500+i),simple:{limit:10000,period:60}}])),
  assets:{directory:path.resolve(here,config.assets.directory),binding:'ASSETS',routerConfig:{has_user_worker:true,static_routing:{user_worker:config.assets.run_worker_first}},assetConfig:{not_found_handling:'none'}}});
 try{
  const db=await mf.getD1Database('STATUS_DB');
  const exists=await db.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='collector_current'").first();
  if(!exists){const sql=fs.readFileSync(path.join(here,'migrations/0001_status.sql'),'utf8').replace(/^--.*$/gm,'');for(const q of sql.split(';').map(s=>s.trim()).filter(Boolean))await db.prepare(q).run();}
  const setSnapshot=async s=>{await db.prepare('UPDATE collector_current SET snapshot_json=?,collected_at_ms=?,received_at_ms=? WHERE collector_id=?').bind(JSON.stringify(s),s.collectedAt,Date.now(),'home-v1').run();};
  if(seed)await setSnapshot(synthetic());
  return {mf,db,url:(await mf.ready).origin,setSnapshot,close:()=>mf.dispose()};
 }catch(e){await mf.dispose();throw e;}
}
if(process.argv[1]===fileURLToPath(import.meta.url)){
 const server=await localServer({seed:process.argv.includes('--synthetic'),persist:path.join(here,'.local/status')});
 console.log('LOCAL ONLY',server.url,process.argv.includes('--synthetic')?'SYNTHETIC FIXTURE, not live infrastructure':'no synthetic data seeded');
 for(const sig of ['SIGINT','SIGTERM'])process.once(sig,async()=>{await server.close();process.exit(0);});
}
