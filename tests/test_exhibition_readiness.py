"""Synthetic GET-only readiness, explicit historical fixtures and mutation controls."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from tools.exhibition_hashes import describe, encoded
from tools.exhibition_control import ControlError
from tools.exhibition_readiness import (SCHEMA, historical_record, historical_jobs, quota_headroom,
    MissingObservation, full_preflight, delete_inventory, saved_responses, verify_entrypoints)
from tools.exhibition_preflight import object_path
from tools.exhibition_plan import readiness_revision
from tests.test_exhibition_hash_contract import fixtures, build


def history(root):
    job={'apiVersion':'batch/v1','kind':'Job','metadata':{'name':'observer','namespace':'demo-a','uid':'history-job',
        'labels':{'opsloop.io/demo-a-observation':'old-observation'}},'spec':{'template':{'spec':{'containers':[{'name':'observer','image':'observer@sha256:123'}]}}},
        'status':{'completionTime':'2026-01-01T00:01:00Z','conditions':[{'type':'Complete','status':'True'}],'succeeded':1}}
    pod={'apiVersion':'v1','kind':'Pod','metadata':{'namespace':'demo-a','name':'observer-pod','uid':'history-pod',
        'labels':copy.deepcopy(job['metadata']['labels']),'ownerReferences':[{'kind':'Job','name':'observer','uid':'history-job','controller':True}]},
        'spec':{'containers':[{'name':'observer','image':'observer@sha256:123'}]},
        'status':{'phase':'Succeeded','containerStatuses':[{'name':'observer','ready':False,'state':{'terminated':{'exitCode':0,'finishedAt':'2026-01-01T00:01:00Z'}}}]}}
    old=root/'historical.json';old.write_bytes(encoded({'pod':pod}));old.chmod(0o600)
    ref={'path':str(old),'sha256':hashlib.sha256(old.read_bytes()).hexdigest(),'pointer':'/pod','expected':copy.deepcopy(pod)}
    record=historical_record(job,[pod],identity_proof=ref,purpose_proof=ref)
    return job,pod,record


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        prior,self.capture=fixtures();self.plan=build(prior,self.capture)
        self.job,self.pod,self.record=history(self.root)
        self.plan['clusterUID']='cluster';self.plan['timeWindow'].update(maxRuntimeGrantSeconds=600,maxCleanupSecondsPerRun=180)
        for key,t in self.plan['fixedTargets'].items():t['namespaceUID']='ns-'+key;t['quotaPolicyCount']=1
        for o in self.plan['migration']['legacyObjectsAllowed']:o['kind']='RecoveryIncident'
        self.plan['readinessContract']={'schema':SCHEMA,'entrypoint':'python3 -m tools.exhibition_preflight --full',
            'historicalJobs':[self.record],'writesDuringPreflight':0,'unknownObjects':'BLOCK',
            'quotaAdditional':{k:{'count/recoverypolicies.opsloop.io':0,'count/jobs.batch':0,'count/pods':1} for k in ('a','b','system')},
            'comparisons':[],'requiredEvidence':[{'id':k,'kind':'exact-evidence','path':'/missing/'+k,'equals':[{'pointer':'/value','value':'required'}],'basis':'/preflight','purpose':'Synthetic missing dependency'} for k in ('controller.instance','registry.tag','operator.permissions','host.stop','host.model-budget','tools.entrypoints','nodes','schema.policy','schema.incident','controller.static-config','controller.rbac','system.quota','metrics','a.service','b.service')]}
        self.values={object_path(describe(o.obj,'/')['object']):copy.deepcopy(o.obj) for o in self.capture.values()}
        self.values['/api/v1/namespaces/kube-system']={'metadata':{'uid':'cluster'}}
        for key,t in self.plan['fixedTargets'].items():
            ns=t['namespace'];core='/api/v1/namespaces/'+ns;ops='/apis/opsloop.io/v1alpha1/namespaces/'+ns
            self.values[core]={'metadata':{'uid':t['namespaceUID']}}
            self.values[ops+'/recoverypolicies?limit=100']={'items':[self.values[ops+'/recoverypolicies/fixed']]}
            self.values[ops+'/recoveryincidents?limit=100']={'items':[self.values[ops+'/recoveryincidents/incident']]}
            self.values[core+'/pods?limit=100']={'items':[copy.deepcopy(self.pod)] if key=='a' else []}
            self.values['/apis/batch/v1/namespaces/'+ns+'/jobs?limit=100']={'items':[copy.deepcopy(self.job)] if key=='a' else []}
            self.values['/apis/apps/v1/namespaces/'+ns+'/replicasets?limit=100']={'items':[]}
            hard={'count/recoverypolicies.opsloop.io':'1','count/jobs.batch':'2','count/pods':'4'}
            self.values[core+'/resourcequotas?limit=100']={'items':[{'metadata':{'name':'bound'},'spec':{'hard':hard},'status':{'hard':hard,'used':{'count/recoverypolicies.opsloop.io':'1','count/jobs.batch':'0','count/pods':'0'}}}]}
            self.values['/apis/apps/v1/namespaces/'+ns+'/deployments/app']['status']['availableReplicas']=1
        # Deliberately don't alter the pinned Policy spec to manufacture a CPU PASS.
        self.reads=[]
    def tearDown(self):self.tmp.cleanup()
    def get(self,path):
        self.reads.append(('GET',path))
        if path not in self.values:raise MissingObservation()
        return self.values[path]
    def run_report(self):
        with patch('tools.exhibition_preflight.verify_sources'):
            return full_preflight(self.plan,self.get)
    def check(self,name):return next(r for r in self.run_report()['checks'] if r['id']==name)
    def test_known_completed_history_passes_same_full_preflight(self):
        row=self.check('a.historical-jobs');self.assertEqual(row['status'],'PASS')
        self.assertEqual(row['details']['preservedPodCount'],1)
        self.assertEqual(self.check('a.quota')['status'],'PASS')
        self.assertEqual(self.run_report()['scope'],'SAVED_RESPONSE_LOCAL_ONLY')
        self.assertFalse(self.run_report()['executionAuthorized']);self.assertTrue(all(m=='GET' for m,_ in self.reads))
    def test_no_raw_spec_or_credentials_reflected_in_check_details(self):
        report=self.run_report();self.assertNotIn('MESSAGE',json.dumps(report));self.assertNotIn('MODE',json.dumps(report))
    def test_replaced_same_named_job_uid_rejected(self):
        self.values['/apis/batch/v1/namespaces/demo-a/jobs?limit=100']['items'][0]['metadata']['uid']='replacement'
        self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_running_terminating_missing_status_pods_rejected(self):
        base=copy.deepcopy(self.values['/api/v1/namespaces/demo-a/pods?limit=100'])
        for mode in ('running','terminating','unknown','missing-container','wrong-owner','other-uid'):
            self.values['/api/v1/namespaces/demo-a/pods?limit=100']=copy.deepcopy(base)
            p=self.values['/api/v1/namespaces/demo-a/pods?limit=100']['items'][0]
            if mode=='running':p['status']['phase']='Running'
            if mode=='terminating':p['metadata']['deletionTimestamp']='2026-01-01T00:01:01Z'
            if mode=='unknown':p['status']['containerStatuses'][0]['state']={'waiting':{}}
            if mode=='missing-container':p['status'].pop('containerStatuses')
            if mode=='wrong-owner':p['metadata']['ownerReferences'][0]['uid']='other'
            if mode=='other-uid':p['metadata']['uid']='replaced'
            with self.subTest(mode=mode):self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_active_zero_or_succeeded_one_is_not_terminal_proof(self):
        j=self.values['/apis/batch/v1/namespaces/demo-a/jobs?limit=100']['items'][0]
        j['status']={'active':0,'succeeded':1}
        self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_job_terminating_or_uncounted_pods_blocks(self):
        j=self.values['/apis/batch/v1/namespaces/demo-a/jobs?limit=100']['items'][0]
        for field,value in [('terminating',1),('uncountedTerminatedPods',{'succeeded':['pending']})]:
            j['status'][field]=value;self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL');j['status'].pop(field)
    def test_unknown_job_cannot_self_allowlist(self):
        j=copy.deepcopy(self.job);j['metadata'].update(name='unknown',uid='unknown')
        self.values['/apis/batch/v1/namespaces/demo-a/jobs?limit=100']['items'].append(j)
        self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_historical_proof_and_purpose_tampering_rejected(self):
        self.record['purpose']='different';self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
        self.record['purpose']='old-observation';Path(self.record['identityProof']['path']).write_text('{}')
        self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_current_run_annotation_blocks_history(self):
        j=self.values['/apis/batch/v1/namespaces/demo-a/jobs?limit=100']['items'][0]
        j['metadata']['annotations']={'opsloop.io/exhibition-run':'current'}
        self.assertEqual(self.check('a.historical-jobs')['status'],'FAIL')
    def test_complete_objects_count_against_count_quota_even_if_status_lags(self):
        q=self.values['/api/v1/namespaces/demo-a/resourcequotas?limit=100']['items'][0]
        q['spec']['hard']['count/jobs.batch']='0';q['status']['hard']['count/jobs.batch']='0'
        self.assertEqual(self.check('a.quota')['status'],'FAIL')
    def test_no_room_for_next_object_rejects(self):
        q=self.values['/api/v1/namespaces/demo-a/resourcequotas?limit=100']['items'][0]
        q['spec']['hard']['count/pods']='1';q['status']['hard']['count/pods']='1'
        self.assertEqual(self.check('a.quota')['status'],'FAIL')
    def test_quota_future_deletions_not_subtracted(self):
        q={'items':[{'metadata':{'name':'q'},'spec':{'hard':{'count/jobs.batch':'1'}},'status':{'hard':{'count/jobs.batch':'1'},'used':{'count/jobs.batch':'1'}}}]}
        with self.assertRaises(ControlError):quota_headroom(q,{'count/jobs.batch':0},{'count/jobs.batch':1})
    def test_independent_failures_collected_missing_not_assumed_pass(self):
        self.values['/apis/apps/v1/namespaces/demo-a/deployments/app']['spec']['replicas']=2
        self.values['/apis/opsloop.io/v1alpha1/namespaces/demo-b/recoverypolicies/fixed']['status']['conditions']=[]
        del self.values['/apis/opsloop.io/v1alpha1/namespaces/demo-b/recoverypolicies?limit=100']
        rows={r['id']:r for r in self.run_report()['checks']}
        self.assertEqual(rows['raw.a-deployment']['status'],'FAIL');self.assertEqual(rows['b.fence']['status'],'FAIL')
        self.assertEqual(rows['b.policy-count']['status'],'NOT_CHECKED');self.assertEqual(rows['a.historical-jobs']['status'],'PASS')
        self.assertTrue(all(method=='GET' for method,_ in self.reads));self.assertEqual(self.run_report()['mutationRequests'],0)
    def test_historical_jobs_never_migration_or_cleanup_inventory(self):
        result=delete_inventory(self.plan);self.assertEqual({r['uid'] for r in result},{'inc-a','inc-b'})
        self.plan['migration']['legacyObjectsAllowed'].append({'kind':'Job','uid':'history-job'})
        with self.assertRaisesRegex(ControlError,'HISTORICAL_DELETE'):delete_inventory(self.plan)
        from tools.exhibition_lifecycle import Lifecycle
        with self.assertRaises(ControlError):Lifecycle(self.root/'state')._object({'namespace':'demo-a','binding':{'runID':'new','epoch':'new'}},self.job,{'kind':'Job','uid':'history-job','name':'observer','resourceVersion':'1'})
    def test_plan_generator_and_execution_use_same_complete_checks(self):
        contract=copy.deepcopy(self.plan['readinessContract']);old=copy.deepcopy(self.plan)
        with patch('tools.exhibition_preflight.verify_sources'):
            revised,report=readiness_revision(self.plan,contract,self.get,previous_ref={'path':'time-r2','sha256':'old'})
            actual=full_preflight(revised,self.get)
        self.assertEqual(report,actual);self.assertEqual(self.plan,old)
        self.assertNotIn('approvalValidUntil',revised);self.assertEqual(revised['status'],'LOCAL_DRAFT_NOT_EXECUTABLE')
    def test_read_only_window_does_not_reuse_expired_execution_authority(self):
        import datetime as dt
        from tools.exhibition_preflight import main
        file=self.root/'draft.json';file.write_text(json.dumps(self.plan));file.chmod(0o600)
        end=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=10)).isoformat()
        args=['preflight','--full','--plan',str(file),'--sha256',hashlib.sha256(file.read_bytes()).hexdigest(),'--live-read-until',end]
        with patch('sys.argv',args),patch('tools.exhibition_readiness.full_preflight',return_value={'state':'NO_GO'}) as check,patch('subprocess.run') as network,patch('builtins.print'):
            with self.assertRaises(SystemExit) as error:main()
        self.assertEqual(error.exception.code,2);check.assert_called_once();network.assert_not_called()

    def test_saved_response_requires_raw_domain_and_bytes(self):
        raw=self.root/'data';raw.write_text('{}');raw.chmod(0o600)
        catalog=self.root/'responses';value={'schema':'opsloop-readonly-responses/v1','mutationRequests':0,'records':[{'path':'/target','method':'GET','representation':'display-projection','observedAt':'yesterday','source':{'path':str(raw),'sha256':hashlib.sha256(raw.read_bytes()).hexdigest()}}]}
        catalog.write_text(json.dumps(value));catalog.chmod(0o600)
        get=saved_responses(catalog,hashlib.sha256(catalog.read_bytes()).hexdigest())
        with self.assertRaisesRegex(ControlError,'RAW_RESPONSE'):get('/target')
        with self.assertRaises(MissingObservation):get('/missing')

if __name__=='__main__':unittest.main()
