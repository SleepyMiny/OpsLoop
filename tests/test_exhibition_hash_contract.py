"""Local-only hash contract tests with fixed bytes, adversarial controls and GET-only replay."""
import copy
import dataclasses
import datetime as dt
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from tools.exhibition_control import ControlError
from tools.exhibition_hashes import (Observation, RAW, DISPLAY, VERSION, NORMALIZATION,
    describe, encoded, digest, verify, load_capture, project)
from tools.exhibition_plan import generate
from tools.exhibition_preflight import preflight, object_path, saved_reader

GOLDEN_SPEC = b'{"replicas":1,"template":{"spec":{"containers":[{"env":[{"name":"MESSAGE","value":"\\ud55c\\uae00"},{"name":"MODE","value":"safe"}],"image":"demo@sha256:abc","name":"app","securityContext":{"runAsNonRoot":true}}]}}}'
GOLDEN_SHA = 'dc114aefa57f841db9a915fd2737b6236271cbde87803ebb413f9553f18d9f1b'


def deployment(name,uid,ns):
    return {'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':name,'namespace':ns,'uid':uid,'generation':1,'resourceVersion':'7'},
        'spec':json.loads(GOLDEN_SPEC),'status':{'observedGeneration':1,'readyReplicas':1,'replicas':1,'updatedReplicas':1}}


def fixtures():
    raw={};targets={};incs=[]
    for k in ('a','b'):
        ns='demo-'+k;dep=deployment('app','dep-'+k,ns)
        if k=='b':dep['spec']['template']['spec']['containers'][0]['env']=[{'name':'ENV'+str(i),'value':'unchanged'} for i in range(5)]+[{'name':'DEMO_B_REPLAY','value':'empty-order-after-30s'}]
        pol={'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryPolicy','metadata':{'namespace':ns,'name':'fixed','uid':'pol-'+k,'generation':2,'resourceVersion':'8'},
             'spec':{'enabled':False},'status':{'incidentSequence':1,'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':2}]}}
        inc={'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryIncident','metadata':{'namespace':ns,'name':'incident','uid':'inc-'+k,'resourceVersion':'9','annotations':{'opsloop.io/execution-grant-binding':'grant-'+k}},
             'spec':{'policyRef':{'uid':'pol-'+k},'workload':{'uid':'dep-'+k}},'status':{'runtime':{'phase':'FAILED','completedAt':'2026-01-01T00:00:00Z'}}}
        targets[k]={'namespace':ns,'deployment':'app','deploymentUID':'dep-'+k,'observedDeploymentGeneration':1,'observedDeploymentSpecSHA256':'wrong-redacted',
          'policySpecSHA256':digest(pol['spec']),'policy':'fixed','policyUID':'pol-'+k,'observedPolicyGeneration':2,'observedIncidentSequence':1,'image':'demo@sha256:abc'}
        incs.append({'namespace':ns,'name':'incident','uid':'inc-'+k,'resourceVersion':'9','phase':'FAILED','grantBinding':'grant-'+k})
        raw.update({k+'-deployment':dep,k+'-policy':pol,k+'-incident':inc})
    ctrl=deployment('controller','controller-uid','system');raw['controller-deployment']=ctrl
    prior={'candidate':{'path':'previous','sha256':'old'},'fixedTargets':targets,'migration':{'legacyObjectsAllowed':incs},
      'deployment':{'controller':{'namespace':'system','name':'controller','uid':'controller-uid','expectedSpecSHA256':digest(ctrl['spec'])},'imagePublication':{'digest':'unchanged-image'}},
      'authority':{'maxNewPermits':4},'trials':{'sequence':['A1','A2','B1','B2'],'modelCalls':2},'cleanupAndRollback':{'same':True},'excludedEvenOnProposedApproval':['Quota','RBAC'],'timeWindow':{'maxWholeSessionMinutes':90}}
    capture={k:Observation(obj,RAW,hashlib.sha256(json.dumps(obj).encode()).hexdigest(),'2026-01-01T00:00:00Z',object_path(describe(obj,'/')['object'])) for k,obj in raw.items()}
    return prior,capture


