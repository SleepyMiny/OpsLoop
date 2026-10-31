"""Signed local fixtures only: no Kubernetes, fault, model or real credentials."""
import base64
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from tools.exhibition_control import ControlError
from tools.exhibition_demo_b_trial import (Stop, bind_controller, exhibition_permit,
    granted_at, initial_policy, policy_generations, setup, remember_incident, trial)
from tools.exhibition_handoff import preflight, engineering_budget


def digest(data): return hashlib.sha256(data).hexdigest()
def encode(value): return json.dumps(value,sort_keys=True,separators=(',',':')).encode()


class RepeatedHandoffTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.key=self.root/'synthetic.key'
        result=subprocess.run(['/usr/bin/openssl','genpkey','-algorithm','ED25519','-out',str(self.key)],capture_output=True)
        self.assertEqual(result.returncode,0);os.chmod(self.key,0o600)
        public=subprocess.run(['/usr/bin/openssl','pkey','-in',str(self.key),'-pubout','-outform','DER'],capture_output=True,check=True).stdout
        self.public=base64.b64encode(public[-32:]).decode()
        self.begin=dt.datetime.now(dt.timezone.utc)-dt.timedelta(seconds=1)
        self.binding={'runID':'repeated-b-1','epoch':'fresh-b-epoch-1','policyUID':'fixed-policy-uid','deploymentUID':'fixed-deployment-uid'}
        self.scope={**self.binding,'clusterUID':'cluster','namespaceUID':'namespace','namespace':'opsloop-demo-b','policy':'engineering-demo','deployment':'engineering-demo','policyGeneration':12,'policyHash':'a'*64,'incident':'engineering-demo-fixed-po-00008'}
        self.grant={'version':'opsloop-execution-grant/v1','scope':self.scope,'instance':'b'*64,'notBefore':self.begin.isoformat(),'notAfter':(self.begin+dt.timedelta(seconds=600)).isoformat()}
        self.payload={'schema':'opsloop-exhibition-permit/v1','grant':self.grant,'authorizationID':'auth-repeat-1','ordinal':1,'baseIncidentSequence':7,'maxIncidents':1,'maxActions':1,'previousCompletionSHA256':'c'*64,'allowedCircuitSignatures':[],
            'modelBudget':{'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt','maxModelCalls':1,'totalSeconds':900,'expiresAt':(self.begin+dt.timedelta(seconds=900)).isoformat()}}
        self.cfg=self.sign(self.payload)
        initial={'mode':'Enforce','enabled':False,'budget':{'maxActionsPerIncident':1}}
        self.runtime={'runID':self.binding['runID'],'epoch':self.binding['epoch'],'executionScope':self.scope,'initialPolicyGeneration':10,'initialIncidentSequence':7,'initialPolicySpec':initial,'observePolicySpec':{**initial,'mode':'ObserveOnly','enabled':True},'enforcePolicySpec':{**initial,'enabled':True},'exhibitionPermit':self.cfg,'authorizationId':self.payload['authorizationID'],'expiresAt':self.payload['modelBudget']['expiresAt']}
        self.setup={'state':'STATIC_CONTROLLER_READY_POLICY_DISABLED','authorityController':{'instance':'b'*64}}
        (self.root/'verification').mkdir()
        prov=self.pin('verification/image-provenance.json',{'repository':'repo','base_commit':'a'*40})
        self.plan={'schema':'opsloop-exhibition-plan/v1','planId':'plan-repeat-1','authorizationId':self.payload['authorizationID'],'approvalReference':'SYNTHETIC LOCAL TEST ONLY','track':'engineering','source':'measured','binding':self.binding,'totalSeconds':900,'expiresAt':self.payload['modelBudget']['expiresAt'],'model':'gpt-6-luna','maxModelCalls':1,'exhibitionPermit':self.cfg,'engineering':{'repository':'repo','base_commit':'a'*40},'runtimeCapture':{'root':str(self.root),'provenance':prov['path'],'provenanceSHA256':prov['sha256']}}
        self.repin()

    def pin(self,name,value):
        path=self.root/name;path.write_bytes(encode(value));os.chmod(path,0o600)
        return {'path':str(path),'sha256':digest(path.read_bytes())}

    def sign(self,payload):
        data=self.root/'payload.json';data.write_bytes(encode(payload))
        signed=subprocess.run(['/usr/bin/openssl','pkeyutl','-sign','-inkey',str(self.key),'-rawin','-in',str(data)],capture_output=True,check=True).stdout
        env={'payload':base64.b64encode(data.read_bytes()).decode(),'signature':base64.b64encode(signed).decode()}
        cfg=self.pin('permit.json',env);cfg['publicKey']=self.public;return cfg

    def repin(self):
        p=self.pin('runtime.json',self.runtime);self.setup['planSHA256']=p['sha256'];s=self.pin('setup.json',self.setup)
        self.plan['runtimeCapture'].update(plan=p['path'],planSHA256=p['sha256'],setupReceipt=s['path'],setupReceiptSHA256=s['sha256'])

    def policy(self):
        return {'metadata':{'uid':self.binding['policyUID'],'generation':10,'annotations':{}},'spec':copy.deepcopy(self.runtime['initialPolicySpec']),
                'status':{'incidentSequence':7,'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':10}],
                          'applicationFailureCircuit':{'state':'FAILED','signature':'f'*64},'failureHistory':[{'signature':'f'*64}]}}

    def test_nonzero_generation_and_cumulative_sequence_are_not_reset(self):
        pol=self.policy();before=copy.deepcopy(pol)
        self.assertEqual(policy_generations(self.runtime),(10,11,12,13));initial_policy(pol,self.runtime)
        _,payload=exhibition_permit(self.runtime,self.setup)
        self.assertEqual(payload['baseIncidentSequence'],7);self.assertEqual(pol,before)
        # The next authorized round uses the same Policy UID, but increasing sequence/generation.
        next_plan=copy.deepcopy(self.runtime);next_plan.update(initialPolicyGeneration=13,initialIncidentSequence=8)
        next_plan['executionScope']['policyGeneration']=15
        pol['metadata']['generation']=13;pol['status']['incidentSequence']=8;pol['status']['conditions'][0]['observedGeneration']=13
        initial_policy(pol,next_plan);self.assertEqual(policy_generations(next_plan),(13,14,15,16))
        with self.assertRaises(Stop):initial_policy(pol,self.runtime)

    def test_fixed_signed_grant_core_cannot_be_extended_by_runtime(self):
        at=self.begin+dt.timedelta(seconds=5)
        grant=granted_at(self.runtime,self.setup,at,598,self.payload)
        self.assertEqual({k:grant[k] for k in self.grant},self.grant)
        self.assertEqual(grant['heartbeatAt'],at.isoformat())
        with self.assertRaises(Stop):granted_at(self.runtime,self.setup,self.begin+dt.timedelta(seconds=601),599,self.payload)
        wrong=copy.deepcopy(self.setup);wrong['authorityController']['instance']='another-instance'
        with self.assertRaises(Stop):granted_at(self.runtime,wrong,at,598,self.payload)

    def test_repeat_preflight_checks_exact_signed_authority_without_setup(self):
        before=sorted(str(p) for p in self.root.rglob('*'))
        preflight(self.plan)
        self.assertEqual(before,sorted(str(p) for p in self.root.rglob('*')))
        with self.assertRaisesRegex(Stop,'cannot redeploy'):
            setup(None,self.runtime,{},lambda:None)

    def test_missing_or_changed_inner_permit_cannot_use_outer_authority(self):
        for change in ('missing','hash','auth'):
            original=copy.deepcopy(self.runtime)
            if change=='missing':self.runtime.pop('exhibitionPermit')
            if change=='hash':self.runtime['exhibitionPermit']={**self.cfg,'sha256':'0'*64}
            if change=='auth':self.runtime['authorizationId']='other-authorization'
            self.repin()
            with self.subTest(change=change),self.assertRaises(ControlError):preflight(self.plan)
            self.runtime=original;self.repin()

    def test_old_sequence_instance_and_unclean_authority_refused(self):
        for mutation in ('sequence','active','fence','stale-fence','grant','permit'):
            pol=self.policy()
            if mutation=='sequence':pol['status']['incidentSequence']=8
            if mutation=='active':pol['status']['activeIncident']={'name':'old'}
            if mutation=='fence':pol['status']['conditions']=[]
            if mutation=='stale-fence':pol['status']['conditions'][0]['observedGeneration']=9
            if mutation=='grant':pol['metadata']['annotations']['opsloop.io/execution-grant']='old'
            if mutation=='permit':pol['metadata']['annotations']['opsloop.io/exhibition-permit']='old'
            with self.subTest(mutation=mutation),self.assertRaises(Stop):initial_policy(pol,self.runtime)
        wrong=copy.deepcopy(self.setup);wrong['authorityController']['instance']='old-instance'
        with self.assertRaises(Stop):exhibition_permit(self.runtime,wrong)

    def test_foreign_run_or_modified_signature_rejected_before_any_trial(self):
        wrong=copy.deepcopy(self.runtime);wrong['runID']='old-run'
        with self.assertRaises(Stop):exhibition_permit(wrong,self.setup)
        env=json.loads(Path(self.cfg['path']).read_bytes());env['signature']=base64.b64encode(b'x'*64).decode()
        self.cfg.update(self.pin('permit.json',env));self.runtime['exhibitionPermit']=self.cfg;self.plan['exhibitionPermit']=self.cfg;self.repin()
        with self.assertRaisesRegex(ControlError,'SIGNATURE'):preflight(self.plan)

    def incident(self):
        return {'metadata':{'name':self.scope['incident'],'namespace':self.scope['namespace'],'uid':'first-incident-uid',
                            'annotations':{'opsloop.io/execution-grant-binding':'d'*64,'opsloop.io/exhibition-run':self.runtime['runID'],'opsloop.io/exhibition-epoch':self.runtime['epoch']}},
                'spec':{'policyRef':{'name':self.scope['policy'],'uid':self.scope['policyUID']},'policyGeneration':self.scope['policyGeneration'],'policyHash':self.scope['policyHash'],
                        'workload':{'name':self.scope['deployment'],'uid':self.scope['deploymentUID']}},
                'status':{'runtime':{'phase':'DETECTED'}}}

    def test_first_incident_identity_is_preserved_before_failure_capture(self):
        record={};inc=self.incident();saved=[]
        with patch('tools.exhibition_demo_b_trial.ROOT',self.root):
            remember_incident(inc,self.runtime,'d'*64,record,lambda:saved.append(copy.deepcopy(record)))
            raw=(self.root/'incident-identity.json').read_bytes()
            self.assertEqual(json.loads(raw)['uid'],'first-incident-uid')
            self.assertEqual(record['incidentIdentitySHA256'],digest(raw))
            self.assertEqual((self.root/'incident-identity.json').stat().st_mode&0o777,0o600)
            self.assertFalse((self.root/'incident-raw').exists())
            self.assertEqual(len(saved),1)
            inc['status']['runtime']['phase']='FAILED'
            remember_incident(inc,self.runtime,'d'*64,record,lambda:None)
            self.assertEqual((self.root/'incident-identity.json').read_bytes(),raw)
            inc['metadata']['uid']='replacement-same-name'
            with self.assertRaisesRegex(Stop,'UID replaced'):remember_incident(inc,self.runtime,'d'*64,record,lambda:None)
            self.assertEqual((self.root/'incident-identity.json').read_bytes(),raw)
            with self.assertRaisesRegex(Stop,'cannot restart'):remember_incident(self.incident(),self.runtime,'d'*64,{},lambda:None)
            with self.assertRaisesRegex(Stop,'cannot restart'):trial(None,self.runtime,self.setup,{},lambda:None)

    def test_unbound_incident_never_becomes_cleanup_identity(self):
        for change in ('grant','run','epoch','namespace','policy','deployment'):
            inc=self.incident()
            if change=='grant':inc['metadata']['annotations']['opsloop.io/execution-grant-binding']='e'*64
            if change=='run':inc['metadata']['annotations']['opsloop.io/exhibition-run']='old-run'
            if change=='epoch':inc['metadata']['annotations']['opsloop.io/exhibition-epoch']='old-epoch'
            if change=='namespace':inc['metadata']['namespace']='other'
            if change=='policy':inc['spec']['policyRef']['uid']='other-policy'
            if change=='deployment':inc['spec']['workload']['uid']='other-deployment'
            with self.subTest(change=change),patch('tools.exhibition_demo_b_trial.ROOT',self.root),self.assertRaises(Stop):
                remember_incident(inc,self.runtime,'d'*64,{},lambda:None)
            self.assertFalse((self.root/'incident-identity.json').exists())

    def test_fresh_seal_derives_v2_budget_without_extending_signed_authority(self):
        budget=engineering_budget(self.plan,'f'*64)
        self.assertEqual(budget['schema'],'opsloop-engineering-plan/v2')
        self.assertEqual(budget['bundle_sha256'],'f'*64)
        self.assertEqual(budget['exhibition'],{'authorization_id':self.plan['authorizationId'],'permit':self.cfg,'previous_completion_sha256':'c'*64})
        self.assertEqual(budget['max_model_calls'],1);self.assertTrue(budget['expires_at'].endswith('Z'))
        for key,value in [('maxModelCalls',2),('totalSeconds',899),('expiresAt',(self.begin+dt.timedelta(seconds=901)).isoformat())]:
            changed=copy.deepcopy(self.plan);changed[key]=value
            with self.subTest(key=key),self.assertRaises(ControlError):engineering_budget(changed,'f'*64)

    def test_existing_static_controller_binds_without_redeployment(self):
        target={k:self.scope[k] for k in ('clusterUID','namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}
        cfg={'schema':'opsloop-exhibition-authority/v1','publicKey':self.public,'targets':[target]}
        self.runtime.update(controllerUID='controller',controllerTemplate={'approved':'static'},controllerImageRef='repo@sha256:'+ 'd'*64,controllerImageID='sha256:'+'d'*64,controllerNode='control',staticAuthorityKey='authority.json',configMap={'metadata':{'name':'static-authority'},'data':{'authority.json':json.dumps(cfg)}})
        self.setup.update(controllerGeneration=9,authorityController={**self.setup['authorityController'],'podName':'stable-controller','podUID':'controller-pod','containerID':'container','imageID':self.runtime['controllerImageID'],'scopeConfigMapUID':'static-cm'})
        dep={'metadata':{'uid':'controller','generation':9},'spec':{'template':{'approved':'static'}},'status':{'readyReplicas':1}}
        pod={'metadata':{'uid':'controller-pod'},'spec':{'nodeName':'control','containers':[{'image':self.runtime['controllerImageRef']}]},'status':{'containerStatuses':[{'ready':True,'restartCount':0,'containerID':'container','imageID':self.runtime['controllerImageID']}]}}
        cm={'metadata':{'uid':'static-cm'},'immutable':True,'data':self.runtime['configMap']['data']}
        class ReadsOnly:
            def get(self,path):
                if '/deployments/' in path:return dep
                if '/pods/' in path:return pod
                if '/configmaps/' in path:return cm
                raise AssertionError(path)
        bind_controller(ReadsOnly(),self.runtime,self.setup)
        cm['data']={'authority.json':json.dumps({**cfg,'publicKey':'wrong'})}
        with self.assertRaises(Stop):bind_controller(ReadsOnly(),self.runtime,self.setup)


if __name__=='__main__':unittest.main()
