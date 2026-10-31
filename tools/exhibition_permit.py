"""Offline Ed25519 run permits. Issuance is host CLI only, never an HTTP action.

The Controller independently verifies the signed bytes against its static key.
Private keys are neither uploaded to Kubernetes nor included in run evidence.
"""
from __future__ import annotations
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import stat
import subprocess
import tempfile
import time

from tools.exhibition_control import ControlError, canonical, identifier, instant, pinned

ANNOTATION = 'opsloop.io/exhibition-permit'
PUBLIC_DER_PREFIX = bytes.fromhex('302a300506032b6570032100')
REQUIRED = {'schema','grant','authorizationID','ordinal','baseIncidentSequence','maxIncidents',
            'maxActions','previousCompletionSHA256','allowedCircuitSignatures'}


def require(ok, code):
    if not ok: raise ControlError(code)


def strict_json(raw):
    def unique(pairs):
        result = {}
        for k,v in pairs:
            require(k not in result, 'PERMIT_DUPLICATE_FIELD'); result[k] = v
        return result
    try: return json.loads(raw, object_pairs_hook=unique)
    except (ValueError, UnicodeError): raise ControlError('PERMIT_JSON_INVALID') from None


def validate_payload(p, *, current=True, model=False):
    require(isinstance(p,dict) and REQUIRED <= set(p) <= REQUIRED | {'resourceConditions','modelBudget','circuitReplay','circuitEvidence','predecessorCompletion','newExecutionAuthorization','approvedReproduction'}, 'PERMIT_SCHEMA_INVALID')
    require(p['schema'] in ('opsloop-exhibition-permit/v1','opsloop-exhibition-permit/v2','opsloop-exhibition-permit/v3') and identifier(p['authorizationID']), 'PERMIT_SCHEMA_INVALID')
    for k,minimum in [('ordinal',1),('baseIncidentSequence',0)]:
        require(type(p[k]) is int and minimum <= p[k] < (2**62 if k=='baseIncidentSequence' else 2**63-1), 'PERMIT_SEQUENCE_INVALID')
    require(type(p['maxIncidents']) is int and type(p['maxActions']) is int and p['maxIncidents']==p['maxActions']==1, 'PERMIT_BUDGET_INVALID')
    require(isinstance(p['previousCompletionSHA256'],str) and re.fullmatch('[0-9a-f]{64}',p['previousCompletionSHA256']), 'PERMIT_PREVIOUS_COMPLETION_REQUIRED')
    require(isinstance(p['allowedCircuitSignatures'],list) and len(p['allowedCircuitSignatures'])<=10 and len(set(p['allowedCircuitSignatures']))==len(p['allowedCircuitSignatures']) and all(isinstance(s,str) and re.fullmatch('[0-9a-f]{64}',s) for s in p['allowedCircuitSignatures']), 'PERMIT_CIRCUIT_ALLOWLIST_INVALID')
    g = p['grant']; require(isinstance(g,dict) and set(g)=={'version','scope','instance','notBefore','notAfter'}, 'PERMIT_GRANT_INVALID')
    require(g['version']=='opsloop-execution-grant/v1' and identifier(g['instance']), 'PERMIT_GRANT_INVALID')
    scope=g['scope']; fields={'clusterUID','namespaceUID','namespace','policy','policyUID','policyGeneration','policyHash','deployment','deploymentUID','incident','runID','epoch'}
    require(isinstance(scope,dict) and set(scope)==fields, 'PERMIT_SCOPE_INVALID')
    require(all(identifier(scope[k]) for k in fields-{'policyGeneration','policyHash'}), 'PERMIT_SCOPE_INVALID')
    require(type(scope['policyGeneration']) is int and scope['policyGeneration']>=1 and re.fullmatch('[0-9a-f]{64}',scope['policyHash']), 'PERMIT_SCOPE_INVALID')
    require(scope['incident']==scope['policy']+'-'+scope['policyUID'][:8]+'-'+str(p['baseIncidentSequence']+1).zfill(5),'PERMIT_INCIDENT_SEQUENCE_NAME')
    from tools.exhibition_replay import validate_replay_shape,validate_v2_roles
    validate_replay_shape(p);validate_v2_roles(p)
    begin,end=instant(g['notBefore']),instant(g['notAfter'])
    require(0 < end-begin <= 600, 'PERMIT_WINDOW_INVALID')
    if 'modelBudget' in p:
        b=p['modelBudget']; require(isinstance(b,dict) and set(b)=={'model','reasoningEffort','authMode','maxModelCalls','totalSeconds','expiresAt'},'PERMIT_MODEL_BUDGET_INVALID')
        require((b['model'],b['reasoningEffort'],b['authMode'],b['maxModelCalls'])==('gpt-6-luna','medium','chatgpt',1) and type(b['maxModelCalls']) is int,'PERMIT_MODEL_BUDGET_INVALID')
        require(type(b['totalSeconds']) is int and 1<=b['totalSeconds']<=900 and begin<instant(b['expiresAt'])<=begin+b['totalSeconds'],'PERMIT_MODEL_WINDOW_INVALID')
    if model:
        require('modelBudget' in p,'PERMIT_MODEL_NOT_AUTHORIZED')
        end=instant(p['modelBudget']['expiresAt'])
    if current: require(begin<=time.time()<end,'PERMIT_EXPIRED_OR_FUTURE')
    return p