def build(prior,capture):
    original=fixtures()[1]
    basis={'decision':'NO_GO_STOPPED_BEFORE_MUTATION','approvalSHA256':'old','findings':[{'target':k,'deployment':{'actualRawSpecSHA256':describe(original[k+'-deployment'].obj,'/spec')['sha256']},'incident':{'actualFullObjectSHA256':describe(original[k+'-incident'].obj,'/')['sha256']}} for k in ('a','b')]}
    return generate(prior,capture,previous_ref={'path':'old','sha256':'old'},capture_ref={'path':'raw-catalog','sha256':'original'},candidate_ref={'path':'candidate','sha256':'candidate'},repository='/tmp',prepared_at='2026-01-01T00:00:00+00:00',valid_until='2026-01-01T01:00:00+00:00',correction_basis=basis,basis_ref={'path':'original-no-go','sha256':'basis'},candidate_tree='c'*40)


class ContractTests(unittest.TestCase):
    def test_fixed_canonical_bytes_and_external_expected_digest(self):
        obj=deployment('app','dep-1','demo')
        self.assertEqual(encoded(obj['spec']),GOLDEN_SPEC)
        self.assertEqual(describe(obj,'/spec')['sha256'],GOLDEN_SHA)
        self.assertEqual(describe(obj,'/spec')['normalization'],NORMALIZATION)
        for change in ('env','image','security'):
            other=copy.deepcopy(obj);c=other['spec']['template']['spec']['containers'][0]
            if change=='env':c['env'][0]['value']='changed'
            if change=='image':c['image']='different'
            if change=='security':c['securityContext']['runAsNonRoot']=False
            self.assertNotEqual(describe(other,'/spec')['sha256'],GOLDEN_SHA)
    def test_wrong_domain_uid_scope_version_or_missing_contract_rejected(self):
        obj=deployment('app','dep-1','demo');pin=describe(obj,'/spec')
        for key,value in [('representation',DISPLAY),('fields','/'),('normalization','unknown'),('schema','v0')]:
            with self.subTest(key=key),self.assertRaises(ControlError):verify(obj,{**pin,key:value},fields='/spec')
        for invalid in (None,{},pin|{'unknown':True}):
            with self.assertRaises(ControlError):verify(obj,invalid,fields='/spec')
        replacement=copy.deepcopy(obj);replacement['metadata']['uid']='same-name-different-uid'
        with self.assertRaisesRegex(ControlError,'IDENTITY_CHANGED'):verify(replacement,pin,fields='/spec')
        with self.assertRaises(ControlError):verify(project(obj),pin,fields='/spec')
    def test_duplicate_keys_and_nonfinite_json_refused(self):
        from tools.exhibition_hashes import parse
        for raw in (b'{"a":1,"a":2}',b'{"v":NaN}',b'{"v":Infinity}'):
            with self.assertRaises(ControlError):parse(raw)


