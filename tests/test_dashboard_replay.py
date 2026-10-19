"""Offline derivative safety: synthetic fixtures only, no cluster/credentials required."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('export_replay', ROOT/'scripts/export-dashboard-replay.py')
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)
PUBLIC = json.loads((ROOT/'frontend/dashboard/src/data/replay/evidence.json').read_text())

class ReplayExportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.roots = []
        for index, exp in enumerate(PUBLIC['experiments']):
            root = Path(self.temp.name)/str(index); root.mkdir(); self.roots.append(root)
            first, final = copy.deepcopy(exp['frames'])
            snapshot = final['snapshot']
            incident = snapshot['recentIncidents'][0]
            # These deliberate sentinel fields must never enter the derivative.
            incident['metadata']['annotations'] = {'password': 'PRIVATE_SENTINEL'}
            incident['spec']['trigger']['evidence'] = ['PRIVATE_SENTINEL']
            incident['status']['runtime']['message'] = 'PRIVATE_SENTINEL'
            d = {'metadata': {'name':'test', 'namespace':'test', 'uid':'fake-workload'}, 'spec':{'replicas':1,'template':{'spec':{'containers':[{'image':'private.invalid/test@sha256:'+'a'*64,'env':[{'name':'TOKEN','value':'PRIVATE_SENTINEL'}],'resources':{'requests':{'cpu':'50m','memory':'64Mi'}}}]}}}}
            files = {'evidence-before-delete.json': {'at':'2026-10-20T00:00:00Z','deployment':d,'recoverypolicies.opsloop.io':snapshot['policy'],'recoveryincidents.opsloop.io':incident}, ('initial-policy-disabled-verified.json' if index==0 else 'policy-preflight.json'): first['snapshot']['policy']}
            if index==1:
                files['experiment-timeline.json']={'fault_request_utc':'2026-10-20T00:00:00Z','restore_dispatch_elapsed':60,'node_ready_agent_active_elapsed':80}
            for name, obj in files.items(): (root/name).write_text(json.dumps(obj))
            self.checksums(root)
    def checksums(self, root):
        (root/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in sorted(root.glob('*.json'))))
    def export(self): return exporter.export(*self.roots)
    def test_allowlist_drops_sensitive_fields(self):
        data = self.export(); text=json.dumps(data)
        self.assertNotIn('PRIVATE_SENTINEL',text); self.assertNotIn('private.invalid',text)
        self.assertNotIn('annotations',text); self.assertNotIn('env',text)
        self.assertEqual(data['experiments'][1]['frames'][1]['snapshot']['recentIncidents'][0]['status']['runtime']['attempts'],[])
    def test_checksum_tamper_rejected(self):
        p=self.roots[0]/'evidence-before-delete.json'; p.write_text(p.read_text()+' ')
        with self.assertRaisesRegex(ValueError,'integrity'): self.export()
    def test_unlisted_consumed_file_rejected(self):
        p=self.roots[0]/'SHA256SUMS'; p.write_text('\n'.join(x for x in p.read_text().splitlines() if 'evidence-before-delete.json' not in x)+'\n')
        with self.assertRaisesRegex(ValueError,'not checksummed'): self.export()
    def test_enforce_rejected(self):
        root=self.roots[0]; p=root/'evidence-before-delete.json'; obj=json.loads(p.read_text()); obj['recoveryincidents.opsloop.io']['spec']['policy']['mode']='Enforce'; p.write_text(json.dumps(obj));self.checksums(root)
        with self.assertRaisesRegex(ValueError,'automatic recovery'): self.export()
    def test_nonzero_attempts_rejected(self):
        root=self.roots[0];p=root/'evidence-before-delete.json';obj=json.loads(p.read_text());obj['recoveryincidents.opsloop.io']['status']['runtime']['attempts']=[{'outcome':'Succeeded'}];p.write_text(json.dumps(obj));self.checksums(root)
        with self.assertRaisesRegex(ValueError,'automatic recovery'): self.export()
    def test_unknown_terminal_rejected(self):
        root=self.roots[0];p=root/'evidence-before-delete.json';obj=json.loads(p.read_text());obj['recoveryincidents.opsloop.io']['status']['runtime']['phase']='RECOVERED';p.write_text(json.dumps(obj));self.checksums(root)
        with self.assertRaisesRegex(ValueError,'terminal phase'): self.export()
    def test_checksum_manifest_escape_rejected(self):
        (self.roots[0]/'SHA256SUMS').write_text('0'*64+'  ../outside\n')
        with self.assertRaisesRegex(ValueError,'integrity'): self.export()
    def test_output_is_deterministic_and_sources_unchanged(self):
        before={str(p):p.read_bytes() for root in self.roots for p in root.iterdir()}
        self.assertEqual(self.export(),self.export())
        self.assertEqual(before,{str(p):p.read_bytes() for root in self.roots for p in root.iterdir()})

if __name__=='__main__': unittest.main()
