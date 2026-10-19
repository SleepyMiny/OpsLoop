import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const sha=(b)=>crypto.createHash('sha256').update(b).digest('hex');
const controls=fs.readdirSync(path.join(root,'public-static')).sort();
if(JSON.stringify(controls)!==JSON.stringify(['404.html','_headers']))throw Error('Unreviewed public-static file');
const config=JSON.parse(fs.readFileSync(path.join(root,'wrangler.public.json'),'utf8'));
if(JSON.stringify(Object.keys(config).sort())!==JSON.stringify(['assets','compatibility_date','name']) || config.assets.directory!=='./dist-public' || config.assets.not_found_handling!=='none' || Object.keys(config.assets).length!==2)throw Error('Static-only Wrangler boundary changed');
const graph=JSON.parse(fs.readFileSync(path.join(root,'.public-build-audit.json'),'utf8'));
if(graph.profile!=='public'||graph.envFilesLoaded!==false||graph.liveAdapterIncluded!==false)throw Error('Public build module evidence missing');
if(!['src/public/main.tsx','src/public/evidence.public.json'].every((id)=>graph.modules.includes(id)) || graph.modules.some((id)=>/^src\/data\/(createDataSource\.ts|live\/httpSource\.ts|replay\/(replaySource\.ts|evidence\.json))$/.test(id)))throw Error('Invalid public import graph');
const files=[];
function walk(dir){for(const ent of fs.readdirSync(dir,{withFileTypes:true})){
 const p=path.join(dir,ent.name),rel=path.relative(path.join(root,'dist-public'),p).replaceAll('\\','/');
 if(ent.isSymbolicLink())throw Error('Symlink in public output');
 if(ent.isDirectory()){if(rel!=='assets')throw Error('Unexpected output directory');walk(p);continue;}
 if(!['index.html','404.html','_headers'].includes(rel)&&!/^assets\/[A-Za-z0-9_.-]+\.(js|css|woff2?)$/.test(rel))throw Error('Unexpected public output file');
 const b=fs.readFileSync(p);if(/\.(js|css|html)$/.test(rel)){
  const text=b.toString('utf8');
  if(/sarmubuntuserver|opsloop-control|opsloop-azure-worker|\.azurecr\.io|\.duckdns\.org|\b(?:100\.82\.|100\.73\.|100\.102\.|192\.168\.|10\.43\.)|\/home\/|\/tmp\/|\/api\/v1\/snapshot|BEGIN (?:RSA |OPENSSH )?PRIVATE KEY|client-key-data|OPSLOOP_PUBLIC_CANARY/.test(text))throw Error('Forbidden public content category in '+rel);
 }
 files.push({path:rel,bytes:b.length,sha256:sha(b)});
}}
walk(path.join(root,'dist-public'));if(!['index.html','404.html','_headers'].every(p=>files.some(f=>f.path===p)))throw Error('Required static files missing');files.sort((a,b)=>a.path.localeCompare(b.path,'en'));
const manifest={profile:'public',sourceDataBoundary:'reviewed input hash + exact derivative + import graph + isolated build + allowlisted files; pattern scan is supplementary',files,totalBytes:files.reduce((n,f)=>n+f.bytes,0)};
fs.writeFileSync(path.join(root,'public-artifact-manifest.json'),JSON.stringify(manifest,null,2)+'\n');
console.log('Public artifact gate PASS: '+files.length+' allowlisted files, no runtime/API/private source, '+manifest.totalBytes+' bytes');