class ConnectedTests(unittest.TestCase):
    def setUp(self):self.prior,self.capture=fixtures();self.plan=build(self.prior,self.capture)
    def test_saved_original_generator_real_preflight_pass_with_zero_mutations(self):
        reads=[];reader=saved_reader(self.capture)
        def get(path):reads.append(('GET',path));return reader(path)
        result=preflight(self.plan,get)
        self.assertEqual(self.plan['candidateTree'],'c'*40)
        self.assertEqual(result['state'],'OBJECT_HASH_PREFLIGHT_PASS');self.assertEqual(result['mutationRequests'],0)
        self.assertEqual(len(reads),7);self.assertTrue(all(x[0]=='GET' for x in reads))
        # Explicit baseline expectation, not only helper-to-helper equality.
        b=self.capture['b-deployment'].obj;expected=copy.deepcopy(b['spec'])
        expected['template']['spec']['containers'][0]['env'][5]['value']=''
        self.assertEqual(self.plan['fixedTargets']['b']['baselineSpecHash']['sha256'],hashlib.sha256(json.dumps(expected,sort_keys=True,separators=(',',':'),ensure_ascii=True).encode()).hexdigest())
        self.assertEqual(self.plan['trials'],self.prior['trials']);self.assertEqual(self.plan['deployment']['imagePublication'],self.prior['deployment']['imagePublication'])
    def test_display_original_mix_refused_at_generation(self):
        for key in ('a-deployment','a-incident'):
            capture=dict(self.capture);capture[key]=dataclasses.replace(capture[key],obj=project(capture[key].obj),representation=DISPLAY)
            with self.assertRaisesRegex(ControlError,'RAW_PROVENANCE'):build(self.prior,capture)
    def test_generator_hash_inputs_feed_real_migration_and_restore_checks(self):
        from tools.exhibition_plan import migration_hash_inputs
        from tools.exhibition_hashes import bound_pin
        from tools.exhibition_lifecycle import patched
        for key in ('a','b'):
            cfg=migration_hash_inputs(self.plan,key)
            obj=self.capture[key+'-deployment'].obj
            baseline=patched(obj,self.plan['fixedTargets'][key]['baselineDerivation']['patch'])
            verify(baseline,bound_pin(cfg,'baselineSpec',fields='/spec'),fields='/spec')
            verify(self.capture[key+'-policy'].obj,bound_pin(cfg,'policySpec',fields='/spec'),fields='/spec')
            verify(self.capture[key+'-incident'].obj,cfg['objects'][0]['hash'],fields='/')
        changed=copy.deepcopy(self.plan)
        changed['fixedTargets']['a']['baselineSpecHash']['sha256']='0'*64
        changed['fixedTargets']['a']['readyBaselineSpecSHA256']='0'*64
        with self.assertRaises(ControlError):preflight(changed,saved_reader(self.capture))

    def test_generator_cannot_silently_adopt_new_raw_content(self):
        changed=dict(self.capture);obj=copy.deepcopy(changed['a-deployment'].obj)
        obj['spec']['template']['spec']['containers'][0]['env'][0]['value']='changed-after-no-go'
        changed['a-deployment']=dataclasses.replace(changed['a-deployment'],obj=obj)
        with self.assertRaisesRegex(ControlError,'RAW_CHANGED_SINCE_NO_GO'):build(self.prior,changed)

    def test_missing_original_refused_at_generation(self):
        capture=dict(self.capture);del capture['a-incident']
        with self.assertRaisesRegex(ControlError,'REQUIRED_RAW_MISSING'):build(self.prior,capture)
    def test_original_env_image_security_uid_phase_mutations_refused_no_writes(self):
        for change in ('env','image','security','uid','phase'):
            with self.subTest(change=change):
                objects={object_path(describe(o.obj,'/')['object']):copy.deepcopy(o.obj) for o in self.capture.values()}
                d=objects[object_path(self.plan['objectHashContract']['objects']['a-deployment']['object'])]
                c=d['spec']['template']['spec']['containers'][0]
                if change=='env':c['env'][0]['value']='tampered'
                if change=='image':c['image']='tampered'
                if change=='security':c['securityContext']['runAsNonRoot']=False
                if change=='uid':d['metadata']['uid']='replacement'
                if change=='phase':objects[object_path(self.plan['objectHashContract']['objects']['a-incident']['object'])]['status']['runtime']['phase']='EXECUTING'
                calls=[]
                def get(path):calls.append('GET');return objects[path]
                with self.assertRaises(ControlError):preflight(self.plan,get)
                self.assertNotIn('PATCH',calls);self.assertNotIn('DELETE',calls)
    def test_missing_metadata_and_custody_delete_mismatch_fail_before_get(self):
        from unittest.mock import Mock
        cases=[]
        no=copy.deepcopy(self.plan);del no['objectHashContract'];cases.append(no)
        no=copy.deepcopy(self.plan);del no['objectHashContract']['objects']['a-incident']['normalization'];cases.append(no)
        no=copy.deepcopy(self.plan);no['migration']['legacyObjectsAllowed'][0]['uid']='different-delete-target';cases.append(no)
        no=copy.deepcopy(self.plan);no['fixedTargets']['a']['observedDeploymentSpecHash']['representation']=DISPLAY;cases.append(no)
        for plan in cases:
            get=Mock()
            with self.assertRaises(ControlError):preflight(plan,get)
            get.assert_not_called()
    def test_private_capture_byte_pin_prevents_relabelled_view(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);obj=self.capture['a-deployment'].obj;raw=root/'original.json';raw.write_text(json.dumps(obj));raw.chmod(0o600)
            row={'name':'a-deployment','method':'GET','exitCode':0,'representation':RAW,'historicalOriginal':False,'observedAt':'2026-01-01T00:00:00Z','pathToRaw':str(raw),'responseBytesSHA256':hashlib.sha256(raw.read_bytes()).hexdigest(),'uid':obj['metadata']['uid'],'resourceVersion':'7','path':self.capture['a-deployment'].path}
            file=root/'capture.json';file.write_text(json.dumps({'records':[row],'mutations':0}));file.chmod(0o600);pin=hashlib.sha256(file.read_bytes()).hexdigest()
            self.assertEqual(load_capture(file,pin)['a-deployment'].obj,obj)
            raw.write_text(json.dumps(project(obj)))
            with self.assertRaisesRegex(ControlError,'SOURCE_BYTES_CHANGED'):load_capture(file,pin)
    def test_new_observation_not_relabelled_as_past_original(self):
        result=self.plan['objectHashContract'];self.assertIn('New GET capture',result['observationMeaning'])
        self.assertEqual(result['capture']['sha256'],'original')


