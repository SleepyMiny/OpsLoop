import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
// Build tools need these OS settings. No inherited VITE_*, cloud, SSH, Kube or token env.
const env = {};
for (const key of ['PATH','HOME','TMPDIR','LANG','LC_ALL','TZ','SystemRoot']) if (process.env[key]) env[key] = process.env[key];
for (const args of [
  ['scripts/verify-public-evidence.mjs'],
  ['node_modules/typescript/bin/tsc','--noEmit','-p','tsconfig.json'],
  ['node_modules/vite/bin/vite.js','build','--config','vite.status.config.mjs'],
  ['scripts/audit-status.mjs'],
]) {
  const p = spawnSync(process.execPath, args, {cwd: root, env, stdio: 'inherit'});
  if (p.error || p.status !== 0) process.exit(p.status ?? 1);
}
