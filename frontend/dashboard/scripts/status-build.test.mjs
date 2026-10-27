import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawnSync} from 'node:child_process';
const source=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
function fixture(change){
 const temp=fs.mkdtempSync(path.join(os.tmpdir(),'opsloop-status-artifact-')),root=path.join(temp,'frontend/dashboard');
 try{
  fs.mkdirSync(path.join(root,'scripts'),{recursive:true});fs.mkdirSync(path.join(root,'dist-status/assets'),{recursive:true});fs.mkdirSync(path.join(root,'status-static'));fs.mkdirSync(path.join(temp,'worker'));
  fs.copyFileSync(path.join(source,'scripts/audit-status.mjs'),path.join(root,'scripts/audit-status.mjs'));
  fs.copyFileSync(path.join(source,'../../worker/wrangler.status.json'),path.join(temp,'worker/wrangler.status.json'));
  fs.writeFileSync(path.join(root,'.status-build-audit.json'),JSON.stringify({profile:'status',modules:['src/status/main.tsx','src/public/evidence.public.json']}));
  fs.writeFileSync(path.join(root,'dist-status/index.html'),'<!doctype html><title>Synthetic artifact</title>');
  for(const f of ['404.html','_headers']){fs.copyFileSync(path.join(source,'status-static',f),path.join(root,'status-static',f));fs.copyFileSync(path.join(source,'status-static',f),path.join(root,'dist-status',f));}
  change(root,temp);
 }finally{fs.rmSync(temp,{recursive:true});}
}
const audit=root=>spawnSync(process.execPath,['scripts/audit-status.mjs'],{cwd:root,encoding:'utf8'}).status;
test('status output accepts only reviewed static assets',()=>fixture(root=>assert.equal(audit(root),0)));
test('status output rejects a hidden development environment file',()=>fixture(root=>{fs.writeFileSync(path.join(root,'dist-status/.env.local'),'SYNTHETIC_ONLY');assert.notEqual(audit(root),0);}));
test('status module graph rejects private Live adapter',()=>fixture(root=>{const p=path.join(root,'.status-build-audit.json'),g=JSON.parse(fs.readFileSync(p));g.modules.push('src/data/live/httpSource.ts');fs.writeFileSync(p,JSON.stringify(g));assert.notEqual(audit(root),0);}));
test('status output rejects server signing code and private sentinel',()=>fixture(root=>{fs.writeFileSync(path.join(root,'dist-status/assets/invalid.js'),'INGEST_KEYS = "SYNTHETIC_ONLY"');assert.notEqual(audit(root),0);}));
test('status input rejects unreviewed files before publishing',()=>fixture(root=>{fs.writeFileSync(path.join(root,'status-static/unknown.json'),'{}');assert.notEqual(audit(root),0);}));
test('status routing rejects unreviewed worker bindings',()=>fixture((root,temp)=>{const p=path.join(temp,'worker/wrangler.status.json'),c=JSON.parse(fs.readFileSync(p));c.vars.INGEST_KEYS='SYNTHETIC_ONLY';fs.writeFileSync(p,JSON.stringify(c));assert.notEqual(audit(root),0);}));
test('status headers reject loss of same-origin CSP',()=>fixture(root=>{fs.writeFileSync(path.join(root,'dist-status/_headers'),"/*\n  Content-Security-Policy: connect-src *\n");assert.notEqual(audit(root),0);}));
