"""Synthetic CLI API transport; real issuance/materialization/admission, no model."""
from contextlib import closing
import concurrent.futures
import copy
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest import mock
from tests.test_exhibition_finite_rounds import FiniteFixture
from tools import exhibition_finite_cli as cli
from tools.exhibition_hashes import describe, digest
from tools.exhibition_inputs import pin


class CLIFixture(FiniteFixture):
    def put(self,name,value):
        path=super().put(name,value);path.chmod(0o600);return path

    def __init__(self, root, policy_hash='a'*64):
        super().__init__(root,policy_hash)
        self.plan['status']='SEALED_EXECUTION_CANDIDATE'
        self.receipt.update(podName='controller-pod',containerID='container-id',node='node',configMapUID='authority-uid')
        controller={'apiVersion':'apps/v1','kind':'Deployment','metadata':{'uid':'controller-uid','name':'controller','namespace':'opsloop-system','generation':7},
            'spec':{'replicas':1,'template':{'spec':{'containers':[{'name':'manager','image':'example/controller@sha256:'+'f'*64,
                'args':['--exhibition-authority-file=/etc/opsloop/execution/authority.json'],
                'volumeMounts':[{'name':'authority','mountPath':'/etc/opsloop/execution','readOnly':True}]}],
                'volumes':[{'name':'authority','configMap':{'name':'authority','items':[{'key':'authority.json','path':'authority.json'}]}}]}}},
            'status':{'observedGeneration':7,'replicas':1,'readyReplicas':1,'updatedReplicas':1,'availableReplicas':1}}
        self.objects.update(controller=controller,cluster={'metadata':{'uid':self.plan['clusterUID']}})
        self.plan['deployment']['controller'].update(namespace='opsloop-system',expectedSpecHash=describe(controller,'/spec'))
        self.objects['controller-replicasets']={'items':[{'metadata':{'uid':'controller-rs','ownerReferences':[{'uid':'controller-uid','controller':True}]}}]}
        self.objects['controller-pods']={'items':[{'metadata':{'uid':self.receipt['podUID'],'name':'controller-pod','ownerReferences':[{'uid':'controller-rs','controller':True}]},
            'spec':{'nodeName':'node'},'status':{'containerStatuses':[{'name':'manager','ready':True,'restartCount':0,'containerID':'container-id','imageID':self.receipt['imageID']}]}}]}
        self.objects['controller-log-'+self.receipt['podUID']]=json.dumps({'schema':'opsloop-controller-execution-identity/v1','instance':self.receipt['instance'],'enforceDefault':'deny-without-fresh-bound-grant'})
        e=self.plan['executionInputs'];i=e['integration'];e['authorityConfigName']='authority'
        self.plan['readinessContract']={'historicalJobs':[],'requiredEvidence':[]}
        self.target['observedIncidentSequence']=4
        pol=self.objects['b-policy'];pol['spec'].update(mode='ObserveOnly',budget={'maxActionsPerIncident':1})
        self.objects['b-policies']={'items':[pol]};self.objects['b-incidents']={'items':[]}
        self.objects['b-pods']={'items':[{'metadata':{'uid':'app-pod','ownerReferences':[{'kind':'ReplicaSet','uid':'baseline-rs','controller':True}]},'status':{'phase':'Running','conditions':[{'type':'Ready','status':'True'}]}}]}
        quota=self.objects['b-quotas']['items'][0]
        for k in ('spec','status'):quota[k]['hard']['count/recoverypolicies.opsloop.io']='1'
        quota['status']['used']['count/recoverypolicies.opsloop.io']='1'
        # A real Lifecycle restore must return to these same signed bytes.
        template=self.objects['b-deployment']['spec']['template']
        template['spec']['nodeSelector']={'node':'baseline'}
        template['metadata']['annotations']={'opsloop.io/attempt-id':'baseline'}
        template['spec']['containers'][0]['env'][5].pop('value',None)
        self.objects['b-replicasets']['items'][0]['spec']['template']=copy.deepcopy(template)
        self.target['baselineSpecHash']=describe(self.objects['b-deployment'],'/spec')
        recipe=self.intent['approvedReproduction'];recipe['baselineTemplate']=copy.deepcopy(template)
        recipe['template']=copy.deepcopy(template);recipe['template']['spec']['containers'][0]['env'][5]['value']='empty-order-after-30s'
        from tools.exhibition_replay import projection_hash
        self.bounded['approvedReproductionSHA256']=projection_hash(recipe)
        self.bound_ref=pin(self.put('cli-finite-approval',self.bounded));self.select(0)
        a=copy.deepcopy(self.target);a.update(policy='a-policy',policyUID='a-policy-uid',deployment='a-app',deploymentUID='a-app-uid')
        self.plan['fixedTargets']['a']=a
        for kind in ('deployment','policy','replicasets','quotas','pods','incidents'):self.objects['a-'+kind]=copy.deepcopy(self.objects['b-'+kind])
        self.objects['a-policy']['metadata']['uid']=a['policyUID'];self.objects['a-deployment']['metadata']['uid']=a['deploymentUID']
        a['baselineSpecHash']=describe(self.objects['a-deployment'],'/spec')
        self.objects['a-replicasets']['items'][0]['metadata']['ownerReferences'][0]['uid']=a['deploymentUID']
        self.objects['a-policies']={'items':[self.objects['a-policy']]}
        e['expectedCircuitHistory']['a']={'initialSignatures':[self.signature],'additions':[]}
        self.objects['b-service']={'metadata':{'uid':'service-b'},'spec':{'ports':[{'port':8080}]}}
        targets=[{'clusterUID':self.plan['clusterUID'],**{k:t[k] for k in ('namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}} for t in self.plan['fixedTargets'].values()]
        cm={'metadata':{'uid':'authority-uid','name':'authority'},'immutable':True,'data':{'authority.json':json.dumps({'schema':'opsloop-exhibition-authority/v1','publicKey':self.public,'targets':targets})}}
        self.objects['authority']=cm
        (root/'session').mkdir(mode=0o700)
        i.update(sessionRoot=str(root/'session'),controlState=str(root/'control'),archiveRoot=str(root/'archive'),apiOrigin='https://synthetic.invalid')
        helper=root/'synthetic-helper';helper.write_text('#!/bin/sh\nprintf "%s\\n" '+policy_hash+'\n');helper.chmod(0o700)
        for name in ('policyHash','driver','grant','quoteProbe','kubernetes','runtimeB','audit','kubectl','kubeconfig'):i[name]=pin(helper)
        i['provenance']=pin(self.put('provenance',{'repository':'owner/repo','base_commit':'a'*40}))
        i['engineering']={'state':str(root/'engineering'),'repository':'owner/repo','base_commit':'a'*40}
        i['modelAuthorization']={'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt','requests':1,'automaticFallback':False,'automaticRetry':False}
        e['baselineSpecs']={k:copy.deepcopy(self.objects[k+'-deployment']['spec']) for k in ('a','b')}
        for k in ('a','b'):
            baseline=e['baselineSpecs'][k];baseline['template']['spec']['nodeSelector']={'node':'baseline'}
            baseline['template']['metadata']['annotations']={'opsloop.io/attempt-id':'baseline'}
            self.plan['fixedTargets'][k]['readyBaselineSpecSHA256']=digest(self.objects[k+'-deployment']['spec'])
        self.plan['cleanupAndRollback']={'deploymentRestorePaths':['/spec/template/spec/nodeSelector','/spec/template/metadata/annotations/opsloop.io~1attempt-id']}
        sources=[pin(Path(cli.__file__).resolve()),pin(Path(cli.__file__).resolve().with_name('exhibition_resume.py'))]
        self.plan['newInterval']={'schema':cli.SCHEMA,'authorization':self.bound_ref,'sourcePins':sources,'sessionRoot':str(root/'session'),
            'issuerKey':pin(self.key),'controllerSnapshot':pin(self.put('static-controller',controller)),
            'controllerReceipt':pin(self.put('current-controller-receipt',self.receipt)),'authorityConfig':pin(self.put('static-authority',cm)),
            'continuation':{'predecessorCompletion':self.predecessor}}

    def reader(self,*args):
        f=self
        class Reader:
            def snapshot(self):return copy.deepcopy(f.objects)
        return Reader()


class FiniteCLITests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.f=CLIFixture(Path(self.tmp.name));self.auth=self.f.intent['authorizationID']
    def execute(self,**kwargs):
        with mock.patch.object(cli,'Reader',side_effect=self.f.reader):return cli.execute(self.f.plan,'synthetic-pinned-plan',**kwargs)
    def test_legacy_v4_mix_rejected_before_any_effect(self):
        from tools.exhibition_resume import execute
        plan=copy.deepcopy(self.f.plan);plan['newInterval'].pop('schema')
        with mock.patch('tools.exhibition_resume.private_dir') as mkdir,self.assertRaisesRegex(ValueError,'FINITE_EXPLICIT_OPT_IN_REQUIRED'):execute(plan,'synthetic')
        mkdir.assert_not_called();self.assertFalse((self.f.root/'issuer/finite-cli.sqlite3').exists())
    def test_explicit_cli_action_required(self):
        from tools.exhibition_resume import execute
        with self.assertRaisesRegex(ValueError,'FINITE_EXPLICIT_OPT_IN_REQUIRED'):execute(self.f.plan,'synthetic')
        with self.assertRaisesRegex(ValueError,'FINITE_ONE_ACTION_REQUIRED'):cli.execute(self.f.plan,'synthetic')
    def test_exact_current_controller(self):
        *_,expected,public=cli.inputs(self.f.plan,self.auth)
        receipt=cli.controller_receipt(self.f.plan,self.f.objects,expected,public)
        self.assertEqual(receipt['instance'],self.f.receipt['instance']);self.assertEqual(receipt['priorPodUID'],receipt['podUID'])
    def test_controller_identity_drift(self):
        *_,expected,public=cli.inputs(self.f.plan,self.auth)
        changes=[lambda o:o['controller']['metadata'].update(uid='other'),lambda o:o['controller']['metadata'].update(generation=8),
            lambda o:o['controller']['spec'].update(replicas=2),lambda o:o['controller-pods']['items'][0]['status']['containerStatuses'][0].update(restartCount=1),
            lambda o:o['controller-pods']['items'][0]['status']['containerStatuses'][0].update(containerID='other'),
            lambda o:o['controller-pods']['items'][0]['status']['containerStatuses'][0].update(imageID='other'),
            lambda o:o['authority']['metadata'].update(uid='other'),lambda o:o['authority']['data'].update({'authority.json':'{}'})]
        for change in changes:
            objects=copy.deepcopy(self.f.objects);change(objects)
            with self.subTest(change=change),self.assertRaises((ValueError,KeyError)):cli.controller_receipt(self.f.plan,objects,expected,public)
    def test_unknown_and_legacy_approval_cannot_reserve(self):
        with self.assertRaisesRegex(ValueError,'FINITE_SELECTED_SLOT_CHANGED'):self.execute(authorization_id='other')
        self.f.plan['newInterval']['authorization']=pin(self.f.put('legacy',{'schema':'opsloop-b-work-approval/v1','approved':True}))
        with self.assertRaisesRegex(ValueError,'FINITE_APPROVAL_REQUIRED'):self.execute(authorization_id=self.auth)
        self.assertFalse((self.f.root/'issuer/finite-cli.sqlite3').exists())
    def test_expired_intent_cannot_create_journal(self):
        future=cli.time.time()+3600
        with mock.patch.object(cli.time,'time',return_value=future),self.assertRaisesRegex(ValueError,'EXPIRED'):
            self.execute(authorization_id=self.auth)
        self.assertFalse((self.f.root/'issuer/finite-cli.sqlite3').exists())
    def test_source_pin_omission_or_changed_bytes_block_before_reservation(self):
        original=self.f.plan['newInterval']['sourcePins']
        for pins in ([], original[:1], [{**original[0],'sha256':'0'*64},original[1]]):
            self.f.plan['newInterval']['sourcePins']=pins
            with self.subTest(pins=pins),self.assertRaises(ValueError):self.execute(authorization_id=self.auth)
        self.assertFalse((self.f.root/'issuer/finite-cli.sqlite3').exists())
    def test_corrupt_predecessor_blocks_before_reservation(self):
        Path(self.f.predecessor['path']).write_text('{}')
        with self.assertRaises(ValueError):self.execute(authorization_id=self.auth)
        self.assertFalse((self.f.root/'issuer/finite-cli.sqlite3').exists())
    def test_expiry_after_final_database_close_tombstones_session(self):
        integration=self.f.plan['executionInputs']['integration'];cfg=self.f.plan['newInterval']
        clock=[100];original=sqlite3.connect
        class Connection:
            def __init__(self,*args,**kwargs):self.db=original(*args,**kwargs)
            def execute(self,*args):return self.db.execute(*args)
            def close(self):self.db.close();clock[0]=201
        with mock.patch.object(cli.time,'time',side_effect=lambda:clock[0]),mock.patch.object(cli.sqlite3,'connect',side_effect=Connection):
            with self.assertRaisesRegex(ValueError,'FINITE_SESSION_DEADLINE_PASSED'):
                with cli.journal(integration,deadline=200) as db:
                    cli.bind_session(db,cfg,integration)
                    db.execute('INSERT INTO slots VALUES (?,?,?,?,?)',(self.auth,'run',5,'COMPLETED','{}'))
        with closing(original(self.f.root/'issuer/finite-cli.sqlite3')) as db:
            self.assertEqual(db.execute('SELECT state FROM session').fetchone()[0],'EXPIRED')
        with self.assertRaisesRegex(ValueError,'FINITE_SESSION_CLOSED_OR_UNCONFIRMED'):self.execute(authorization_id=self.auth)
    def test_circuit_and_partial_baseline_prevent_issuance(self):
        for mode in ('circuit','baseline','fence'):
            original=copy.deepcopy(self.f.objects)
            if mode=='circuit':self.f.objects['b-policy']['status']['circuits'].append({'signature':'c'*64})
            if mode=='baseline':self.f.objects['b-deployment']['status']['readyReplicas']=0
            if mode=='fence':self.f.objects['b-policy']['status']['conditions']=[]
            with self.subTest(mode=mode),self.assertRaises(ValueError):self.execute(authorization_id=self.auth)
            self.f.objects=original
        with closing(sqlite3.connect(self.f.root/'issuer/issuance.sqlite3')) as db:self.assertEqual(db.execute('SELECT count(*) FROM permits').fetchone()[0],2)
    def test_close_and_slot_share_lock(self):
        with cli.journal(self.f.plan['executionInputs']['integration']):
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                results=[pool.submit(self.execute,**action) for action in ({'close':True},{'authorization_id':self.auth})]
                for result in results:
                    with self.assertRaisesRegex(ValueError,'FINITE_SESSION_BUSY'):result.result()
    def test_close_persists_without_claiming_controller_rollback(self):
        result=self.execute(close=True);self.assertEqual(result['state'],'FINITE_SESSION_CLOSED_CONTROLLER_UNCHANGED');self.assertFalse(result['nextRunIsAuthorized'])
        with self.assertRaisesRegex(ValueError,'FINITE_SESSION_CLOSED_OR_UNCONFIRMED'):self.execute(authorization_id=self.auth)
    def test_uncertain_close_cannot_reopen(self):
        self.f.objects['b-policy']['spec']['enabled']=True
        with self.assertRaisesRegex(ValueError,'CURRENT_FENCE_REQUIRED'):self.execute(close=True)
        self.f.objects['b-policy']['spec']['enabled']=False
        with self.assertRaisesRegex(ValueError,'FINITE_SESSION_CLOSED_OR_UNCONFIRMED'):self.execute(authorization_id=self.auth)
    def test_dispatch_loss_real_materialize_preflight_no_retry(self):
        from tools.exhibition_handoff import preflight
        original=subprocess.run;captures=[]
        def transport(argv,**kwargs):
            if '-m' in argv and 'tools.exhibition_session' in argv:
                control=json.loads(Path(argv[argv.index('--config')+1]).read_text());outer=json.loads(Path(control['plans'][0]['path']).read_text())
                preflight(outer);captures.append(outer);raise subprocess.TimeoutExpired(argv,1)
            return original(argv,**kwargs)
        with mock.patch('subprocess.run',side_effect=transport),self.assertRaises(subprocess.TimeoutExpired):self.execute(authorization_id=self.auth)
        self.assertEqual(len(captures),1);outer=captures[0]
        self.assertEqual(outer['totalSeconds'],900)
        runtime=json.loads(Path(outer['runtimeCapture']['plan']).read_text());self.assertEqual(runtime['maximumTrialMS'],600000)
        self.assertTrue((Path(outer['runtimeCapture']['root'])/'child-dispatch.json').is_file())
        with closing(sqlite3.connect(self.f.root/'issuer/issuance.sqlite3')) as db:self.assertEqual(db.execute('SELECT count(*) FROM permits').fetchone()[0],3)
        with self.assertRaisesRegex(ValueError,'FINITE_PRIOR_DISPATCH_UNCONFIRMED'):self.execute(authorization_id=self.auth)
        with self.assertRaisesRegex(ValueError,'FINITE_PRIOR_DISPATCH_UNCONFIRMED'):self.execute(close=True)


if __name__=='__main__':unittest.main()
