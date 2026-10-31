"""No-network preparation entrypoint tests; fake APIs record every write."""
import base64
import copy
import datetime as dt
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from tools.exhibition_control import ControlError
from tools.exhibition_hashes import describe, encoded
from tools.exhibition_preparation import (controller_transition, deploy_once, restore_controller_once, publish_once, authority)


class PreparationTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        self.before={'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':'controller','namespace':'system','uid':'controller-uid','resourceVersion':'123','generation':7},
            'spec':{'replicas':1,'strategy':{'type':'Recreate'},'template':{'spec':{'serviceAccountName':'fenced',
              'securityContext':{'runAsNonRoot':True},'containers':[{'name':'manager','image':'old@sha256:123','args':['--leader-elect=true','--execution-scope-file=/etc/opsloop/execution/scope.json'],
                'resources':{'limits':{'cpu':'100m'}},'volumeMounts':[{'name':'scope','mountPath':'/etc/opsloop/execution','readOnly':True}]}],
              'volumes':[{'name':'scope','configMap':{'name':'old-scope','defaultMode':292,'items':[{'key':'scope.json','path':'scope.json'}]}}]}}}}
        self.plan={'status':'SEALED_EXECUTION_CANDIDATE','authority':{'order':['A1','A2','B1','B2'],'maxNewPermits':4},'clusterUID':'cluster',
            'fixedTargets':{key:{'namespace':'demo-'+key,'namespaceUID':'ns-'+key,'policy':'fixed','policyUID':'pol-'+key,'deployment':'app','deploymentUID':'dep-'+key} for key in ('a','b')},
            'deployment':{'controller':{'namespace':'system','name':'controller','uid':'controller-uid','expectedSpecHash':describe(self.before,'/spec'),
                'serviceAccountUnchanged':'fenced','staticConfigMap':{'name':'new-authority'}},
                'imagePublication':{'repository':'registry.invalid/controller','digest':'sha256:'+'a'*64,'newTag':'candidate','maxPushAttempts':1,'localOCI':'unused'}}}
        now=dt.datetime.now(dt.timezone.utc)
        self.session={'approvedPlanSHA256':'approved','planContentSHA256':hashlib.sha256(encoded(self.plan)).hexdigest(),
            'approvedAt':(now-dt.timedelta(seconds=20)).isoformat(),'startedAt':(now-dt.timedelta(seconds=10)).isoformat(),
            'preparationDeadline':(now+dt.timedelta(minutes=15)).isoformat(),'deadline':(now+dt.timedelta(minutes=80)).isoformat(),
            'completionDeadline':(now+dt.timedelta(minutes=85)).isoformat(),'livePreflightState':'LIVE_CHECKS_PASS','livePreflightScope':'CURRENT_GET_ONLY',
            'modelChecks':[{'state':'private-runner','binding':{'policyUID':'pol-b','deploymentUID':'dep-b'}}]}
        self.public=base64.b64encode(bytes(range(32))).decode();parent=self
        class API:
            def __init__(self):self.current=copy.deepcopy(parent.before);self.cm=None;self.writes=[]
            def get(self,path,absent=False):
                if '/configmaps/' in path:return self.cm
                if '/recoverypolicies/' in path:
                    k='a' if '/demo-a/' in path else 'b'
                    return {'metadata':{'uid':'pol-'+k,'generation':8},'spec':{'enabled':False},'status':{'conditions':[{'type':'Quiesced','status':'True','reason':'FenceClosed','observedGeneration':8}]}}
                return copy.deepcopy(self.current)
            def create(self,path,body):
                self.writes.append(('CREATE',path));self.cm=copy.deepcopy(body);self.cm['metadata']['uid']='created-cm';return self.cm
            def patch(self,path,body):
                from tools.exhibition_lifecycle import patched
                self.writes.append(('PATCH',path));self.current=patched(self.current,body)
                self.current['metadata']['resourceVersion']=str(int(self.current['metadata']['resourceVersion'])+1);self.current['metadata']['generation']+=1
                return copy.deepcopy(self.current)
        self.api=API()
    def tearDown(self):self.tmp.cleanup()
    def deploy(self):return deploy_once(self.plan,'approved',self.session,self.root,self.api,self.public)
    def test_exact_deploy_changes_only_image_argument_and_configmap_reference(self):
        change=controller_transition(self.plan,self.before,self.public);after=copy.deepcopy(change['after'])
        s=after['spec']['template']['spec'];old=self.before['spec']['template']['spec']
        self.assertEqual(s['serviceAccountName'],old['serviceAccountName']);self.assertEqual(s['securityContext'],old['securityContext'])
        self.assertEqual(s['volumes'][0]['configMap']['defaultMode'],292)
        s['containers'][0]['image']=old['containers'][0]['image'];s['containers'][0]['args']=old['containers'][0]['args'];s['volumes']=old['volumes']
        self.assertEqual(after,self.before)
        config=json.loads(change['configMap']['data']['authority.json']);self.assertEqual(len(config['targets']),2)
        self.assertEqual(config['publicKey'],self.public);self.assertNotIn('private',json.dumps(change))
    def test_deploy_once_never_creates_policy_or_changes_rbac(self):
        result=self.deploy();self.assertEqual(len(self.api.writes),2)
        self.assertEqual(self.api.writes[0][0],'CREATE');self.assertIn('/configmaps',self.api.writes[0][1])
        with self.assertRaises(ControlError):self.deploy()
        self.assertEqual(len(self.api.writes),2)
    def test_invalid_spec_or_failed_readiness_zero_writes(self):
        self.api.current['spec']['template']['spec']['containers'][0]['image']='tampered'
        with self.assertRaises(ControlError):self.deploy()
        self.api.current=copy.deepcopy(self.before)
        for state,scope in [('NO_GO','CURRENT_GET_ONLY'),('LOCAL_CHECKS_PASS','SAVED_RESPONSE_LOCAL_ONLY'),('LIVE_CHECKS_PASS','SAVED_RESPONSE_LOCAL_ONLY')]:
            self.session.update(livePreflightState=state,livePreflightScope=scope)
            with self.subTest(state=state,scope=scope),self.assertRaises(ControlError):self.deploy()
        self.assertEqual(self.api.writes,[])
    def test_expired_or_wrong_approval_zero_writes(self):
        self.session['approvedPlanSHA256']='different'
        with self.assertRaises(ControlError):self.deploy()
        self.session['approvedPlanSHA256']='approved';self.session['preparationDeadline']='2000-01-01T00:00:00+00:00'
        with self.assertRaises(ControlError):self.deploy()
        self.assertEqual(self.api.writes,[])
    def test_unconfirmed_create_not_replayed(self):
        original=self.api.create
        def ambiguous(*args):original(*args);raise TimeoutError()
        self.api.create=ambiguous
        with self.assertRaises(TimeoutError):self.deploy()
        self.api.cm=None
        with self.assertRaises(FileExistsError):self.deploy()
        self.assertEqual(len(self.api.writes),1)
    def test_restore_exact_fields_after_fences_and_model_check(self):
        self.deploy();checker=Mock(return_value=[])
        restored=restore_controller_once(self.plan,'approved',self.session,self.root,self.api,checker)
        checker.assert_called_once();self.assertEqual(restored['spec'],self.before['spec'])
        with self.assertRaises(ControlError):restore_controller_once(self.plan,'approved',self.session,self.root,self.api,checker)
        self.assertEqual(len(self.api.writes),3)
    def test_unknown_model_stop_prevents_restore(self):
        self.deploy();checker=Mock(side_effect=ControlError('MODEL_UNCONFIRMED'))
        with self.assertRaises(ControlError):restore_controller_once(self.plan,'approved',self.session,self.root,self.api,checker)
        self.assertEqual(len(self.api.writes),2)
    def test_unknown_policy_fence_prevents_restore(self):
        self.deploy();orig=self.api.get
        def unfenced(path,**kwargs):
            v=orig(path,**kwargs)
            if '/recoverypolicies/' in path:v['spec']['enabled']=True
            return v
        self.api.get=unfenced
        with self.assertRaises(ControlError):restore_controller_once(self.plan,'approved',self.session,self.root,self.api,Mock())
        self.assertEqual(len(self.api.writes),2)
    def registry(self):
        r=Mock();r.head.return_value=404;r.create_tag.return_value={'status':201,'digest':self.plan['deployment']['imagePublication']['digest']};r.get_tag.return_value=b'index';return r
    def publish(self,registry):
        with patch('tools.exhibition_preparation.oci_payload',return_value=(b'index',[],{})):
            return publish_once(self.plan,'approved',self.session,self.root,registry)
    def test_publication_once_exact_digest_conditional_tag(self):
        r=self.registry();self.publish(r)
        self.assertEqual(r.head.call_count,2);r.create_tag.assert_called_once_with(self.plan['deployment']['imagePublication'],b'index',if_none_match='*')
        with self.assertRaises(FileExistsError):self.publish(r)
        self.assertEqual(r.publish_digest.call_count,1)
    def test_existing_or_unconfirmed_tag_no_publication(self):
        for status in (200,401,403,500):
            r=self.registry();r.head.return_value=status
            with self.assertRaises(ControlError):self.publish(r)
            r.publish_digest.assert_not_called();r.create_tag.assert_not_called()
    def test_publication_ambiguous_result_is_not_replayed(self):
        r=self.registry();r.publish_digest.side_effect=TimeoutError()
        with self.assertRaises(TimeoutError):self.publish(r)
        with self.assertRaises(FileExistsError):self.publish(r)
        self.assertEqual(r.publish_digest.call_count,1);r.create_tag.assert_not_called()

if __name__=='__main__':unittest.main()
