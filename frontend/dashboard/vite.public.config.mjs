import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import fs from 'node:fs';
import path from 'node:path';
import boundary from './src/public/evidence-boundary.json' with {type: 'json'};

export default defineConfig({
  plugins: [react(), {
    name: 'public-data-boundary',
    transformIndexHtml: {order: 'pre', handler: () => fs.readFileSync('public.html', 'utf8')},
    transform(code, id) {
      // Existing local simulations keep their identifiers. Only the public derivative changes labels.
      if (!/\/src\/data\/demo\/(baseline|engine|scenarios)\.ts$/.test(id)) return;
      let out = code;
      for (const [from,to] of Object.entries(boundary.aliases)) out = out.replaceAll(from,to);
      out = out.replaceAll('http://control-amd64:30080/', 'simulation://demo-api');
      return {code: out, map: null};
    },
    generateBundle() {
      const modules = [...this.getModuleIds()].map((id) => id.replaceAll('\\','/'));
      const forbidden = modules.filter((id) => /\/src\/data\/(createDataSource\.ts|live\/httpSource\.ts|replay\/(replaySource\.ts|evidence\.json))$/.test(id));
      if (forbidden.length) throw Error('Public bundle imported a private/local source module');
      fs.writeFileSync('.public-build-audit.json', JSON.stringify({
        profile: 'public', envFilesLoaded: false, liveAdapterIncluded: false,
        modules: modules.map((id) => id.includes('/node_modules/') ? 'node_modules/'+id.split('/node_modules/').pop() : path.relative(process.cwd(),id)),
      }, null, 2)+'\n');
    },
  }],
  envDir: false,
  envPrefix: 'OPSLOOP_PUBLIC_DISABLED_',
  publicDir: 'public-static',
  base: './',
  build: {assetsInlineLimit: 0, outDir: 'dist-public', sourcemap: false, rollupOptions: {input: 'index.html'}},
  preview: {host: '127.0.0.1', port: 4174, strictPort: true},
});
