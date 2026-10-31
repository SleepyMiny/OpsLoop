import tempfile
from pathlib import Path
import unittest
from tools.engineering_runner.contracts import Rejected, digest, canonical
from tools.exhibition_disclosure import public_candidate_patch, load_public_artifacts

class DisclosureTests(unittest.TestCase):
    def test_exact_review_and_fixed_paths_required(self):
        content = b'--- a/apps/engineering-demo/discount.go\n+++ b/apps/engineering-demo/discount.go\n@@ -1 +1 @@\n-old\n+new\n'
        with tempfile.TemporaryDirectory() as d:
            path=Path(d)/'patch'; path.write_bytes(content)
            self.assertEqual(public_candidate_patch(path,digest(content))['content'],content.decode())
            for pin in ('',None,'0'*64):
                with self.assertRaises(Rejected): public_candidate_patch(path,pin)
            for body in (content.replace(b'apps/engineering-demo/discount.go',b'../../secret'),
                         content+b'+Bearer hidden-secret\n',content+b'+/home/person/.ssh/key\n'):
                path.write_bytes(body)
                with self.assertRaises(Rejected): public_candidate_patch(path,digest(body))

    def test_catalog_cannot_disclose_another_run_or_freeform_logs(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d); payload={'checks':[{'name':'unit','passed':True}]}
            artifact=root/'checks'; artifact.write_bytes(canonical(payload))
            catalog={'schema':'opsloop-exhibition-disclosures/v1','approvalReference':'explicit-local-review',
                     'entries':[{'runId':'run-1','kind':'checks','path':str(artifact),'sha256':digest(artifact.read_bytes())}]}
            path=root/'catalog'; path.write_bytes(canonical(catalog))
            self.assertEqual(load_public_artifacts(path,digest(path.read_bytes()),'other'),{'artifacts':[]})
            self.assertEqual(len(load_public_artifacts(path,digest(path.read_bytes()),'run-1')['artifacts']),1)
            payload['checks'][0]['output']='private output'
            artifact.write_bytes(canonical(payload)); catalog['entries'][0]['sha256']=digest(artifact.read_bytes())
            path.write_bytes(canonical(catalog))
            with self.assertRaises(Rejected): load_public_artifacts(path,digest(path.read_bytes()),'run-1')

if __name__=='__main__': unittest.main()