def verify(envelope, public_key, *, current=True, model=False):
    require(isinstance(envelope,dict) and set(envelope)=={'payload','signature'}, 'PERMIT_ENVELOPE_INVALID')
    try:
        raw=base64.b64decode(envelope['payload'],validate=True); signature=base64.b64decode(envelope['signature'],validate=True)
        key=base64.b64decode(public_key,validate=True)
    except (ValueError, TypeError): raise ControlError('PERMIT_ENCODING_INVALID') from None
    require(0<len(raw)<=32768 and len(signature)==64 and len(key)==32,'PERMIT_SIZE_INVALID')
    with tempfile.TemporaryDirectory(prefix='opsloop-verify-permit-') as directory:
        root=Path(directory)
        for name,data in [('public.der',PUBLIC_DER_PREFIX+key),('payload',raw),('signature',signature)]:
            (root/name).write_bytes(data)
        result=subprocess.run(['/usr/bin/openssl','pkeyutl','-verify','-pubin','-keyform','DER','-inkey',str(root/'public.der'),'-rawin','-in',str(root/'payload'),'-sigfile',str(root/'signature')],capture_output=True,timeout=5)
    require(result.returncode==0,'PERMIT_SIGNATURE_INVALID')
    return validate_payload(strict_json(raw),current=current,model=model)


def bound_permit(plan, *, model=False, current=True):
    cfg=plan.get('exhibitionPermit')
    require(isinstance(cfg,dict) and set(cfg)=={'path','sha256','publicKey'},'PERMIT_PIN_REQUIRED')
    envelope=pinned(cfg['path'],cfg['sha256'])
    payload=verify(envelope,cfg['publicKey'],model=model,current=current)
    require(payload['authorizationID']==plan['authorizationId'],'PERMIT_AUTHORIZATION_CHANGED')
    require(all(payload['grant']['scope'][k]==v for k,v in plan['binding'].items()),'PERMIT_RUN_BINDING_CHANGED')
    return envelope,payload


