// Local synthetic UI checks only. Every API route is intercepted; no cluster/model access.
import fs from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createServer} from '../../node_modules/vite/dist/node/index.js';
import react from '../../node_modules/@vitejs/plugin-react/dist/index.js';

if (!process.env.M3_PLAYWRIGHT_MODULE) throw Error('Explicit existing Playwright module path required');
const {chromium} = await import(process.env.M3_PLAYWRIGHT_MODULE);
const dashboard = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const out = path.resolve(process.env.M3_BROWSER_OUT ?? path.join(dashboard, '../../.opsloop/live-integration/repeatability-ui'));
fs.mkdirSync(out, {recursive: true});
const receipt = {scope: 'LOCAL_SYNTHETIC_UI_ONLY_NO_CLUSTER_OR_MODEL', checks: [], errors: []};
const check = (name, pass) => {receipt.checks.push({name, pass}); if (!pass) throw Error(name);};
const flags = (complete) => ({stopConfirmed: complete, modelStopped: complete, baselineRestored: complete, evidenceArchived: complete, objectsCleaned: complete, reasonCode: complete ? 'COMPLETE' : 'LOCAL_SYNTHETIC_PENDING'});
let server, browser;
try {
  server = await createServer({configFile: false, root: dashboard, plugins: [react()], server: {host: '127.0.0.1', port: 0, strictPort: true}});
  await server.listen();
  const base = `http://127.0.0.1:${server.httpServer.address().port}`;
  browser = await chromium.launch({executablePath: '/opt/google/chrome/chrome', headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage']});
  for (const track of ['runtime', 'engineering']) {
    const context = await browser.newContext({viewport: {width: 1366, height: 900}});
    const binding = (id) => ({runID: `${track}-${id}`, epoch: `${track}-epoch-${id}`, policyUID: `${track}-fixed-policy`, deploymentUID: `${track}-fixed-workload`});
    const first = {runId: `${track}-1`, planId: `${track}-plan-1`, track, state: 'STOPPED', source: 'synthetic', binding: binding(1), lastSeq: 1, stopConfirmed: true, lifecycleComplete: false, lifecycle: flags(false), lifecycleReceiptSHA256: null};
    let active = first, runs = [first], allowed = false;
    const plan = {planId: `${track}-plan-2`, track, expiresAt: '2099-01-01T00:00:00Z', source: 'synthetic', binding: binding(2), ready: false, totalSeconds: 300, ...(track === 'engineering' ? {model: 'gpt-6-luna', maxModelCalls: 1} : {})};
    const posts = [];
    const events = (id) => [{runId: id, seq: 1, at: '2026-10-28T00:00:00Z', actor: 'coordinator', kind: 'STOP_CONFIRMED', source: 'synthetic', data: {reasonCode: 'LOCAL_SYNTHETIC_ONLY'}}];
    await context.route('**/*', async route => {
      const request = route.request(), url = new URL(request.url());
      if (url.origin !== base) {receipt.errors.push('external request refused'); return route.abort();}
      if (url.pathname === '/api/exhibition/capabilities') return route.fulfill({json: {live: true, observationConfigured: false, control: true, mode: 'live', csrfToken: 'synthetic-only'}});
      if (url.pathname === '/api/exhibition/state') return route.fulfill({json: {active, runs, plans: [plan], nextRunAllowed: allowed}});
      if (url.pathname === '/api/exhibition/events') return route.fulfill({json: {events: Number(url.searchParams.get('after')) === 0 ? events(url.searchParams.get('runId')) : []}});
      if (url.pathname === '/api/exhibition/artifacts') return route.fulfill({json: {artifacts: []}});
      if (request.method() === 'POST' && url.pathname.startsWith('/api/exhibition/')) {
        const body = request.postDataJSON(); posts.push({path: url.pathname, body});
        if (url.pathname.endsWith('/reconcile')) {
          check(`${track}:cleanup-only-run-id`, Object.keys(body).join() === 'runId' && body.runId === active.runId);
          active.state = 'RECONCILING';
        } else if (url.pathname.endsWith('/prepare')) {
          check(`${track}:prepare-after-completion`, allowed && active === null);
          plan.ready = true;
        } else if (url.pathname.endsWith('/start')) {
          check(`${track}:start-new-approved-binding`, allowed && plan.ready && body.planId === plan.planId && typeof body.requestId === 'string');
          active = {runId: `${track}-2`, planId: plan.planId, track, source: 'synthetic', binding: binding(2), state: 'RUNNING', stopConfirmed: false, lastSeq: 1, lifecycleComplete: false, lifecycle: flags(false), lifecycleReceiptSHA256: null};
          runs = [active, ...runs]; allowed = false;
        } else return route.fulfill({status: 400, json: {error: 'UNEXPECTED_LOCAL_TEST_ACTION'}});
        return route.fulfill({json: active ?? {prepared: plan.planId}});
      }
      if (url.pathname.startsWith('/api/')) {receipt.errors.push('unexpected API refused'); return route.abort();}
      return route.continue();
    });
    const page = await context.newPage(); page.on('pageerror', error => receipt.errors.push(error.message));
    await page.goto(base);
    await page.getByText('실행 원장 연결됨', {exact: true}).waitFor();
    check(`${track}:stop-alone-blocks-next`, await page.getByRole('button', {name: '다음 시연 준비'}).isDisabled() && await page.getByRole('button', {name: '승인된 실험 시작'}).isDisabled());
    check(`${track}:no-auto-action`, posts.length === 0);
    await page.reload(); await page.getByText('실행 원장 연결됨', {exact: true}).waitFor();
    check(`${track}:reload-does-not-reexecute`, posts.length === 0);
    first.state = 'UNCONFIRMED_TERMINATION'; first.stopConfirmed = false;
    await page.getByText('이전 실행의 종료가 불명확합니다.', {exact: false}).waitFor();
    await page.getByRole('button', {name: '종료 · 복원 · 정리 확인', exact: true}).evaluate(button => {button.click(); button.click();});
    await page.getByRole('button', {name: '종료 · 정리 확인 중'}).waitFor();
    check(`${track}:reconcile-no-start-or-duplicate`, posts.length === 1 && posts[0].path.endsWith('/reconcile'));
    check(`${track}:reconciling-blocks-next`, await page.getByRole('button', {name: '다음 시연 준비'}).isDisabled());
    await page.screenshot({path: path.join(out, `${track}-cleanup-blocked.png`), fullPage: true});
    // Explicit new server receipt: no timer invents a completed stage in the product.
    Object.assign(first, {state: 'STOPPED', stopConfirmed: true, lifecycleComplete: true, lifecycle: flags(true), lifecycleReceiptSHA256: 'a'.repeat(64)});
    active = null; allowed = true;
    await page.getByText('이전 실행의 종료 절차가 확인됐습니다.', {exact: false}).waitFor();
    check(`${track}:completion-releases-preparation`, await page.getByRole('button', {name: '다음 시연 준비'}).isEnabled());
    check(`${track}:preparation-still-required`, await page.getByRole('button', {name: '승인된 실험 시작'}).isDisabled());
    await page.getByRole('button', {name: '다음 시연 준비'}).click();
    await page.waitForFunction(() => ![...document.querySelectorAll('button')].find(b => b.textContent === '승인된 실험 시작').disabled);
    await page.getByRole('button', {name: '승인된 실험 시작'}).evaluate(button => {button.click(); button.click();});
    await page.waitForFunction(() => [...document.querySelectorAll('strong')].some(e => e.textContent === 'RUNNING'));
    check(`${track}:one-new-run-same-policy`, posts.filter(p => p.path.endsWith('/start')).length === 1 && runs[0].binding.policyUID === runs[1].binding.policyUID && runs[0].binding.runID !== runs[1].binding.runID);
    await page.getByText('실행 기록 · Replay · 공개 근거', {exact: false}).click();
    await page.getByLabel('현재 실행 / 완료된 Replay').selectOption(first.runId);
    await page.getByRole('button', {name: '이 실행 Replay 보기'}).waitFor();
    check(`${track}:prior-replay-retained`, await page.getByRole('list', {name: 'run 이벤트 순서'}).getByRole('listitem').count() === 1);
    await page.setViewportSize({width: 390, height: 844});
    check(`${track}:mobile-contained`, await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.screenshot({path: path.join(out, `${track}-repeat-mobile.png`), fullPage: true});
    await context.close();
  }
  check('no-browser-errors', receipt.errors.length === 0);
} catch (error) {
  receipt.errors.push(error.message); process.exitCode = 1;
} finally {
  await browser?.close(); await server?.close();
  fs.writeFileSync(path.join(out, 'verification.json'), JSON.stringify(receipt, null, 2));
  console.log(JSON.stringify(receipt));
}
