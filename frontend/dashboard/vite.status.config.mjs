import fs from 'node:fs';
import path from 'node:path';
import original from './vite.public.config.mjs';
export default {...original,
 publicDir:'status-static',
 plugins:original.plugins.map(plugin=>plugin.name==='public-data-boundary'?{...plugin,name:'status-data-boundary',
  transformIndexHtml:{order:'pre',handler:()=>fs.readFileSync('status.html','utf8')},
  generateBundle(){const ids=[...this.getModuleIds()].map(id=>id.replaceAll('\\','/'));if(ids.some(id=>/\/src\/data\/(createDataSource\.ts|live\/httpSource\.ts|replay\/(replaySource\.ts|evidence\.json))$/.test(id)))throw Error('Private source imported into status build');fs.writeFileSync('.status-build-audit.json',JSON.stringify({profile:'status',modules:ids.map(id=>id.includes('/node_modules/')?'node_modules/'+id.split('/node_modules/').pop():path.relative(process.cwd(),id))},null,2)+'\n');}
 }:plugin),
 build:{...original.build,outDir:'dist-status'},
};
