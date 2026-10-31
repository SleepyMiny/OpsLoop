"""Ephemeral synthetic signing keys only; no operational credentials or model."""
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
from tools.exhibition_control import ControlError, canonical
from tools.exhibition_permit import verify, issue, bound_permit, PUBLIC_DER_PREFIX
from tools.engineering_runner.runner import Ledger
from tools.engineering_runner.contracts import Rejected
from tests.test_engineering_runner import bundle


def fixture(root, suffix='1', key=None):
    key=key or root/'test-key.pem'
    if not key.exists():
        subprocess.run(['/usr/bin/openssl','genpkey','-algorithm','ED25519','-out',str(key)],check=True,capture_output=True);os.chmod(key,0o600)
    pub=subprocess.run(['/usr/bin/openssl','pkey','-in',str(key),'-pubout','-outform','DER'],check=True,capture_output=True).stdout
    assert pub.startswith(PUBLIC_DER_PREFIX)
    public=base64.b64encode(pub[len(PUBLIC_DER_PREFIX):]).decode()
    now=dt.datetime.now(dt.timezone.utc)
    iso=lambda seconds:(now+dt.timedelta(seconds=seconds)).strftime('%Y-%m-%dT%H:%M:%SZ')
    scope={'clusterUID':'cluster','namespaceUID':'ns','namespace':'demo-b','policy':'fixed','policyUID':'policy_uid-1','policyGeneration':7,'policyHash':'a'*64,'deployment':'engineering-demo','deploymentUID':'deployment_uid-1','incident':'fixed-policy_u-'+str(int(suffix)+1).zfill(5),'runID':'run-'+suffix,'epoch':'epoch-'+suffix}
    payload={'schema':'opsloop-exhibition-permit/v1','grant':{'version':'opsloop-execution-grant/v1','scope':scope,'instance':'c'*64,'notBefore':iso(-60),'notAfter':iso(500)},'authorizationID':'auth-'+suffix,'ordinal':int(suffix),'baseIncidentSequence':int(suffix),'maxIncidents':1,'maxActions':1,'previousCompletionSHA256':'b'*64,'allowedCircuitSignatures':[],'modelBudget':{'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt','maxModelCalls':1,'totalSeconds':900,'expiresAt':iso(800)}}
    def sign(p):
        raw=canonical(p).encode();path=root/('payload-'+suffix);path.write_bytes(raw)
        sig=subprocess.run(['/usr/bin/openssl','pkeyutl','-sign','-inkey',str(key),'-rawin','-in',str(path)],check=True,capture_output=True).stdout
        return {'payload':base64.b64encode(raw).decode(),'signature':base64.b64encode(sig).decode()}
    envelope=sign(payload); path=root/('permit-'+suffix+'.json');path.write_text(canonical(envelope))
    pin={'path':str(path),'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'publicKey':public}
    b=bundle();b['incident'].update(uid='incident-'+suffix,run_id=scope['runID'],epoch=scope['epoch']);b['runtime']['attempts'][0].update(patched_at=iso(-50),completed_at=iso(-10));b['authorization']['expires_at']=iso(1000)
    plan={'schema':'opsloop-engineering-plan/v2','model':'gpt-6-luna','reasoning_effort':'medium','max_model_calls':1,'total_seconds':900,'expires_at':payload['modelBudget']['expiresAt'],'exhibition':{'authorization_id':payload['authorizationID'],'permit':pin,'previous_completion_sha256':payload['previousCompletionSHA256']}}
    return payload,envelope,pin,b,plan,key,sign


class PermitTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.payload,self.envelope,self.pin,self.bundle,self.plan,self.key,self.sign=fixture(self.root)
    def test_go_signed_golden_verifies_exact_bytes(self):
        golden=json.loads((Path(__file__).resolve().parents[1]/'internal/controller/testdata/exhibition-permit-golden.json').read_bytes())
        payload=verify(golden['envelope'],golden['publicKey'],current=False)
        self.assertEqual(payload['schema'],'opsloop-exhibition-permit/v1')

    def test_valid_signature_and_payload_binding(self):
        self.assertEqual(verify(self.envelope,self.pin['publicKey']),self.payload)
        plan={'authorizationId':'auth-1','binding':{k:self.payload['grant']['scope'][k] for k in ('runID','epoch','policyUID','deploymentUID')},'exhibitionPermit':self.pin}
        self.assertEqual(bound_permit(plan)[1],self.payload)
        plan['binding']['runID']='old-run'
        with self.assertRaises(ControlError):bound_permit(plan)
    def test_forgery_time_budget_and_unknown_fields_rejected(self):
        bad=copy.deepcopy(self.envelope);bad['signature']=base64.b64encode(b'x'*64).decode()
        with self.assertRaises(ControlError):verify(bad,self.pin['publicKey'])
        for mutate in (lambda p:p.update(extra=True),lambda p:p.update(maxIncidents=2),lambda p:p['modelBudget'].update(maxModelCalls=2),lambda p:p['grant'].update(notAfter='2020-01-01T00:00:00Z')):
            p=copy.deepcopy(self.payload);mutate(p)
            with self.subTest(payload=p),self.assertRaises(ControlError):verify(self.sign(p),self.pin['publicKey'])
    def test_duplicate_payload_fields_rejected_even_if_signed(self):
        raw=canonical(self.payload).encode()[:-1]+b',"maxActions":1}'
        path=self.root/'duplicate';path.write_bytes(raw)
        sig=subprocess.run(['/usr/bin/openssl','pkeyutl','-sign','-inkey',str(self.key),'-rawin','-in',str(path)],check=True,capture_output=True).stdout
        with self.assertRaises(ControlError):verify({'payload':base64.b64encode(raw).decode(),'signature':base64.b64encode(sig).decode()},self.pin['publicKey'])
    def test_ordinary_duplicate_guard_and_new_exhibition_are_distinct(self):
        ledger=Ledger(self.root/'ledger');self.addCleanup(ledger.db.close)
        ledger.claim(self.bundle,'operational');ledger.update('operational','ENGINEERING_FAILED',{})
        with self.assertRaises(Rejected):ledger.claim(self.bundle,'auto-repeat')
        ledger.claim(self.bundle,'exhibit-1',self.plan);ledger.update('exhibit-1','VERIFIED_PENDING_REVIEW',{})
        with self.assertRaises(Rejected):ledger.claim(self.bundle,'double-click',self.plan)
        _,_,_,b,p,_,_=fixture(self.root,'2',self.key)
        ledger.claim(b,'exhibit-2',p);ledger.update('exhibit-2','ENGINEERING_FAILED',{})
        with self.assertRaises(Rejected):ledger.claim(b,'auto-after-exhibit')
        self.assertEqual(ledger.db.execute('SELECT COUNT(*) FROM jobs').fetchone()[0],3)
        self.assertEqual(ledger.db.execute('SELECT COUNT(*) FROM exhibition_claims').fetchone()[0],2)
    def test_new_authority_cannot_reuse_old_incident_or_evidence(self):
        ledger=Ledger(self.root/'ledger');self.addCleanup(ledger.db.close)
        ledger.claim(self.bundle,'first',self.plan);ledger.update('first','ENGINEERING_FAILED',{})
        _,_,_,b,p,_,_=fixture(self.root,'2',self.key);b['incident']['uid']=self.bundle['incident']['uid']
        with self.assertRaises(Rejected):ledger.claim(b,'relabelled',p)
        b['incident']['uid']='fresh';b['runtime']['attempts'][0]['patched_at']='2020-01-01T00:00:00Z'
        with self.assertRaises(Rejected):ledger.claim(b,'old-evidence',p)
    def test_interrupted_job_blocks_new_signed_run(self):
        ledger=Ledger(self.root/'ledger');ledger.claim(self.bundle,'first',self.plan);ledger.db.close()
        ledger=Ledger(self.root/'ledger');self.addCleanup(ledger.db.close)
        _,_,_,b,p,_,_=fixture(self.root,'2',self.key)
        with self.assertRaisesRegex(Rejected,'reconciliation'):ledger.claim(b,'second',p)

if __name__=='__main__':unittest.main()

class IssuanceChainTests(unittest.TestCase):
    setUp = PermitTests.setUp
    def completion(self, binding, *, migration=False, sequence=1):
        directory=self.root/('receipt-'+str(len(list(self.root.glob('receipt-*')))));directory.mkdir(mode=0o700)
        blob=b'SYNTHETIC_ARCHIVED_EVIDENCE';checksum=hashlib.sha256(blob).hexdigest();(directory/checksum).write_bytes(blob)
        manifest={'schema':'opsloop-exhibition-migration-archive/v1' if migration else 'opsloop-exhibition-archive/v1','binding':binding,'files':[{'sha256':checksum}],'objects':[]}
        if migration:manifest.update(migrationID='synthetic-migration',planSHA256='d'*64)
        archive=directory/'manifest.json';archive.write_text(canonical(manifest));authority=directory/'authority-retirement.json';authority.write_text(canonical({'binding':binding,'annotations':{}}))
        value={'schema':'opsloop-exhibition-migration-completion/v1' if migration else 'opsloop-exhibition-completion/v1','binding':binding,'nextRunAllowed':True,'stopConfirmed':True,'baselineRestored':True,'modelStopped':True,'evidenceArchived':True,'objectsCleaned':True,'archiveManifest':str(archive),'archiveSHA256':hashlib.sha256(archive.read_bytes()).hexdigest(),'retiredAuthoritySHA256':hashlib.sha256(authority.read_bytes()).hexdigest()}
        if migration:value.update(migrationID='synthetic-migration',planSHA256='d'*64,baseIncidentSequence=sequence)
        receipt=directory/'completion.json';receipt.write_text(canonical(value));return receipt
    def mint(self,payload,receipt,output):
        payload=copy.deepcopy(payload);payload['previousCompletionSHA256']=hashlib.sha256(receipt.read_bytes()).hexdigest()
        source=self.root/(output+'.payload');source.write_text(canonical(payload));sha=hashlib.sha256(source.read_bytes()).hexdigest()
        approval={'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':sha,'authorizationID':payload['authorizationID'],'expiresAt':payload['grant']['notAfter'],'previousCompletion':str(receipt),'previousCompletionSHA256':payload['previousCompletionSHA256']}
        ap=self.root/(output+'.approval');ap.write_text(canonical(approval))
        return issue(source,sha,ap,hashlib.sha256(ap.read_bytes()).hexdigest(),self.key,self.root/'issuer',self.root/output)
    def bootstrap(self):
        scope=self.payload['grant']['scope'];fields=('clusterUID','namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')
        return self.completion({k:scope[k] for k in fields},migration=True,sequence=self.payload['baseIncidentSequence'])
    def test_previous_completion_must_be_immediate_and_consumed_once(self):
        migration=self.bootstrap();self.mint(self.payload,migration,'first')
        p,_,_,_,_,_,_=fixture(self.root,'2',self.key)
        with self.assertRaisesRegex(ControlError,'PREDECESSOR'):self.mint(p,migration,'second-wrong')
        binding={k:self.payload['grant']['scope'][k] for k in ('runID','epoch','policyUID','deploymentUID')}
        complete=self.completion(binding);self.mint(p,complete,'second')
        p3,_,_,_,_,_,_=fixture(self.root,'3',self.key)
        with self.assertRaisesRegex(ControlError,'PREDECESSOR'):self.mint(p3,complete,'third-wrong')
    def test_bootstrap_archive_corruption_and_other_policy_rejected(self):
        migration=self.bootstrap();v=json.loads(migration.read_text());archive=Path(v['archiveManifest']);m=json.loads(archive.read_text());(archive.parent/m['files'][0]['sha256']).write_text('changed')
        with self.assertRaisesRegex(ControlError,'ARCHIVE_INTEGRITY'):self.mint(self.payload,migration,'no')
        migration=self.bootstrap();p=copy.deepcopy(self.payload);p['grant']['scope']['deploymentUID']='another-deployment'
        with self.assertRaisesRegex(ControlError,'TARGET_MIGRATION'):self.mint(p,migration,'wrong-target')
    def test_issuer_never_reissues_after_signing_or_output_failure(self):
        migration=self.bootstrap();(self.root/'reserved-output').write_text('preserved')
        with self.assertRaises(FileExistsError):self.mint(self.payload,migration,'reserved-output')
        self.assertEqual((self.root/'reserved-output').read_text(),'preserved')
        with self.assertRaisesRegex(ControlError,'PREDECESSOR'):self.mint(self.payload,migration,'retry')
