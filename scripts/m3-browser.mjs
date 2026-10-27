// Real local Chrome rendering; only UI failure tests alter responses, labelled below.
import { chromium } from '../worker/node_modules/playwright-core/index.mjs';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

const option = name => { const i = process.argv.indexOf(name); return i < 0 ? undefined : process.argv[i+1]; };
const base = option('--base') ?? process.env.M3_URL ?? 'http://127.0.0.1:4173';
const out = path.resolve(option('--output') ?? process.env.M3_BROWSER_OUT ?? 'reports/m3/dashboard');
const headed = process.argv.includes('--headed') || process.env.M3_HEADED === '1';
const replayOnly = process.argv.includes('--replay-only');
fs.mkdirSync(out, {recursive:true});
const receipt = {schema:'opsloop-m3-browser/v1', observedAt:new Date().toISOString(), browser:'installed Chrome', checks:[], requests:[], errors:[], screenshots:[], status:'FAIL'};
let browser;
const check = (name, pass, detail={}) => { receipt.checks.push({name, pass, ...detail}); if(!pass) throw Error(name); };
try {
  browser = await chromium.launch({executablePath:process.env.M3_CHROME ?? '/opt/google/chrome/chrome', headless:!headed, args:['--no-sandbox','--disable-dev-shm-usage']});
  receipt.browserVersion = browser.version();
  receipt.headed = headed;
  receipt.replayOnly = replayOnly;
  const context = await browser.newContext({viewport:{width:1366,height:768},serviceWorkers:'block'});
  await context.route('**/*', route => {
    const request = route.request(); const url = new URL(request.url());
    if(url.origin !== base) { receipt.errors.push('unexpected external request '+url.origin); return route.abort(); }
    receipt.requests.push({method:request.method(),path:url.pathname});
    return route.continue();
  });
  const page = await context.newPage();
  page.on('pageerror', e => receipt.errors.push(e.message));
  async function shot(name) { const file=name+'.png'; await page.screenshot({path:path.join(out,file),fullPage:true}); receipt.screenshots.push(file); }
  await page.goto(base+'/?source=exhibition');
  await page.getByRole('heading',{name:'두 복구 루프, 하나의 증거 흐름.'}).waitFor();
  check('exhibition-real-record-default', await page.getByText('PR_CREATED',{exact:true}).isVisible());
  await shot('1366-overview');
  for(const [run,view,count] of [['demo-a','runtime',8],['demo-b','engineering',7]]) {
    await page.goto(base+`/?source=replay&view=${view}&run=${run}`);
    const timeline = page.getByRole('region',{name:'실행 이벤트 타임라인'});
    const buttons = timeline.getByRole('button');
    check(run+'-event-count', await buttons.count()===count);
    for(let i=0;i<count;i++) {await buttons.nth(i).click();check(run+'-event-'+i, await page.getByRole('region',{name:'선택한 이벤트'}).isVisible());}
    await shot('1366-'+view+'-final');
    await buttons.first().click();
    await page.getByRole('button',{name:'기록 재생',exact:true}).click();
    await page.waitForTimeout(5300);
    check(run+'-play-advances', await page.getByRole('slider').inputValue()==='1');
    await page.getByRole('button',{name:'일시 정지',exact:true}).click();
    await page.getByRole('slider').fill(String(count-1));
    if(run==='demo-b') {
      check('pr-link',await page.locator('a[href="https://github.com/SleepyMiny/OpsLoop/pull/6"]').count()===1);
      check('pr-not-deployed',await page.getByText('NOT_DEPLOYED',{exact:true}).isVisible());
      check('service-not-recovered',await page.getByText('복구되지 않음',{exact:true}).isVisible());
    }
  }
  const catalog=JSON.parse(fs.readFileSync('frontend/dashboard/src/data/m3/evidence.json','utf8'));
  const sources=new Map(catalog.runs.flatMap(r=>r.events.flatMap(e=>e.sources)).map(s=>[s.href,s]));
  for(const [href,s] of sources) {
    const response=await context.request.get(base+'/'+href);
    check('source-link-'+href,response.status()===200 && crypto.createHash('sha256').update(await response.body()).digest('hex')===(s.downloadSha256??s.sha256));
  }
  check('replay-zero-observation-requests',receipt.requests.every(r=>!r.path.startsWith('/api/')));
  await context.setOffline(true);
  await page.getByRole('slider').fill('0');
  await page.getByRole('button',{name:'다음 →'}).click();
  check('offline-loaded-replay-seek',await page.getByRole('slider').inputValue()==='1');
  await context.setOffline(false);
  for(const viewport of [{width:1920,height:1080},{width:390,height:844}]) {
    await page.setViewportSize(viewport);
    await page.goto(base+'/?source=replay&view=runtime');
    check('no-page-overflow-'+viewport.width,await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
    await shot(viewport.width+'-runtime');
  }
  await page.setViewportSize({width:1366,height:768});
  if(!replayOnly) {
  const snapshots={};
  for(const scope of ['a','b']) {
    const response=await context.request.get(base+`/api/${scope}/api/v1/snapshot`);
    check('live-'+scope+'-api',response.status()===200);
    snapshots[scope]=await response.json();
    fs.writeFileSync(path.join(out,'live-'+scope+'-snapshot.json'),JSON.stringify(snapshots[scope],null,2)+'\n');
    await page.goto(base+`/?source=live&scope=${scope}`);
    await page.getByText('연결됨',{exact:true}).waitFor({timeout:15000});
    check('live-'+scope+'-namespace',await page.getByText(`opsloop-demo-${scope}/${scope==='a'?'demo-api':'engineering-demo'}`,{exact:true}).count()>0);
    check('live-'+scope+'-observed-nodes',snapshots[scope].cluster.nodes.length===3);
    if(snapshots[scope].workload.platforms.length===0) {
      check('live-'+scope+'-unknown-platform-is-not-mismatch',await page.getByText('이미지 플랫폼 manifest를 조회하지 않아 현재 후보 적격 여부는 미확인입니다.',{exact:false}).isVisible());
    }
    await shot('1366-live-'+scope);
    if(scope==='b' && process.argv.includes('--engineering')) {
      check('live-engineering-exact-run-binding',snapshots.b.engineeringRuns?.length===1 && snapshots.b.engineeringRuns[0].runtimeRunID==='demo-b-empty-order-1' && snapshots.b.engineeringRuns[0].epoch==='demo-b-epoch-1');
      await page.getByRole('tab',{name:'Engineering',exact:true}).click();
      await page.getByText('PR_CREATED',{exact:true}).first().waitFor();
      check('live-history-pr-link',await page.locator('a[href="https://github.com/SleepyMiny/OpsLoop/pull/6"]').count()===1);
      check('live-history-marked-as-history',await page.getByText('종료된 Incident 이력:',{exact:false}).count()>0);
      await shot('1366-live-engineering-history');
    }
  }
  // Synthetic response timing/failure only; never recorded as cluster observation.
  await page.route('**/api/b/api/v1/snapshot',route=>route.fulfill({json:{...snapshots.b,observedAt:Date.now()-60000}}));
  await page.goto(base+'/?source=live&scope=b');
  await page.getByText('STALE · 데이터 지연',{exact:true}).waitFor();
  check('synthetic-stale-ui',true,{scope:'UI-only response timestamp override'});
  await shot('1366-ui-stale-test');
  await page.unroute('**/api/b/api/v1/snapshot');
  await page.route('**/api/b/api/v1/snapshot',route=>route.abort());
  await page.goto(base+'/?source=live&scope=b');
  await page.getByText('DISCONNECTED · 연결 끊김',{exact:true}).waitFor();
  check('synthetic-disconnected-ui',true,{scope:'UI-only request abortion'});
  await shot('1366-ui-disconnected-test');
  }
  check('all-browser-requests-read-only',receipt.requests.every(r=>r.method==='GET'));
  check('no-browser-errors',receipt.errors.length===0);
  receipt.status='PASS';
  await context.close();
} catch(error) { receipt.errors.push(String(error)); process.exitCode=1; }
finally {
  await browser?.close();
  receipt.completedAt=new Date().toISOString();
  receipt.kubernetesMutationRequests=0;
  fs.writeFileSync(path.join(out,'verification.json'),JSON.stringify(receipt,null,2)+'\n');
  console.log(JSON.stringify({status:receipt.status,checks:receipt.checks.length,errors:receipt.errors,output:out}));
}
