"""Cleanup custody and fault settlement fixtures; no Kubernetes/model access."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import Mock, patch
from tools.exhibition_control import ControlError, canonical
from tools.exhibition_reconcile import inventory, reconcile_runtime, new_file
from tests.test_exhibition_runtime import fixture, FakeAPI, FakeAdapter


class CustodyTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.binding={'runID':'run','epoch':'epoch','policyUID':'policy','deploymentUID':'dep'}
        self.cfg={'namespace':'ns','policy':'fixed','binding':self.binding}
        self.inc={'metadata':{'name':'incident','uid':'uid-original','resourceVersion':'3','annotations':{'opsloop.io/exhibition-run':'run','opsloop.io/exhibition-epoch':'epoch','opsloop.io/execution-grant-binding':'c'*64}},'spec':{'policyRef':{'uid':'policy'},'workload':{'uid':'dep'}}}
        self.pol={'metadata':{'uid':'policy','generation':8,'resourceVersion':'10'},'spec':{'enabled':False},'status':{'incidentSequence':7,'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':8}]}}
        self.api=Mock();self.api.request.side_effect=lambda path,**kw:self.pol if 'recoverypolicies/' in path else self.inc
    def identity(self):
        new_file(self.root/'incident-identity.json',{'schema':'opsloop-exhibition-incident-identity/v1','binding':self.binding,'namespace':'ns','name':'incident','uid':'uid-original','grantBinding':'c'*64})
    def test_unknown_first_observation_is_not_adopted(self):
        with self.assertRaisesRegex(ControlError,'CUSTODY_UNKNOWN'):inventory(self.api,self.cfg,'incident',self.root,7)
    def test_same_name_replaced_uid_and_missing_before_archive_rejected(self):
        self.identity()
        self.inc['metadata']['uid']='uid-replacement'
        with self.assertRaisesRegex(ControlError,'REPLACED'):inventory(self.api,self.cfg,'incident',self.root,7)
        self.inc=None
        with self.assertRaisesRegex(ControlError,'MISSING'):inventory(self.api,self.cfg,'incident',self.root,7)
    def test_known_uid_retained_and_no_consumption_case_requires_sequence(self):
        self.identity();items=inventory(self.api,self.cfg,'incident',self.root,7)
        self.assertEqual(items[0]['uid'],'uid-original')
        (self.root/'incident-identity.json').unlink();self.inc=None
        self.assertEqual(inventory(self.api,self.cfg,'incident',self.root,7),[])
        self.pol['status']['incidentSequence']=8
        with self.assertRaisesRegex(ControlError,'CONSUMED'):inventory(self.api,self.cfg,'incident',self.root,7)
    def test_stale_fence_generation_is_not_stop_proof(self):
        self.inc=None;self.pol['status']['conditions'][0]['observedGeneration']=7
        with self.assertRaisesRegex(ControlError,'STOP_UNCONFIRMED'):inventory(self.api,self.cfg,'incident',self.root,7)


class RuntimeCleanupTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.plan=fixture(self.root);self.api=FakeAPI(self.plan);self.adapter=FakeAdapter(self.api);self.adapter.phase='STOP_CONFIRMED'
        self.api.policy['spec']['enabled']=False;self.api.policy['status']={'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':1}]}
        self.cfg={'namespace':'demo-a','policy':'demo-api','binding':self.plan['binding']}
        self.call=patch('tools.exhibition_reconcile.config',return_value=(self.cfg,self.root));self.call.start();self.addCleanup(self.call.stop)
        new_file(self.root/'execution-start.json',{'binding':self.plan['binding']})
    def intent(self, confirmed=True, elapsed=0, boot=None):
        new_file(self.root/'fault-intent.json',{'binding':self.plan['binding']})
        if confirmed:new_file(self.root/'fault-confirmed.json',{'binding':self.plan['binding'],'sourcePodUID':'pod-old','durationSeconds':30,'hostBoot':boot or Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'confirmedBootSeconds':time.clock_gettime(time.CLOCK_BOOTTIME)-elapsed})
    def test_stale_ready_immediately_after_fault_does_not_unlock(self):
        self.intent()
        with self.assertRaisesRegex(ControlError,'TTL_NOT_ENDED'):reconcile_runtime(self.plan,self.adapter,lambda *a:None)
        self.assertFalse((self.root/'cleanup-input.json').exists());self.assertEqual(self.api.faults,0)
    def test_missing_fault_response_or_other_boot_stays_blocked(self):
        self.intent(confirmed=False)
        with self.assertRaisesRegex(ControlError,'DISPATCH_UNCONFIRMED'):reconcile_runtime(self.plan,self.adapter,lambda *a:None)
        (self.root/'fault-intent.json').unlink();self.intent(boot='other-boot',elapsed=999)
        with self.assertRaisesRegex(ControlError,'RECEIPT_CHANGED'):reconcile_runtime(self.plan,self.adapter,lambda *a:None)
    def test_confirmed_elapsed_fault_requires_incident_custody_before_cleanup(self):
        self.intent(elapsed=35)
        # FakeAPI supplies a real-looking Incident but it was never observed by this run.
        with self.assertRaisesRegex(ControlError,'CUSTODY_UNKNOWN'):reconcile_runtime(self.plan,self.adapter,lambda *a:None)
        self.assertFalse((self.root/'cleanup-input.json').exists())

if __name__=='__main__':unittest.main()

class InterruptedModelTests(unittest.TestCase):
    def test_failed_main_pid_zero_does_not_hide_populated_descendants(self):
        from tools.exhibition_reconcile import confirm_cgroup_empty
        unit='opsloop-eb-'+'a'*32
        fields={'LoadState':'loaded','ControlGroup':'/user.slice/'+unit+'.service'}
        original=Path.read_text
        def contents(path,*a,**kw):
            if path.name=='cgroup.procs':return ''
            if path.name=='cgroup.events':return 'populated 1\nfrozen 0\n'
            return original(path,*a,**kw)
        with patch.object(Path,'is_symlink',return_value=False),patch.object(Path,'exists',return_value=True),patch.object(Path,'read_text',contents):
            with self.assertRaisesRegex(ControlError,'POPULATED'):confirm_cgroup_empty({},fields,unit)
        with patch.object(Path,'is_symlink',return_value=False),patch.object(Path,'exists',return_value=True),patch.object(Path,'read_text',side_effect=['','populated 0\n']):
            confirm_cgroup_empty({},fields,unit)
    def test_claim_owner_binding_is_atomic_and_cleanup_retry_preserves_evidence(self):
        import os,sqlite3
        from tools.engineering_runner.runner import Ledger
        from tools.exhibition_reconcile import model_jobs_stopped
        from tests.test_engineering_runner import bundle
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);ledger=Ledger(root);b=bundle();job='eb-'+'b'*32;ledger.claim(b,job)
            raw=ledger.db.execute('SELECT detail FROM jobs').fetchone()[0];detail=json.loads(raw)
            self.assertEqual(detail['incident'],b['incident']);self.assertEqual(detail['owner']['pid'],os.getpid())
            detail['owner']['boot']='prior-boot';detail['model_process']={'descendants_terminated':False}
            ledger.update(job,'UNCONFIRMED_TERMINATION',detail);ledger.db.close()
            work=root/'runs'/job;work.mkdir(parents=True);unit='opsloop-eb-'+'a'*32
            (work/'model.process.json').write_text(json.dumps({'schema':'opsloop-sandbox-process/v1','unit':unit,'hostBoot':'prior-boot'}))
            binding={'runID':b['incident']['run_id'],'epoch':b['incident']['epoch']}
            response=Mock(returncode=0,stdout=b'ActiveState=inactive\nMainPID=0\nControlGroup=\nLoadState=not-found\n')
            with patch('tools.exhibition_reconcile.subprocess.run',return_value=response) as command:
                first=model_jobs_stopped(root,binding);proof=(work/'cleanup-reconciliation.json').read_bytes()
                second=model_jobs_stopped(root,binding)
                self.assertEqual(first,second);self.assertEqual((work/'cleanup-reconciliation.json').read_bytes(),proof)
                self.assertTrue(all('stop' not in call.args[0] for call in command.call_args_list))
            db=sqlite3.connect(root/'ledger.sqlite3');self.assertEqual(db.execute('SELECT state FROM jobs').fetchone()[0],'CANCELLED');self.assertEqual(db.execute('SELECT count(*) FROM jobs').fetchone()[0],1);db.close()

class PrearmTests(unittest.TestCase):
    setUp = CustodyTests.setUp
    def test_cleanup_only_disables_exact_observe_state_with_separate_authority(self):
        from tools.exhibition_reconcile import prearm_stop
        before={'generation':8,'spec':{'enabled':False,'mode':'Enforce'}}
        self.pol['metadata']['generation']=9;self.pol['spec']={'enabled':True,'mode':'ObserveOnly'}
        self.cfg['allowPreArmDisable']=True
        mutations=[]
        def request(path,patch=None,**kw):
            if patch:
                mutations.append(patch);self.pol['spec']['enabled']=False;self.pol['metadata']['generation']=10;self.pol['status']['conditions'][0]['observedGeneration']=10
            return self.pol
        self.api.request.side_effect=request
        prearm_stop({'binding':self.binding},self.cfg,self.api,self.root,before,7)
        prearm_stop({'binding':self.binding},self.cfg,self.api,self.root,before,7)
        self.assertEqual(len(mutations),1);self.assertEqual(mutations[0][-1],{'op':'replace','path':'/spec/enabled','value':False})
        self.assertEqual(self.pol['status']['incidentSequence'],7)
    def test_prearm_never_stops_an_enforced_or_foreign_run_as_fallback(self):
        from tools.exhibition_reconcile import prearm_stop
        before={'generation':8,'spec':{'enabled':False,'mode':'Enforce'}}
        self.pol['metadata']['generation']=9;self.pol['spec']={'enabled':True,'mode':'Enforce'}
        self.cfg['allowPreArmDisable']=True
        with self.assertRaisesRegex(ControlError,'POLICY_CHANGED'):prearm_stop({'binding':self.binding},self.cfg,self.api,self.root,before,7)
        self.pol['spec']['mode']='ObserveOnly';self.pol['metadata']['annotations']={'opsloop.io/execution-grant':'previous'}
        with self.assertRaisesRegex(ControlError,'AUTHORITY_PRESENT'):prearm_stop({'binding':self.binding},self.cfg,self.api,self.root,before,7)
