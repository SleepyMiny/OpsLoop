import {chromium} from 'playwright-core';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {localServer,synthetic} from './local.mjs';
const out=path.resolve('../reports/public-status-home/browser');fs.mkdirSync(out,{recursive:true});
const results={syntheticOnly:true,checks:[],api:[],external:[],pageErrors:[],consoleErrors:[],pass:false};
const tmp=fs.mkdtempSync(path.join(os.tmpdir(),'opsloop-status-browser-'));let server,browser;
try{
 server=await localServer({port:0,persist:path.join(tmp,'d1'),seed:true});const base=server.url;
 const asset=await fetch(base);if(asset.status!==200||!asset.headers.get('Content-Security-Policy')?.includes("connect-src 'self'"))throw Error('Actual static asset/CSP routing failure');
 results.assetHeaders={csp:asset.headers.get('Content-Security-Policy'),type:asset.headers.get('Content-Type')};
 for(const [url,method,status]of [['/api/unknown','GET',404],['/api/status','POST',405],['/api/collect/status','GET',405]]){const r=await fetch(base+url,{method});if(r.status!==status||!r.headers.get('Content-Type')?.includes('application/json'))throw Error('API priority/fallback '+url);results.checks.push({case:url,method,status});}
 browser=await chromium.launch({executablePath:process.env.CHROME_PATH||chromium.executablePath(),headless:true,args:['--no-sandbox']});
 results.browserVersion=browser.version();
 for(const viewport of [{width:1920,height:1080},{width:390,height:844}]){
  await server.setSnapshot(synthetic());const context=await browser.newContext({viewport,serviceWorkers:'block'});let api=[];
  await context.route('**/*',route=>{const u=new URL(route.request().url());if(u.origin!==base){results.external.push(u.origin);return route.abort();}if(u.pathname.startsWith('/api/')){const entry={path:u.pathname,method:route.request().method()};api.push(entry);results.api.push(entry);}return route.continue();});
  const page=await context.newPage();page.on('pageerror',e=>results.pageErrors.push(e.message));page.on('console',m=>{if(m.type()==='error')results.consoleErrors.push(m.text());});
  const shot=async name=>{const layout=await page.evaluate(()=>({width:innerWidth,content:document.documentElement.scrollWidth}));if(layout.content>layout.width)throw Error('Horizontal overflow '+name+JSON.stringify(layout));await page.screenshot({path:path.join(out,viewport.width+'-'+name+'.png'),fullPage:viewport.width<1000});results.checks.push({viewport:viewport.width,case:name,pass:true});};
  await page.goto(base);await page.locator('[data-state=current]').waitFor();await page.getByRole('heading',{name:'control',exact:true}).waitFor();await page.getByRole('heading',{name:'cloud',exact:true}).waitFor();if((await page.locator('body').innerText()).match(/192\.0\.2\.|private-node|SYNTHETIC_PRIVATE/))throw Error('Private sentinel visible');await shot('home-current');
  await server.setSnapshot(synthetic(Date.now()-80001));await page.reload();await page.locator('[data-state=stale]').waitFor();await shot('home-stale');
  await server.setSnapshot(synthetic());await page.reload();await page.locator('[data-state=current]').waitFor();results.checks.push({viewport:viewport.width,case:'freshness-recovery',pass:true});
  const partial=synthetic();partial.sources.prometheus={state:'unavailable',observedAt:null,errorCode:'TIMEOUT'};for(const n of partial.nodes)for(const k of ['cpu','memory','exporter'])n[k]={value:null,observedAt:null};await server.setSnapshot(partial);await page.reload();await page.locator('[data-state=partial]').waitFor();await page.getByText('TIMEOUT',{exact:false}).waitFor();await shot('home-partial');
  await page.route('**/api/status',r=>r.fulfill({status:503,contentType:'application/json',body:'{"error":"STORE_UNAVAILABLE","snapshot":null}'}));await page.reload();await page.getByText('현재 관측 API에 연결할 수 없습니다.',{exact:false}).waitFor();if(await page.getByText('시뮬레이션 데이터',{exact:true}).count())throw Error('Demo substituted for outage');await shot('home-api-failure');await page.unroute('**/api/status');
  api=[];await page.goto(base+'/?source=demo');await page.getByRole('heading',{name:'선택형 장애 시뮬레이션'}).waitFor();if(await page.getByText('시뮬레이션 데이터',{exact:true}).count())throw Error('Demo starts before choice');await shot('demo-selection');
  await page.getByRole('button',{name:'시뮬레이션 시작',exact:true}).first().click();await page.getByText('시뮬레이션 데이터',{exact:true}).waitFor();await shot('demo-running');await page.getByRole('button',{name:'시연 종료 · History 저장'}).click();await page.waitForURL('**/?source=history');await page.getByText('simulation · stopped',{exact:false}).waitFor();await page.reload();await page.getByText('simulation · stopped',{exact:false}).waitFor();
  const history=await page.evaluate(()=>new Promise((resolve,reject)=>{const r=indexedDB.open('opsloop-simulation-history-v1',1);r.onsuccess=()=>{const db=r.result,q=db.transaction('history').objectStore('history').getAll();q.onsuccess=()=>{db.close();resolve(q.result);};q.onerror=reject;};r.onerror=reject;}));if(history.length!==1||history[0].source!=='simulation')throw Error('IndexedDB duplicate or wrong provenance');await shot('history-persisted');
  await page.goto(base+'/?source=replay');await page.getByLabel('보존 시점').selectOption('1');await page.getByText('IMAGE_PULL',{exact:true}).first().waitFor();await page.getByText('MANUAL_REQUIRED',{exact:true}).first().waitFor();await shot('image-pull');await page.getByLabel('실증 기록').selectOption('node-not-ready');await page.getByLabel('보존 시점').selectOption('1');await page.getByText('OBSERVED',{exact:true}).first().waitFor();await page.getByRole('tab',{name:'RecoveryIncident',exact:true}).click();const record=await page.getByRole('region',{name:'RecoveryIncident 기록',exact:true}).innerText();if(!record.includes('NODE_NOT_READY')||!record.includes('INFRASTRUCTURE_FAILURE'))throw Error('classification regression');await page.getByText('기록 범위 · 증거 시각 및 출처',{exact:true}).click();await page.getByText(/운영자 start-only 복원 요청 \(자동 복구 아님\)/).waitFor();await shot('node-not-ready');
  for(const mode of ['live','invalid']){await page.goto(base+'/?source='+mode);await page.getByRole('heading',{name:'지원하지 않는 모드',exact:true}).waitFor();}if(api.length)throw Error('API requested in simulation/replay/history/invalid modes: '+JSON.stringify(api));results.checks.push({viewport:viewport.width,case:'static-modes-zero-api',pass:true});
  await page.goto(base+'/?source=history');const csp=await page.evaluate(async()=>{let violation=null;document.addEventListener('securitypolicyviolation',e=>{violation=e.violatedDirective;},{once:true});try{await fetch('https://CSP-BLOCKED.invalid/status');}catch{}await new Promise(r=>setTimeout(r,40));return violation;});if(csp!=='connect-src')throw Error('CSP does not block external API');results.checks.push({viewport:viewport.width,case:'CSP-blocks-external',pass:true});
  await page.evaluate(async()=>{await new Promise((resolve,reject)=>{const q=indexedDB.open('opsloop-simulation-history-v1',1);q.onsuccess=()=>{const db=q.result,t=db.transaction('history','readwrite');const base={source:'simulation',scenarioId:'synthetic-test',title:'Synthetic retention fixture',engineVersion:1,seed:1,startedAt:Date.now(),endedAt:Date.now(),simElapsedMs:1,completion:'stopped',seekUsed:false};for(let i=0;i<70;i++)t.objectStore('history').put({...base,id:crypto.randomUUID(),startedAt:Date.now()-i,endedAt:Date.now()-i});t.objectStore('history').put({...base,id:crypto.randomUUID(),startedAt:Date.now()-31*86400000,endedAt:Date.now()-31*86400000});t.oncomplete=()=>{db.close();resolve();};t.onerror=reject;};q.onerror=reject;});});await page.reload();await page.getByText('Synthetic retention fixture',{exact:true}).first().waitFor();const articles=await page.locator('.status-detail-grid article').count();if(articles!==50)throw Error('History limit '+articles);results.checks.push({viewport:viewport.width,case:'history-50-TTL',pass:true});
  await context.close();
 }
 // Abort a real IndexedDB finalize transaction after its put request succeeds.
 // The UI must stay in Demo, retain pending state and allow a durable retry.
 const retryContext=await browser.newContext({serviceWorkers:'block'});
 await retryContext.route('**/*',route=>{
  const u=new URL(route.request().url());
  if(u.origin!==base){results.external.push(u.origin);return route.abort();}
  if(u.pathname.startsWith('/api/'))throw Error('Storage regression used observation API');
  return route.continue();
 });
 const retryPage=await retryContext.newPage();
 retryPage.on('pageerror',e=>results.pageErrors.push(e.message));
 await retryPage.goto(base+'/?source=demo');
 await retryPage.getByRole('button',{name:'시뮬레이션 시작',exact:true}).first().click();
 await retryPage.getByText('시뮬레이션 데이터',{exact:true}).waitFor();
 await retryPage.evaluate(()=>{
  window.__failHistoryWrites=true;
  const put=IDBObjectStore.prototype.put;
  IDBObjectStore.prototype.put=function(...args){
   const request=put.apply(this,args);
   if(this.transaction.db.name==='opsloop-simulation-history-v1'&&this.name==='history'&&window.__failHistoryWrites)
    request.addEventListener('success',()=>request.transaction.abort(),{once:true});
   return request;
  };
 });
 await retryPage.getByRole('button',{name:'시연 종료 · History 저장'}).click();
 await retryPage.getByRole('alert').getByText('기록을 저장하지 못했습니다.',{exact:false}).waitFor();
 if(new URL(retryPage.url()).searchParams.get('source')!=='demo')throw Error('Navigation before durable finalize');
 const rollback=await retryPage.evaluate(()=>new Promise((resolve,reject)=>{
  const request=indexedDB.open('opsloop-simulation-history-v1',1);
  request.onsuccess=()=>{const db=request.result,t=db.transaction(['history','pending']);let history,pending;
   t.objectStore('history').count().onsuccess=e=>{history=e.target.result;};
   t.objectStore('pending').count().onsuccess=e=>{pending=e.target.result;};
   t.oncomplete=()=>{db.close();resolve({history,pending});};t.onerror=reject;
  };request.onerror=reject;
 }));
 if(rollback.history!==0||rollback.pending!==1)throw Error('Finalize abort lost pending data: '+JSON.stringify(rollback));
 await retryPage.evaluate(()=>{window.__failHistoryWrites=false;});
 await retryPage.getByRole('button',{name:'시연 종료 · History 저장'}).click();
 await retryPage.waitForURL('**/?source=history');
 await retryPage.getByText('simulation · stopped',{exact:false}).waitFor();
 await retryPage.reload();await retryPage.getByText('simulation · stopped',{exact:false}).waitFor();
 const durable=await retryPage.evaluate(()=>new Promise((resolve,reject)=>{
  const request=indexedDB.open('opsloop-simulation-history-v1',1);
  request.onsuccess=()=>{const db=request.result,t=db.transaction(['history','pending']);let history,pending;
   t.objectStore('history').getAll().onsuccess=e=>{history=e.target.result;};
   t.objectStore('pending').count().onsuccess=e=>{pending=e.target.result;};
   t.oncomplete=()=>{db.close();resolve({history,pending});};t.onerror=reject;
  };request.onerror=reject;
 }));
 if(durable.history.length!==1||durable.history[0].completion!=='stopped'||durable.history[0].source!=='simulation'||durable.pending!==0)throw Error('Retry did not commit exactly once');
 results.checks.push({case:'IndexedDB-finalize-abort-visible-error-and-durable-retry',pass:true,rollback,committedHistory:durable.history.length,pendingAfterRetry:durable.pending});
 await retryContext.close();
 const broken=await browser.newContext();await broken.addInitScript(()=>Object.defineProperty(window,'indexedDB',{get(){throw Error('SYNTHETIC_STORAGE_BLOCK');}}));const page=await broken.newPage();await page.goto(server.url+'/?source=history');await page.getByRole('alert').getByText('브라우저 기록을 읽지 못했습니다.',{exact:false}).waitFor();results.checks.push({case:'blocked-storage-visible-error',pass:true});await broken.close();
 results.unexpectedConsoleErrors=results.consoleErrors.filter(x=>!x.includes('the server responded with a status of 503')&&!x.includes("Connecting to 'https://csp-blocked.invalid/status'")&&!x.includes('Fetch API cannot load https://csp-blocked.invalid/status'));if(results.pageErrors.length||results.external.length||results.unexpectedConsoleErrors.length)throw Error('Unexpected browser/network/console error');if(results.api.some(x=>x.path!=='/api/status'||x.method!=='GET'))throw Error('Browser used write/private API');
 results.pass=true;
}catch(e){results.error=String(e);process.exitCode=1;}
finally{await browser?.close();await server?.close();fs.rmSync(tmp,{recursive:true,force:true});fs.writeFileSync(path.join(out,'browser.json'),JSON.stringify(results,null,2));console.log(JSON.stringify({pass:results.pass,checks:results.checks.length,error:results.error,pageErrors:results.pageErrors,external:results.external}));}
