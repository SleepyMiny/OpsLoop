"""Local synthetic API lifecycle tests; no cluster, model, grants or Git calls."""
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest

from tools.exhibition_control import ControlError, Coordinator, canonical
from tools.exhibition_hashes import describe
from tools.exhibition_lifecycle import Lifecycle, RUN, EPOCH, patched, verify_completion


def digest(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


class API:
    def __init__(self, policy, deployment, objects):
        self.policy = policy; self.deployment = deployment; self.objects = objects
        self.deleted = []; self.patch_calls = []; self.ambiguous = False
    def request(self, path, patch=None, absent=False):
        if '/recoverypolicies/' in path:
            value = self.policy
        elif '/deployments/' in path:
            value = self.deployment
        elif '?' in path:
            kind = {'recoveryincidents': 'RecoveryIncident', 'configmaps': 'ConfigMap', 'jobs': 'Job'}.get(path.split('?')[0].split('/')[-1])
            return {'items': [copy.deepcopy(o) for o in self.objects.values() if o['kind'] == kind]}
        else:
            value = self.objects.get(path.split('/')[-1])
            if value is None:
                if absent: return None
                raise ControlError('NOT_FOUND')
        if patch is not None:
            self.patch_calls.append(copy.deepcopy(patch))
            updated = patched(value, patch)
            value.clear(); value.update(updated)
        return copy.deepcopy(value)
    def delete(self, path, uid, rv):
        name = path.split('/')[-1]; obj = self.objects[name]
        assert obj['metadata']['uid'] == uid and obj['metadata']['resourceVersion'] == rv
        self.deleted.append((name, uid, rv)); del self.objects[name]
        if self.ambiguous:
            self.ambiguous = False
            raise ControlError('DELETE_UNCONFIRMED')
    def close(self): pass


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.binding = {'runID': 'run-1', 'epoch': 'epoch-1', 'policyUID': 'fixed-policy', 'deploymentUID': 'fixed-workload'}
        self.cfg = {'schema': 'opsloop-exhibition-lifecycle/v1', 'binding': self.binding,
            'expiresAt': (dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=5)).isoformat(),
            'namespace': 'demo', 'policy': 'fixed', 'deployment': 'app',
            'evidenceRoot': str(self.root/'evidence'), 'archiveRoot': str(self.root/'archive'),
            'allowedRestorePaths': ['/spec/template/spec/nodeSelector'], 'baselineWaitSeconds':0}
        (self.root/'evidence').mkdir(mode=0o700)
        self.file = self.root/'evidence/receipt.json'; self.file.write_text(json.dumps({'binding':self.binding,'synthetic':True})); self.file.chmod(0o600)
        self.spec = {'replicas': 1, 'template': {'spec': {'nodeSelector': {'node': 'baseline'}}}}
        self.cfg['baselineSpecSHA256'] = digest(self.spec)
        self.policy = {'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryPolicy','metadata': {'name': 'fixed', 'namespace': 'demo', 'uid': 'fixed-policy', 'resourceVersion': '7', 'generation':7},
            'spec': {'enabled': False}, 'status': {'incidentSequence': 37, 'failureHistory': ['preserved'],
                'conditions': [{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':7}]}}
        self.dep = {'apiVersion':'apps/v1','kind':'Deployment','metadata': {'name':'app','namespace':'demo','uid':'fixed-workload','resourceVersion':'9','generation':4},'spec':copy.deepcopy(self.spec),
            'status': {'observedGeneration':4,'readyReplicas':1,'updatedReplicas':1,'replicas':1}}
        self.object = {'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryIncident','metadata': {'namespace':'demo','name':'incident-38','uid':'incident-uid','resourceVersion':'3',
            'annotations':{RUN:'run-1',EPOCH:'epoch-1'}},'spec':{'policyRef':{'uid':'fixed-policy'},'workload':{'uid':'fixed-workload'}},
            'status':{'runtime':{'phase':'FAILED','completedAt':'2026-10-28T00:00:00Z'}}}
        self.cfg['baselineSpecHash'] = describe(self.dep, '/spec')
        self.api = API(self.policy,self.dep,{'incident-38':self.object})
        self.receipt = {'binding':self.binding,'stopConfirmed':True,'modelStopped':True,
            'evidence':[{'path':str(self.file),'sha256':hashlib.sha256(self.file.read_bytes()).hexdigest()}],
            'objects':[{'kind':'RecoveryIncident','name':'incident-38','uid':'incident-uid','resourceVersion':'3'}]}
        self.plan = {'binding':self.binding}
        self.lifecycle = Lifecycle(self.root/'state', lambda cfg:self.api)
        self.pin()
    def pin(self):
        file=self.root/'cleanup.json'; file.write_text(json.dumps(self.cfg));file.chmod(0o600)
        self.plan['lifecycle']={'path':str(file),'sha256':hashlib.sha256(file.read_bytes()).hexdigest()}
    def run_cleanup(self):
        return self.lifecycle.reconcile(self.plan,self.receipt,lambda *args:None)
    def test_archive_before_uid_rv_delete_and_sequence_preserved(self):
        before=copy.deepcopy(self.policy)
        result=self.run_cleanup()
        self.assertTrue(result['nextRunAllowed'])
        self.assertEqual(self.api.deleted,[('incident-38','incident-uid','3')])
        self.assertEqual(self.policy,before)
        self.assertEqual(self.file.read_text(),json.dumps({'binding':self.binding,'synthetic':True}))
        path=self.root/'state/run-1/completion.json'
        self.assertEqual(verify_completion(path,result['lifecycleReceiptSHA256'])['binding'],self.binding)
    def test_archive_mutation_blocks_next_authority(self):
        result=self.run_cleanup();manifest=json.loads(Path(result['archiveManifest']).read_text())
        file=Path(result['archiveManifest']).parent/manifest['files'][0]['sha256'];file.chmod(0o600);file.write_text('changed')
        with self.assertRaisesRegex(ControlError,'ARCHIVE_INTEGRITY'):
            verify_completion(self.root/'state/run-1/completion.json',result['lifecycleReceiptSHA256'])
    def test_ambiguous_delete_resumes_confirmation_without_second_delete(self):
        self.api.ambiguous=True
        with self.assertRaisesRegex(ControlError,'DELETE_UNCONFIRMED'):self.run_cleanup()
        self.assertEqual(len(self.api.deleted),1)
        self.assertTrue(self.run_cleanup()['objectsCleaned'])
        self.assertEqual(len(self.api.deleted),1)
    def test_foreign_active_changed_or_unarchived_object_never_deleted(self):
        for mutate in [lambda:setattr(self,'receipt',{**self.receipt,'objects':[]}),
                       lambda:self.object['metadata']['annotations'].update({RUN:'foreign'}),
                       lambda:self.object['status']['runtime'].update(phase='EXECUTING'),
                       lambda:self.object['metadata'].update(uid='replacement')]:
            with self.subTest(mutate=mutate):
                original=copy.deepcopy(self.object);receipt=copy.deepcopy(self.receipt);mutate()
                with self.assertRaises(ControlError):self.run_cleanup()
                self.assertEqual(self.api.deleted,[])
                self.object.clear();self.object.update(original);self.receipt=receipt
    def test_changed_evidence_and_missing_original_block_cleanup(self):
        self.file.write_text('tampered')
        with self.assertRaisesRegex(ControlError,'EVIDENCE_CHANGED'):self.run_cleanup()
        self.assertEqual(self.api.deleted,[])
    def test_stale_fence_generation_cannot_authorize_cleanup(self):
        self.policy['status']['conditions'][0]['observedGeneration'] = 6
        with self.assertRaisesRegex(ControlError, 'POLICY_STOP_UNCONFIRMED'): self.run_cleanup()
        self.assertFalse(self.api.deleted)

    def test_policy_active_or_baseline_not_ready_blocks(self):
        self.policy['spec']['enabled']=True
        with self.assertRaisesRegex(ControlError,'POLICY_STOP'):self.run_cleanup()
        self.policy['spec']['enabled']=False;self.dep['status']['readyReplicas']=0
        with self.assertRaisesRegex(ControlError,'BASELINE_READY'):self.run_cleanup()
        self.assertEqual(self.api.deleted,[])
    def test_exact_pinned_restore_allows_runtime_fields_but_not_other_drift(self):
        self.dep['spec']['template']['spec']['nodeSelector']={'node':'runtime-selected'}
        self.cfg['restorePatch']=[{'op':'replace','path':'/spec/template/spec/nodeSelector','value':{'node':'baseline'}}]
        self.receipt['restore']={'expectedSpecSHA256':digest(self.dep['spec']),'expectedSpecHash':describe(self.dep,'/spec'),'patch':self.cfg['restorePatch']};self.pin()
        self.dep['spec']['unexpected']='foreign'
        self.receipt['restore']['expectedSpecSHA256']=digest(self.dep['spec']);self.receipt['restore']['expectedSpecHash']=describe(self.dep,'/spec')
        with self.assertRaisesRegex(ControlError,'RESTORE_WOULD_NOT_MATCH_BASELINE'):self.run_cleanup()
        self.assertEqual(self.api.patch_calls,[])
        del self.dep['spec']['unexpected'];self.receipt['restore']['expectedSpecSHA256']=digest(self.dep['spec']);self.receipt['restore']['expectedSpecHash']=describe(self.dep,'/spec')
        self.assertTrue(self.run_cleanup()['baselineRestored'])
        self.assertEqual(self.dep['spec'],self.spec)
    def test_expired_cleanup_authority_blocks_all_transport(self):
        self.cfg['expiresAt']='2020-01-01T00:00:00Z';self.pin()
        with self.assertRaisesRegex(ControlError,'CLEANUP_AUTHORITY_EXPIRED'):self.run_cleanup()
        self.assertFalse(self.api.deleted)
    def test_same_name_foreign_uid_after_ambiguous_delete_is_not_adopted(self):
        self.api.ambiguous=True
        with self.assertRaises(ControlError):self.run_cleanup()
        replacement=copy.deepcopy(self.object);replacement['metadata']['uid']='new-uid';self.api.objects['incident-38']=replacement
        with self.assertRaisesRegex(ControlError,'OBJECT_REPLACED'):self.run_cleanup()
        self.assertEqual(len(self.api.deleted),1)
    def test_current_authority_removed_only_after_archive_with_cas(self):
        scope={**self.binding,'other':'not-authority'}
        self.policy['metadata']['annotations']={'opsloop.io/execution-grant':json.dumps({'scope':scope}),'keep':'unchanged'}
        result=self.run_cleanup()
        self.assertEqual(self.policy['metadata']['annotations'],{'keep':'unchanged'})
        self.assertEqual(self.policy['status']['incidentSequence'],37)
        self.assertEqual(len(self.api.patch_calls),1)
        self.assertTrue((Path(result['archiveManifest']).parent/'authority-retirement.json').exists())
    def test_foreign_authority_not_removed(self):
        self.policy['metadata']['annotations']={'opsloop.io/execution-grant':json.dumps({'scope':{**self.binding,'runID':'old-run'}})}
        with self.assertRaisesRegex(ControlError,'RETIRE_FOREIGN'):self.run_cleanup()
        self.assertEqual(self.api.patch_calls,[])

if __name__=='__main__':unittest.main()

class RepeatedLifecycleTests(unittest.TestCase):
    setUp = LifecycleTests.setUp
    pin = LifecycleTests.pin

    def test_a_a_and_b_b_reuse_one_policy_without_cluster_object_growth(self):
        for track in ('runtime', 'engineering'):
            with self.subTest(track=track):
                self.api.objects.clear(); self.api.deleted.clear()
                state=self.root/('coordinator-'+track);config={'schema':'opsloop-exhibition-control/v1','plans':[]};receipts={};plans=[]
                for number in (1,2):
                    binding={**self.binding,'runID':track+'-'+str(number),'epoch':track+'-epoch-'+str(number)}
                    cfg={**self.cfg,'binding':binding,'evidenceRoot':str(self.root/(track+'-evidence-'+str(number)))}
                    folder=Path(cfg['evidenceRoot']);folder.mkdir(mode=0o700)
                    evidence=folder/'observed.json';evidence.write_text(json.dumps({'binding':binding,'synthetic':True}));evidence.chmod(0o600)
                    cfgfile=self.root/(track+'-cleanup-'+str(number)+'.json');cfgfile.write_text(json.dumps(cfg));cfgfile.chmod(0o600)
                    plan={'schema':'opsloop-exhibition-plan/v1','planId':track+'-plan-'+str(number),'authorizationId':track+'-auth-'+str(number),
                          'binding':binding,'source':'measured','track':track,'expiresAt':cfg['expiresAt'],'totalSeconds':30,'approvalReference':'synthetic-local-only',
                          'lifecycle':{'path':str(cfgfile),'sha256':hashlib.sha256(cfgfile.read_bytes()).hexdigest()}}
                    file=self.root/(track+'-plan-'+str(number)+'.json');file.write_text(json.dumps(plan));file.chmod(0o600)
                    config['plans'].append({'path':str(file),'sha256':hashlib.sha256(file.read_bytes()).hexdigest()});plans.append(plan)
                    receipts[binding['runID']]={'binding':binding,'stopConfirmed':True,'modelStopped':True,
                        'evidence':[{'path':str(evidence),'sha256':hashlib.sha256(evidence.read_bytes()).hexdigest()}],
                        'objects':[{'kind':'RecoveryIncident','name':binding['runID'],'uid':binding['runID']+'-uid','resourceVersion':'1'}]}
                parent=self
                class Adapter:
                    calls=0
                    def prepare(self,plan):pass
                    def run(self,plan,emit,cancel):
                        self.calls+=1;parent.policy['status']['incidentSequence']+=1
                        obj=copy.deepcopy(parent.object);b=plan['binding'];obj['metadata'].update(name=b['runID'],uid=b['runID']+'-uid',resourceVersion='1',annotations={RUN:b['runID'],EPOCH:b['epoch']})
                        parent.api.objects[b['runID']]=obj
                        emit('observer','SYNTHETIC_RUN_ONLY',{'status':'LOCAL_SYNTHETIC'})
                        return {'state':'COMPLETED','stopConfirmed':True}
                    def reconcile(self,plan,emit):return receipts[plan['binding']['runID']]
                state.mkdir(mode=0o700)
                adapter=Adapter();c=Coordinator(state,config,{track:adapter},lifecycle=Lifecycle(state/'lifecycle',lambda cfg:self.api))
                try:
                    archived_first=None
                    before_sequence=self.policy['status']['incidentSequence']
                    for number,plan in enumerate(plans):
                        c.prepare(plan['planId']);run=c.start(plan['planId'],'click-'+str(number));c.worker.join(2)
                        self.assertTrue(c.state()['nextRunAllowed']);self.assertEqual(len(self.api.objects),0)
                        self.assertEqual(c.start(plan['planId'],'click-'+str(number))['runId'],run['runId'])
                        with self.assertRaises(ControlError):c.prepare(plan['planId'])
                        first=Path(self.cfg['archiveRoot'])/plans[0]['binding']['runID']/'manifest.json'
                        if archived_first is None:archived_first=first.read_bytes()
                        else:self.assertEqual(first.read_bytes(),archived_first)
                    self.assertEqual(adapter.calls,2);self.assertEqual(len(c.state()['runs']),2)
                    self.assertEqual(self.policy['status']['incidentSequence'],before_sequence+2)
                    self.assertEqual(self.policy['status']['failureHistory'],['preserved'])
                    self.assertEqual(len(self.api.deleted),2)
                finally:c.close()
