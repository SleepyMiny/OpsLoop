"""Host migration synthetic custody tests; no cluster/model execution."""
import copy
import hashlib
import json
from pathlib import Path
import unittest
from unittest.mock import Mock
from tools.exhibition_control import ControlError
from tools.exhibition_hashes import describe
from tools.exhibition_lifecycle import RUN,EPOCH,verify_completion
from tools.exhibition_migration import migrate
from tests import test_exhibition_lifecycle as fixtures
API=fixtures.API
digest=fixtures.digest


class MigrationTests(unittest.TestCase):
    pin=fixtures.LifecycleTests.pin
    def setUp(self):
        fixtures.LifecycleTests.setUp(self)
        self.object['metadata']['annotations']={'opsloop.io/execution-grant-binding':'original-grant-hash'}
        parent=self
        class MigrationAPI(API):
            def request(self,path,patch=None,absent=False):
                if path=='/api/v1/namespaces/kube-system':return {'metadata':{'uid':'cluster'}}
                if path=='/api/v1/namespaces/demo':return {'metadata':{'uid':'namespace'}}
                return super().request(path,patch=patch,absent=absent)
        self.api=MigrationAPI(self.policy,self.dep,{'incident-38':self.object})
        self.plan={'schema':'opsloop-exhibition-migration/v1','approved':True,'approvalReference':'SYNTHETIC_ONLY',
            'migrationID':'migration-1','expiresAt':self.cfg['expiresAt'],
            'target':{'clusterUID':'cluster','namespaceUID':'namespace','namespace':'demo','policy':'fixed',
                      'policyUID':'fixed-policy','deployment':'app','deploymentUID':'fixed-workload'},
            'modelChecks':[{'state':str(self.root/'old-runner'),'binding':self.binding}],
            'evidenceRoot':str(self.root/'evidence'),'archiveRoot':str(self.root/'migration-archive'),
            'stateRoot':str(self.root/'migration-state'),'baselineSpecSHA256':digest(self.spec),'baselineSpecHash':describe(self.dep,'/spec'),
            'policySpecSHA256':digest(self.policy['spec']),'policySpecHash':describe(self.policy,'/spec'),'legacyAuthoritySHA256':digest({}),
            'objects':[{'kind':'RecoveryIncident','name':'incident-38','uid':'incident-uid','resourceVersion':'3',
                        'sha256':digest(self.object),'hash':describe(self.object,'/'),'grantBinding':'original-grant-hash'}],
            'evidence':self.receipt['evidence']}
        self.file=self.root/'migration-plan.json';self.model=Mock(return_value=[])
    def execute(self):
        self.file.write_text(json.dumps(self.plan));self.file.chmod(0o600)
        return migrate(self.file,hashlib.sha256(self.file.read_bytes()).hexdigest(),transport_factory=lambda _:self.api,model_checker=self.model)
    def test_legacy_custody_preserved_without_current_run_adoption(self):
        original=copy.deepcopy(self.object);policy=copy.deepcopy(self.policy)
        result=self.execute();value=verify_completion(result['completion'],result['sha256'])
        self.assertEqual(value['schema'],'opsloop-exhibition-migration-completion/v1')
        self.assertNotIn('runID',value['binding']);self.assertEqual(value['baseIncidentSequence'],37)
        manifest=json.loads(Path(value['archiveManifest']).read_text());record=manifest['objects'][0]
        archived=json.loads((Path(value['archiveManifest']).parent/record['sha256']).read_bytes())
        self.assertEqual(archived,original);self.assertNotIn(RUN,archived['metadata']['annotations'])
        self.assertEqual(self.policy,policy);self.assertEqual(self.api.deleted,[('incident-38','incident-uid','3')])
        self.assertEqual(self.execute(),result);self.assertEqual(len(self.api.deleted),1)
    def test_unexpected_or_missing_original_never_deleted(self):
        self.object['metadata']['uid']='unexpected'
        with self.assertRaisesRegex(ControlError,'MIGRATION_INVENTORY_INCOMPLETE'):self.execute()
        self.api.objects.clear()
        with self.assertRaisesRegex(ControlError,'MISSING_WITHOUT_CUSTODY'):self.execute()
        self.assertFalse(self.api.deleted)
    def test_active_object_rejected_even_if_hash_pinned(self):
        self.object['status']['runtime']['phase']='EXECUTING';self.plan['objects'][0]['sha256']=digest(self.object);self.plan['objects'][0]['hash']=describe(self.object,'/')
        with self.assertRaisesRegex(ControlError,'INCIDENT_ACTIVE'):self.execute()
        self.assertFalse(self.api.deleted)
    def test_model_unknown_stop_blocks_migration(self):
        self.model.side_effect=ControlError('MODEL_OWNER_STILL_ACTIVE')
        with self.assertRaisesRegex(ControlError,'MODEL_OWNER'):self.execute()
        self.assertFalse(self.api.deleted)
    def test_unapproved_migration_rejected(self):
        self.plan['approved']=False
        with self.assertRaisesRegex(ControlError,'MIGRATION_APPROVAL_REQUIRED'):self.execute()
        self.model.assert_not_called()
    def test_explicit_old_authority_retirement_preserves_other_policy_fields(self):
        self.policy['metadata']['annotations']={'opsloop.io/execution-grant':'historical-opaque-approved-value','keep':'yes'}
        self.plan['legacyAuthoritySHA256']=digest({'opsloop.io/execution-grant':'historical-opaque-approved-value'})
        self.plan['retireAuthority']=True
        result=self.execute()
        self.assertEqual(self.policy['metadata']['annotations'],{'keep':'yes'})
        self.assertEqual(self.policy['status']['incidentSequence'],37)
        self.assertEqual(self.policy['status']['failureHistory'],['preserved'])
        self.assertTrue(verify_completion(result['completion'],result['sha256'])['nextRunAllowed'])
    def test_unlisted_policy_incident_blocks_instead_of_adoption(self):
        other=copy.deepcopy(self.object);other['metadata'].update(name='unlisted',uid='unlisted-uid')
        self.api.objects['unlisted']=other
        with self.assertRaisesRegex(ControlError,'MIGRATION_INVENTORY_INCOMPLETE'):self.execute()
        self.assertFalse(self.api.deleted)

    def test_original_grant_binding_required(self):
        self.plan['objects'][0]['grantBinding']='other'
        with self.assertRaisesRegex(ControlError,'GRANT_CUSTODY'):self.execute()
        self.assertFalse(self.api.deleted)

if __name__=='__main__':unittest.main()
