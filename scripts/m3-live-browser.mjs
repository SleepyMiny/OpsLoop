// Local UI contract test. All API replies are synthetic; no cluster/model requests.
const { chromium } = await import(process.env.M3_PLAYWRIGHT_MODULE ?? '../worker/node_modules/playwright-core/index.mjs');
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
const base = process.env.M3_URL ?? 'http://127.0.0.1:4195';
const out = path.resolve(process.env.M3_BROWSER_OUT ?? '.opsloop/live-ui-browser');
fs.mkdirSync(out,{recursive:true});
const receipt={scope:'LOCAL_SYNTHETIC_UI_ONLY',checks:[],errors:[]};
const check=(name,pass)=>{receipt.checks.push({name,pass});if(!pass)throw Error(name);};
let browser;
try {
  browser=await chromium.launch({executablePath:'/opt/google/chrome/chrome',headless:true,args:['--no-sandbox','--disable-dev-shm-usage']});
  const {createServer} = await import('../frontend/dashboard/node_modules/vite/dist/node/index.js');
  const fixtureServer = await createServer({root:path.resolve('frontend/dashboard'),server:{middlewareMode:true},appType:'custom'});
  let fixture;
  try {
    const {DemoSimulation} = await fixtureServer.ssrLoadModule('/src/data/demo/engine.ts');
    const {DEMO_SCENARIOS} = await fixtureServer.ssrLoadModule('/src/data/demo/scenarios.ts');
    const simulation = new DemoSimulation(DEMO_SCENARIOS.find(s=>s.id==='node-failure-arm64').script);
    simulation.advanceTo(100_000);
    fixture=simulation.snapshot();
    // Shift the fixture clock as one unit; these remain synthetic samples.
    const shift=Date.now()-fixture.observedAt;
    const shiftTimes=value=>{
      if(Array.isArray(value))return value.map(shiftTimes);
      if(value&&typeof value==='object')return Object.fromEntries(Object.entries(value).map(([key,entry])=>[key,typeof entry==='number'&&/(At$|^at$|^until$)/.test(key)?entry+shift:shiftTimes(entry)]));
      return value;
    };
    fixture=shiftTimes(fixture);
    fixture.cluster.name='LOCAL_SYNTHETIC_BROWSER_FIXTURE';
    fixture.diagnostics={warnings:['개발용 합성 snapshot · 실제 클러스터 관측 아님'],metricsAvailable:true,readOnly:true};
  } finally {await fixtureServer.close();}
  const context=await browser.newContext({viewport:{width:1366,height:768}});
  let mode='live';let posts=[];let observationRequests=0;let run=null;let events=[];let corruptArtifact=false;let connectedSnapshot=false;
  const artifactContent='<script>window.opsloopInjected=true</script>\n--- a/discount.go\n+++ b/discount.go';
  const binding={runID:'browser-run-1',epoch:'browser-epoch-1',policyUID:'p1',deploymentUID:'d1'};
  const plan={planId:'browser-plan-1',track:'runtime',expiresAt:'2099-01-01T00:00:00Z',source:'synthetic',binding,ready:true,totalSeconds:30};
  await context.route('**/*',async route=>{
    const request=route.request(),url=new URL(request.url());
    if(url.origin!==base){receipt.errors.push('external request');return route.abort();}
    if(url.pathname.endsWith('/api/v1/snapshot')){observationRequests++;return connectedSnapshot ? route.fulfill({json:{...fixture,observedAt:Date.now()}}) : route.fulfill({status:503,json:{error:'LOCAL_TEST_OBSERVATION_DISCONNECTED'}});}
    if(url.pathname==='/api/exhibition/capabilities')return route.fulfill({json:{live:mode==='live',control:mode==='live',mode,csrfToken:'browser-only-token'}});
    if(url.pathname==='/api/exhibition/state')return route.fulfill({json:{active:run?.lifecycleComplete?null:run,runs:run?[run]:[],plans:[plan],nextRunAllowed:!run||run.lifecycleComplete===true}});
    if(url.pathname==='/api/exhibition/artifacts')return route.fulfill({json:{artifacts:[{kind:'diff',sha256:corruptArtifact?'0'.repeat(64):crypto.createHash('sha256').update(artifactContent).digest('hex'),content:artifactContent}]}});
    if(url.pathname==='/api/exhibition/events')return route.fulfill({json:{events:events.filter(e=>e.seq>Number(url.searchParams.get('after')))}});
    if(url.pathname.startsWith('/api/exhibition/')&&request.method()==='POST'){
      posts.push({path:url.pathname,body:request.postDataJSON(),csrf:request.headers()['x-opsloop-csrf']});
      if(url.pathname.endsWith('/start')){
        run={runId:'browser-run-1',planId:plan.planId,track:'runtime',state:'RUNNING',source:'synthetic',binding,lastSeq:1,stopConfirmed:false,lifecycleComplete:false,lifecycle:{stopConfirmed:false,baselineRestored:false,evidenceArchived:false,objectsCleaned:false,modelStopped:false,reasonCode:'LOCAL_SYNTHETIC_PENDING'},lifecycleReceiptSHA256:null};
        events=[{runId:run.runId,seq:1,at:'2026-10-28T00:00:00Z',actor:'coordinator',kind:'BASELINE_VERIFYING',source:'synthetic',data:{status:'LOCAL_TEST_ONLY'}}];
      }
      if(url.pathname.endsWith('/stop')){const seq=events.length+1;run={...run,state:'STOPPED',stopConfirmed:true,lastSeq:seq,lifecycle:{...run.lifecycle,stopConfirmed:true}};events.push({...events[0],seq,kind:'STOP_CONFIRMED'});}
      return route.fulfill({json:{ok:true}});
    }
    return route.continue();
  });
  const page=await context.newPage();page.on('pageerror',e=>receipt.errors.push(e.message));
  await page.goto(base);
  await page.getByRole('heading',{name:'Live 관제 · 승인된 실행'}).waitFor();
  await page.getByText('실행 원장 연결됨',{exact:true}).waitFor();
  check('default-live-without-auto-start',posts.length===0);
  await page.getByRole('heading',{name:'관측 API에 연결하지 못했습니다'}).waitFor();
  check('transport-failure-not-service-failure',await page.getByText('관측 연결 실패는 실험 대상 서비스의 장애 판정이 아닙니다.',{exact:false}).isVisible());
  await page.getByRole('button',{name:'승인된 실험 시작'}).evaluate(button=>{button.click();button.click();});
  await page.getByText('실행 기록 · Replay · 공개 근거',{exact:false}).click();
  await page.getByRole('button',{name:'최신 이벤트 따라가기'}).waitFor();
  check('double-click-single-start',posts.filter(p=>p.path.endsWith('/start')).length===1);
  check('mutation-csrf-and-plan-only',posts.every(p=>p.csrf==='browser-only-token'&&Object.keys(p.body).every(k=>['planId','requestId','runId'].includes(k))));
  await page.reload();await page.getByText('실행 기록 · Replay · 공개 근거',{exact:false}).click();await page.getByRole('button',{name:'최신 이벤트 따라가기'}).waitFor();
  check('reload-reconnect-no-restart',posts.length===1);
  const count=await page.getByRole('list',{name:'run 이벤트 순서'}).getByRole('listitem').count();
  await page.waitForTimeout(1600);
  check('no-timer-progress',await page.getByRole('list',{name:'run 이벤트 순서'}).getByRole('listitem').count()===count);
  check('synthetic-label',await page.getByText('개발용 합성 · 실환경 성공 증거 아님',{exact:false}).count()>0);
  await page.getByText('공개 승인된 코드 diff · 테스트 결과',{exact:true}).click();
  await page.getByRole('button',{name:'이 run의 공개 근거 조회'}).click();
  await page.getByRole('heading',{name:'검토된 코드 diff'}).waitFor();
  check('artifact-text-never-html',await page.evaluate(()=>window.opsloopInjected===undefined));
  check('artifact-hash-displayed',await page.getByText(artifactContent,{exact:true}).isVisible());
  corruptArtifact=true;
  await page.getByRole('button',{name:'이 run의 공개 근거 조회'}).click();
  await page.getByText('공개 근거를 검증하지 못했습니다.',{exact:false}).waitFor();
  check('artifact-hash-mismatch-hidden',await page.getByRole('heading',{name:'검토된 코드 diff'}).count()===0);
  await page.screenshot({path:path.join(out,'live-contract-desktop.png'),fullPage:true});
  connectedSnapshot=true;
  await page.reload();
  await page.getByRole('heading',{name:'Hybrid k3s 클러스터',exact:true}).waitFor();
  await page.getByText('연결됨',{exact:true}).waitFor();
  const clusterBox=await page.getByRole('heading',{name:'Hybrid k3s 클러스터',exact:true}).boundingBox();
  receipt.topologyTop=clusterBox.y;
  check('connected-topology-above-fold',clusterBox.y<530);
  check('connected-controls-visible',await page.getByRole('button',{name:'현재 실행 중지'}).isVisible());
  check('evidence-collapsed-by-default',await page.getByRole('list',{name:'run 이벤트 순서'}).isVisible()===false);
  check('synthetic-connected-identity',await page.getByText('LOCAL_SYNTHETIC_BROWSER_FIXTURE',{exact:true}).count()>0);
  check('connected-no-horizontal-overflow',await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.screenshot({path:path.join(out,'live-connected-1366x768.png'),fullPage:false});
  await page.getByText('실행 기록 · Replay · 공개 근거',{exact:false}).click();
  events.push({...events[0],seq:2,actor:'verifier',kind:'VERIFICATION_RESULT',data:{accepted:true,baseline:[{name:'empty-order-regression',passed:false,httpStatus:500,inputSHA256:'a'.repeat(64)}],candidate:[{name:'empty-order-regression',passed:true,httpStatus:200,inputSHA256:'a'.repeat(64)}]}});
  await page.getByText('동일 입력 SHA-256 확인됨',{exact:true}).waitFor();
  check('same-input-before-after-visible',await page.getByText('FAIL · HTTP 500',{exact:true}).isVisible()&&await page.getByText('PASS · HTTP 200',{exact:true}).isVisible());
  await page.setViewportSize({width:390,height:844});
  check('comparison-mobile-contained',await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.screenshot({path:path.join(out,'same-input-mobile.png'),fullPage:true});
  events.push({...events[0],seq:3,actor:'verifier',kind:'VERIFICATION_RESULT',data:{accepted:true,baseline:[{name:'empty-order-regression',passed:false,inputSHA256:'a'.repeat(64)}],candidate:[{name:'empty-order-regression',passed:true,inputSHA256:'b'.repeat(64)}]}});
  await page.getByText('동일 입력 연결 미확인',{exact:false}).waitFor();
  check('different-input-never-success',await page.getByText('동일 입력 SHA-256 확인됨',{exact:true}).count()===0);
  await page.setViewportSize({width:1366,height:768});
  await page.getByRole('button',{name:'현재 실행 중지'}).click();
  await page.getByText('REPLAY · 종료된 실행 기록',{exact:false}).waitFor();
  check('stop-replays-same-run',posts.filter(p=>p.path.endsWith('/stop')).length===1);
  await page.getByRole('heading',{name:'Hybrid k3s 클러스터',exact:true}).waitFor();
  check('completion-keeps-live-topology',await page.getByText('완료된 run의 기록을 재생 중입니다.',{exact:false}).count()===0);
  await page.getByRole('button',{name:'이 실행 Replay 보기'}).click();
  check('replay-hides-current-topology',await page.getByText('완료된 run의 기록을 재생 중입니다.',{exact:false}).isVisible());
  await page.setViewportSize({width:390,height:844});
  check('mobile-no-overflow',await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  await page.screenshot({path:path.join(out,'live-contract-mobile.png'),fullPage:true});
  await page.getByRole('button',{name:'현재 Live 관측으로 돌아가기'}).click();
  await page.getByRole('heading',{name:'Hybrid k3s 클러스터',exact:true}).waitFor();
  check('explicit-return-to-live',await page.getByText('완료된 run의 기록을 재생 중입니다.',{exact:false}).count()===0);
  await page.reload();
  await page.getByText('실행 원장 연결됨',{exact:true}).waitFor();
  await page.getByRole('heading',{name:'Hybrid k3s 클러스터',exact:true}).waitFor();
  check('completed-run-reload-defaults-live',await page.getByText('완료된 run의 기록을 재생 중입니다.',{exact:false}).count()===0);
  mode='replay';observationRequests=0;
  await page.goto(base+'/?source=live');
  await page.getByRole('heading',{name:'두 복구 루프, 하나의 증거 흐름.'}).waitFor();
  await page.waitForTimeout(1200);
  check('replay-server-no-live-poll',observationRequests===0);
  check('replay-server-live-disabled',await page.locator('nav[aria-label="관측 모드"] a[aria-disabled="true"]').count()===1);
  check('no-browser-errors',receipt.errors.length===0);
  receipt.status='PASS';await context.close();
} catch(error) {receipt.errors.push(String(error));receipt.status='FAIL';process.exitCode=1;}
finally {await browser?.close();fs.writeFileSync(path.join(out,'verification.json'),JSON.stringify(receipt,null,2)+'\n');console.log(JSON.stringify(receipt));}
