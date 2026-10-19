import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawnSync} from 'node:child_process';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
function fixture(fn){
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'opsloop-public-gate-'));
 try {
  for(const file of ['scripts/verify-public-evidence.mjs','src/public/evidence-boundary.json','src/public/evidence.public.json','src/data/replay/evidence.json']){
   fs.mkdirSync(path.dirname(path.join(dir,file)),{recursive:true});fs.copyFileSync(path.join(root,file),path.join(dir,file));
  }
  fn(dir);
 }finally{fs.rmSync(dir,{recursive:true});}
}
function verify(dir){return spawnSync(process.execPath,['scripts/verify-public-evidence.mjs'],{cwd:dir,encoding:'utf8'}).status;}
test('reviewed public derivative accepted',()=>fixture(dir=>assert.equal(verify(dir),0)));
test('unreviewed source schema/field change rejected',()=>fixture(dir=>{
 const file=path.join(dir,'src/data/replay/evidence.json'),data=JSON.parse(fs.readFileSync(file));data.unreviewedField='not-a-secret';fs.writeFileSync(file,JSON.stringify(data));assert.notEqual(verify(dir),0);
}));
test('changed published classification/decision rejected',()=>fixture(dir=>{
 const file=path.join(dir,'src/public/evidence.public.json'),data=JSON.parse(fs.readFileSync(file));data.experiments[0].frames[1].snapshot.recentIncidents[0].status.runtime.phase='RECOVERED';fs.writeFileSync(file,JSON.stringify(data));assert.notEqual(verify(dir),0);
}));

function artifactFixture(fn){fixture(dir=>{
 for(const file of ['scripts/audit-public.mjs','wrangler.public.json','public-static/404.html','public-static/_headers']){
  fs.mkdirSync(path.dirname(path.join(dir,file)),{recursive:true});fs.copyFileSync(path.join(root,file),path.join(dir,file));
 }
 fs.writeFileSync(path.join(dir,'.public-build-audit.json'),JSON.stringify({profile:'public',envFilesLoaded:false,liveAdapterIncluded:false,modules:['src/public/main.tsx','src/public/evidence.public.json']}));
 fs.mkdirSync(path.join(dir,'dist-public'));fs.writeFileSync(path.join(dir,'dist-public/index.html'),'<!doctype html><title>Public test</title>');
 for(const file of ['404.html','_headers'])fs.copyFileSync(path.join(dir,'public-static',file),path.join(dir,'dist-public',file));
 fn(dir);
});}
function audit(dir){return spawnSync(process.execPath,['scripts/audit-public.mjs'],{cwd:dir,encoding:'utf8'}).status;}
test('artifact accepts static allowlist and rejects unexpected environment file',()=>artifactFixture(dir=>{
 assert.equal(audit(dir),0);fs.writeFileSync(path.join(dir,'dist-public/.env.local'),'not-a-secret');assert.notEqual(audit(dir),0);
}));
test('artifact rejects unreviewed public input directory file',()=>artifactFixture(dir=>{
 fs.writeFileSync(path.join(dir,'public-static/extra.json'),'{}');assert.notEqual(audit(dir),0);
}));
