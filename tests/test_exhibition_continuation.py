"""Synthetic interrupted-DELETE custody through the real integrated entrypoint.

No cluster, private evidence, registry, account, model or subprocess is contacted.
The fixture explicitly simulates A deleted/B present and retains immutable custody.
"""
import copy,datetime as dt,json,tempfile,unittest
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import patch,Mock
from tools.exhibition_control import ControlError
from tools.exhibition_inputs import pin,new,iso
from tools.exhibition_lifecycle import patched,verify_completion
from tools.exhibition_execution import execute,preparation_report
from tools.exhibition_migration import migrate
from tests.fixtures.exhibition_continuation import configured_plan

class StopAtB(Exception):pass

class ConnectionTests(unittest.TestCase):
    def setUp(self):
        tmp=tempfile.TemporaryDirectory();self.addCleanup(tmp.cleanup);self.root=Path(tmp.name)
        self.p,self.objects,self.cfg,self.marker=configured_plan(self.root)
        cfg=self.cfg
        self.marker_before=self.marker.read_bytes()
        self.writes=[];self.sequence=[];parent=self
        class Reader:
            def __init__(self,plan,root,deadline):self.deadline=deadline
            def get(self,path,**kw):
                for r in parent.p['executionInputs']['readRequests']:
                    if r['path']==path:return copy.deepcopy(parent.objects[r['key']])
                raise AssertionError('Unlisted GET')
            def snapshot(self,**kw):return copy.deepcopy(parent.objects)
        self.Reader=Reader
        class API:
            def request(self,path,patch=None,absent=False):
                if path=='/api/v1/namespaces/kube-system':return parent.objects['cluster']
                if path=='/api/v1/namespaces/demo-a':return parent.objects['namespace-demo-a']
                if '/recoverypolicies/' in path:
                    obj=parent.objects['a-policy']
                    if patch is not None:
                        parent.writes.append(('PATCH',path));parent.sequence.append('a-retire')
                        parent.objects['a-policy']=patched(obj,patch);obj=parent.objects['a-policy']
                    return copy.deepcopy(obj)
                if '/deployments/' in path:return copy.deepcopy(parent.objects['a-deployment'])
                if path.endswith('?limit=1000'):return {'items':[]}
                if path==cfg['path']:
                    v=parent.objects['a-incident'];return None if v=={'absent':True} else copy.deepcopy(v)
                raise AssertionError('Unlisted transport GET')
            def delete(self,*a):raise AssertionError('A DELETE FORBIDDEN')
            def close(self):pass
        self.api=API()
    def run_entry(self):
        now=dt.datetime.now(dt.timezone.utc)
        with ExitStack() as s:
            # Candidate pinning is validated separately by the final default CLI;
            # this test intentionally rewrites only local roots into a temp sandbox.
            s.enter_context(patch('tools.exhibition_execution.local_pins'))
            s.enter_context(patch('tools.exhibition_execution.Reader',self.Reader))
            s.enter_context(patch('tools.exhibition_account.read_once',return_value={'state':'SYNTHETIC_ACCOUNT_READ'}))
            reg=s.enter_context(patch('tools.exhibition_execution.RegistryTransport')).return_value;reg.head.return_value=404
            publish=s.enter_context(patch('tools.exhibition_execution.publish_once',side_effect=AssertionError('publication blocked')))
            deploy=s.enter_context(patch('tools.exhibition_execution.deploy_once',side_effect=AssertionError('deployment blocked')))
            s.enter_context(patch('subprocess.run',side_effect=AssertionError('external process blocked')))
            def migration(path,digest):
                cfg=json.loads(Path(path).read_bytes())
                if cfg['target']['namespace']=='demo-b':
                    self.sequence.append('b-preparation')
                    self.assertEqual(cfg['objects'][0]['uid'],self.objects['b-incident']['metadata']['uid'])
                    self.assertTrue((self.root/'session/a-prior-deletion-confirmed.json').exists())
                    self.assertTrue(self.a_completion['nextRunAllowed'])
                    raise StopAtB()
                self.sequence.append('a-migration')
                result=migrate(path,digest,transport_factory=lambda _:self.api,model_checker=lambda *a:[])
                self.a_completion=verify_completion(result['completion'],result['sha256'])
                return result
            s.enter_context(patch('tools.exhibition_migration.migrate',side_effect=migration))
            try:execute(self.p,'e'*64,approved_at=iso(now),completion_deadline=iso(now+dt.timedelta(minutes=120)))
            finally:publish.assert_not_called();deploy.assert_not_called()
    def test_fixture_never_reads_private_live_evidence(self):
        original=Path.open
        private_root=(Path(__file__).resolve().parents[1]/'.opsloop/live-integration').resolve()
        with tempfile.TemporaryDirectory() as directory:
            sandbox=Path(directory).resolve()
            def bounded_open(path,*args,**kwargs):
                resolved=path.resolve()
                # CI may place TMPDIR below the private evidence root. Only
                # this newly owned sandbox is exempt; symlinks cannot escape it.
                if resolved.is_relative_to(private_root) and not resolved.is_relative_to(sandbox):
                    raise AssertionError('private live evidence access forbidden')
                return original(path,*args,**kwargs)
            with patch.object(Path,'open',bounded_open):
                with self.assertRaisesRegex(AssertionError,'private live evidence access forbidden'):
                    (private_root/'readiness-r4/execution-proposal-r4-final.json').read_bytes()
                escape=sandbox/'private-evidence-link'
                escape.symlink_to(private_root/'readiness-r4/execution-proposal-r4-final.json')
                with self.assertRaisesRegex(AssertionError,'private live evidence access forbidden'):
                    escape.read_bytes()
                plan,objects,cfg,marker=configured_plan(sandbox)
                self.assertEqual(preparation_report(plan,objects)['state'],'PREPARATION_INPUTS_PASS')
                self.assertEqual(objects['a-incident'],{'absent':True})
                self.assertEqual(json.loads(marker.read_text())['state'],'DELETE_DISPATCHED')
    def test_integrated_a_absent_custody_to_b_preparation(self):
        self.assertEqual(preparation_report(self.p,self.objects)['state'],'PREPARATION_INPUTS_PASS')
        with self.assertRaises(StopAtB):self.run_entry()
        self.assertEqual(self.sequence,['a-migration','a-retire','b-preparation'])
        self.assertEqual([v for v in self.writes if v[0]=='DELETE'],[])
        self.assertEqual(self.marker.read_bytes(),self.marker_before)
        self.assertTrue(self.marker.with_suffix('.confirmed.json').exists())
        self.assertEqual(self.a_completion['baseIncidentSequence'],1)
        self.assertEqual(json.loads((self.root/'session/result.json').read_text())['completed'],[])
    def test_other_uid_blocks_before_any_change(self):
        self.objects['a-incident']={'metadata':{'name':self.cfg['identity']['name'],'namespace':'demo-a','uid':'different','resourceVersion':'99'}}
        with self.assertRaisesRegex(ControlError,'OBJECT_REPLACED'):self.run_entry()
        self.assertEqual(self.writes,[]);self.assertEqual(self.sequence,[])
    def test_uncertain_get_blocks_before_any_change(self):
        self.objects['a-incident']={'error':'not-an-absence'}
        with self.assertRaises(ControlError):self.run_entry()
        self.assertEqual(self.writes,[]);self.assertEqual(self.sequence,[])
    def test_bad_custody_prevents_confirmation(self):
        self.marker.write_text('{}')
        with self.assertRaises(ControlError):self.run_entry()
        self.assertEqual(self.writes,[])
    def test_a_confirmation_does_not_bypass_fence(self):
        self.objects['b-policy']['spec']['enabled']=True
        with self.assertRaisesRegex(ValueError,'PREPARATION_BLOCKED'):self.run_entry()
        proof=json.loads((self.root/'session/a-prior-deletion-confirmed.json').read_text())
        self.assertFalse(proof['nextRunAllowed']);self.assertFalse(proof['migrationComplete'])
        self.assertEqual(self.writes,[]);self.assertEqual(self.sequence,[])
    def test_consumed_session_never_replays(self):
        with self.assertRaises(StopAtB):self.run_entry()
        previous=list(self.writes)
        with self.assertRaises(FileExistsError):self.run_entry()
        self.assertEqual(self.writes,previous)
    def test_completion_rechecks_confirmation_chain(self):
        with self.assertRaises(StopAtB):self.run_entry()
        self.marker.with_suffix('.confirmed.json').write_text('{}')
        completion=self.marker.parent/'completion.json'
        with self.assertRaises(ControlError):verify_completion(completion,pin(completion)['sha256'])
    def test_plan_resume_scope_cannot_expand(self):
        from tools.exhibition_inputs import migration_config
        cfg=migration_config(self.p,'a',approval_sha='new',deadline=iso(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=5)),integration=self.p['executionInputs']['integration'])
        cfg['retireAuthority']=False
        ref=new(self.root,'bad-resume.json',cfg);transport=Mock()
        with self.assertRaisesRegex(ControlError,'MIGRATION_RESUME_SCOPE_CHANGED'):migrate(ref['path'],ref['sha256'],transport_factory=transport)
        transport.assert_not_called()

if __name__=='__main__':unittest.main()