class MutationGateTests(unittest.TestCase):
    def test_migration_bad_contract_or_object_never_restores_or_deletes(self):
        from tests.test_exhibition_migration import MigrationTests
        for change in ('missing','display','delete-uid','content'):
            fixture=MigrationTests();fixture.setUp()
            try:
                # A restoration would be necessary, but an invalid custody pin must stop it first.
                fixture.dep['spec']['template']['spec']['nodeSelector']={'node':'changed'}
                fixture.plan['restore']={'patch':[{'op':'replace','path':'/spec/template/spec/nodeSelector','value':{'node':'baseline'}}]}
                fixture.plan['allowedRestorePaths']=['/spec/template/spec/nodeSelector']
                if change=='missing':del fixture.plan['objects'][0]['hash']
                if change=='display':fixture.plan['objects'][0]['hash']['representation']=DISPLAY
                if change=='delete-uid':fixture.plan['objects'][0]['uid']='replacement'
                if change=='content':fixture.object['status']['runtime']['phase']='EXECUTING'
                with self.assertRaises(ControlError):fixture.execute()
                self.assertEqual(fixture.api.patch_calls,[]);self.assertEqual(fixture.api.deleted,[])
            finally:fixture.doCleanups()
    def test_restore_missing_contract_or_changed_env_cannot_patch(self):
        from tests.test_exhibition_lifecycle import LifecycleTests
        for change in ('missing','display','env'):
            fixture=LifecycleTests();fixture.setUp()
            try:
                if change=='missing':del fixture.cfg['baselineSpecHash']
                if change=='display':fixture.cfg['baselineSpecHash']['representation']=DISPLAY
                if change=='env':fixture.dep['spec']['template']['spec']['env']=[{'name':'PROTECTED','value':'changed'}]
                fixture.pin()
                with self.assertRaises(ControlError):fixture.run_cleanup()
                self.assertEqual(fixture.api.patch_calls,[]);self.assertEqual(fixture.api.deleted,[])
            finally:fixture.doCleanups()

if __name__=='__main__':unittest.main()