def issue(payload_path, payload_sha, approval_path, approval_sha, key_path, ledger_path, output):
    """Explicit, exact hash approval consumed before signing; failure never retries."""
    payload=validate_payload(pinned(str(payload_path),payload_sha))
    approval=pinned(str(approval_path),approval_sha)
    require(set(approval)-{'targetMigration','circuitReplayApproval','controllerReceipt'}=={'schema','approved','payloadSHA256','authorizationID','expiresAt','previousCompletion','previousCompletionSHA256'} and approval['schema']=='opsloop-permit-issuance-approval/v1' and approval['approved'] is True,'ISSUANCE_APPROVAL_REQUIRED')
    require(approval['payloadSHA256']==payload_sha and approval['authorizationID']==payload['authorizationID'] and time.time()<instant(approval['expiresAt']),'ISSUANCE_APPROVAL_MISMATCH')
    receipt=pinned(approval['previousCompletion'],approval['previousCompletionSHA256'])
    require(approval['previousCompletionSHA256']==payload['previousCompletionSHA256'] and receipt.get('nextRunAllowed') is True,'ISSUANCE_PREVIOUS_RUN_UNCONFIRMED')
    # Bootstrap requires an independently approved migration completion receipt,
    # never a fabricated zero sequence or erased prior run ledger.
    require(receipt.get('schema') in ('opsloop-exhibition-completion/v1','opsloop-exhibition-migration-completion/v1'),'ISSUANCE_COMPLETION_SCHEMA')
    from tools.exhibition_lifecycle import verify_completion
    verify_completion(approval['previousCompletion'],approval['previousCompletionSHA256'])
    if payload.get('circuitReplay') is not None:
        from tools.exhibition_replay import approved_replay
        ref=approval.get('circuitReplayApproval')
        require(isinstance(ref,dict),'EXPLICIT_REPLAY_APPROVAL_REQUIRED')
        controller_ref=approval.get('controllerReceipt');controller=pinned(controller_ref['path'],controller_ref['sha256']) if controller_ref else None
        require(approved_replay(ref,payload,payload['circuitReplay']['workloadImage'],controller)==payload['circuitReplay'],'ISSUANCE_REPLAY_APPROVAL_CHANGED')
        if payload['schema'] in ('opsloop-exhibition-permit/v2','opsloop-exhibition-permit/v3'):
            from tools.exhibition_replay import v2_roles,verified_predecessor
            require(all(payload[k]==v for k,v in v2_roles(ref,payload,payload['circuitReplay']['workloadImage']).items()),'ISSUANCE_V2_ROLES_CHANGED')
            require(verified_predecessor({'path':approval['previousCompletion'],'sha256':approval['previousCompletionSHA256']},payload['grant']['scope'])==payload['predecessorCompletion'],'ISSUANCE_PREDECESSOR_CHANGED')
    else:
        require('circuitReplayApproval' not in approval and 'controllerReceipt' not in approval,'ISSUANCE_UNUSED_REPLAY_APPROVAL')
    key_path=Path(key_path); st=key_path.stat(follow_symlinks=False)
    require(key_path.is_absolute() and not key_path.is_symlink() and stat.S_ISREG(st.st_mode) and st.st_uid==os.getuid() and not st.st_mode&0o077,'ISSUANCE_PRIVATE_KEY_REQUIRED')
    root=Path(ledger_path); root.mkdir(mode=0o700,parents=True,exist_ok=True)
    require(not root.is_symlink() and not root.stat().st_mode&0o077,'ISSUANCE_PRIVATE_LEDGER_REQUIRED')
    db=sqlite3.connect(root/'issuance.sqlite3',isolation_level=None); os.chmod(root/'issuance.sqlite3',0o600)
    db.execute('PRAGMA synchronous=FULL')
    db.execute('CREATE TABLE IF NOT EXISTS permits (authorization TEXT PRIMARY KEY, run TEXT UNIQUE, epoch TEXT UNIQUE, instance TEXT, ordinal INTEGER, inputHash TEXT, state TEXT, UNIQUE(instance,ordinal))')
    db.execute('CREATE TABLE IF NOT EXISTS completion_chain (completion TEXT PRIMARY KEY, authorization TEXT UNIQUE, predecessorRun TEXT, predecessorEpoch TEXT, receiptPath TEXT)')
    db.execute('CREATE TABLE IF NOT EXISTS target_migrations (policyUID TEXT PRIMARY KEY, deploymentUID TEXT, migrationSHA256 TEXT, receiptPath TEXT)')
    db.execute('CREATE TABLE IF NOT EXISTS circuit_replays (approvalSHA256 TEXT PRIMARY KEY, authorization TEXT UNIQUE, evidenceSHA256 TEXT, state TEXT)')
    try:
        db.execute('BEGIN IMMEDIATE')
        for checksum,path in db.execute('SELECT completion,receiptPath FROM completion_chain').fetchall():
            verify_completion(path,checksum)
        g=payload['grant']; s=g['scope']
        target=db.execute('SELECT deploymentUID,migrationSHA256,receiptPath FROM target_migrations WHERE policyUID=?',(s['policyUID'],)).fetchone()
        if target:
            require(target[0]==s['deploymentUID'],'ISSUANCE_TARGET_CHANGED')
            verify_completion(target[2],target[1])
        else:
            ref=approval.get('targetMigration')
            if ref is None and receipt['schema']=='opsloop-exhibition-migration-completion/v1':
                ref={'path':approval['previousCompletion'],'sha256':approval['previousCompletionSHA256']}
            require(isinstance(ref,dict) and set(ref)=={'path','sha256'},'ISSUANCE_TARGET_MIGRATION_REQUIRED')
            migration=verify_completion(ref['path'],ref['sha256'])
            fields=('clusterUID','namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')
            require(migration.get('schema')=='opsloop-exhibition-migration-completion/v1' and migration.get('binding')=={k:s[k] for k in fields} and migration.get('baseIncidentSequence')==payload['baseIncidentSequence'],'ISSUANCE_TARGET_MIGRATION_MISMATCH')
            db.execute('INSERT INTO target_migrations VALUES (?,?,?,?)',(s['policyUID'],s['deploymentUID'],ref['sha256'],ref['path']))
        previous=db.execute('SELECT run,epoch,state FROM permits ORDER BY rowid DESC LIMIT 1').fetchone()
        if previous:
            require(previous[2]=='ISSUED' and receipt.get('schema')=='opsloop-exhibition-completion/v1' and receipt.get('binding',{}).get('runID')==previous[0] and receipt.get('binding',{}).get('epoch')==previous[1],'ISSUANCE_PREDECESSOR_NOT_COMPLETE')
        else:
            require(receipt.get('schema')=='opsloop-exhibition-migration-completion/v1','ISSUANCE_BOOTSTRAP_REQUIRED')
        db.execute('INSERT INTO completion_chain VALUES (?,?,?,?,?)',(payload['previousCompletionSHA256'],payload['authorizationID'],previous[0] if previous else None,previous[1] if previous else None,approval['previousCompletion']))
        if payload.get('circuitReplay') is not None:
            replay=payload['circuitReplay']
            db.execute('INSERT INTO circuit_replays VALUES (?,?,?,?)',(replay['approvalSHA256'],payload['authorizationID'],replay['bundleSHA256'],'RESERVED_NO_RETRY'))
        g=payload['grant']; s=g['scope']
        maximum=db.execute('SELECT COALESCE(MAX(ordinal),0) FROM permits WHERE instance=?',(g['instance'],)).fetchone()[0]
        require(payload['ordinal']>maximum,'ISSUANCE_ORDINAL_REPLAY')
        db.execute('INSERT INTO permits VALUES (?,?,?,?,?,?,?)',(payload['authorizationID'],s['runID'],s['epoch'],g['instance'],payload['ordinal'],payload_sha,'RESERVED_NO_RETRY'))
        db.execute('COMMIT')
        raw=canonical(payload).encode()
        with tempfile.TemporaryDirectory(prefix='opsloop-sign-permit-') as directory:
            source=Path(directory)/'payload'; source.write_bytes(raw)
            signed=subprocess.run(['/usr/bin/openssl','pkeyutl','-sign','-inkey',str(key_path),'-rawin','-in',str(source)],capture_output=True,timeout=5)
        require(signed.returncode==0 and len(signed.stdout)==64,'ISSUANCE_SIGNING_UNCONFIRMED')
        envelope={'payload':base64.b64encode(raw).decode(),'signature':base64.b64encode(signed.stdout).decode()}
        output=Path(output)
        require(output.parent.is_dir() and not output.parent.is_symlink() and not output.parent.stat().st_mode&0o077,'ISSUANCE_PRIVATE_OUTPUT_REQUIRED')
        with os.fdopen(os.open(output,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600),'wb') as f:
            f.write(canonical(envelope).encode());f.flush();os.fsync(f.fileno())
        db.execute('UPDATE permits SET state=? WHERE authorization=?',('ISSUED',payload['authorizationID']))
        return hashlib.sha256(output.read_bytes()).hexdigest()
    except sqlite3.IntegrityError: raise ControlError('ISSUANCE_ALREADY_CONSUMED') from None
    finally: db.close()


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for flag in ('payload','payload-sha256','approval','approval-sha256','private-key','ledger','output'):p.add_argument('--'+flag,required=True)
    p.add_argument('--approve-issuance',action='store_true',required=True)
    a=p.parse_args()
    print(json.dumps({'permitSHA256':issue(Path(a.payload),a.payload_sha256,Path(a.approval),a.approval_sha256,Path(a.private_key),Path(a.ledger),Path(a.output))}))
if __name__=='__main__':main()
