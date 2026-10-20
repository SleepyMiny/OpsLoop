import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {fileURLToPath} from 'node:url';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const boundary = JSON.parse(fs.readFileSync(path.join(root, 'src/public/evidence-boundary.json'), 'utf8'));
const raw = fs.readFileSync(path.join(root, boundary.input));
if (crypto.createHash('sha256').update(raw).digest('hex') !== boundary.approvedInputSHA256) throw Error('Replay input changed: explicit data review required; do not auto-bless hashes.');
function derive(v) {
  if (Array.isArray(v)) return v.map(derive);
  if (v && typeof v === 'object') return Object.fromEntries(Object.entries(v).map(([k,a]) => [k,derive(a)]));
  if (typeof v === 'string') for (const [from,to] of Object.entries(boundary.aliases)) v = v.replaceAll(from,to);
  return v;
}
const expected = derive(JSON.parse(raw));
for (const exp of expected.experiments) for (const k of boundary.droppedExperimentKeys) delete exp[k];
const actual = JSON.parse(fs.readFileSync(path.join(root, boundary.output), 'utf8'));
if (JSON.stringify(actual) !== JSON.stringify(expected)) throw Error('Public Replay is not the reviewed deterministic derivative.');
console.log('Public Replay: reviewed input hash + exact deterministic derivative PASS');
