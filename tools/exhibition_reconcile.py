"""Cleanup-only adapter bridge. Never starts a trial, grant, fault or model call."""
from __future__ import annotations
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import time

from tools.exhibition_control import ControlError, canonical, pinned, instant


def require(ok, code):
    if not ok: raise ControlError(code)


def config(plan):
    from tools.exhibition_lifecycle import private_dir
    pin=plan['lifecycle'];cfg=pinned(pin['path'],pin['sha256'])
    require(cfg.get('schema')=='opsloop-exhibition-lifecycle/v1' and cfg.get('binding')==plan['binding'] and time.time()<instant(cfg['expiresAt']),'CLEANUP_AUTHORITY_INVALID')
    return cfg,private_dir(cfg['evidenceRoot'])


def new_file(path, value):
    from tools.exhibition_lifecycle import write
    write(path,canonical(value).encode(),exclusive=True)


def record_start(plan):
    if 'lifecycle' not in plan:return
    cfg,root=config(plan)
    new_file(root/'execution-start.json',{'schema':'opsloop-exhibition-start/v1','binding':plan['binding'],'planId':plan['planId'],'authorizationId':plan['authorizationId'],'track':plan['track'],'startedAt':time.time()})


def record_outcome(plan, value):
    if 'lifecycle' not in plan:return
    # Evidence may be written after a deadline, but this never authorizes any API action.
    pin=plan['lifecycle'];cfg=pinned(pin['path'],pin['sha256']);root=Path(cfg['evidenceRoot'])
    require(cfg['binding']==plan['binding'],'CLEANUP_BINDING_CHANGED')
    new_file(root/'execution-outcome.json',{'schema':'opsloop-exhibition-outcome/v1','binding':plan['binding'],'result':value})


def descriptor(path):
    return {'path':str(path),'sha256':hashlib.sha256(path.read_bytes()).hexdigest()}


def stopped_policy(api, cfg):
    p=api.request('/apis/opsloop.io/v1alpha1/namespaces/'+cfg['namespace']+'/recoverypolicies/'+cfg['policy'])
    require(p['metadata']['uid']==cfg['binding']['policyUID'] and not p['metadata'].get('deletionTimestamp'),'CLEANUP_POLICY_IDENTITY_CHANGED')
    require(p['spec']['enabled'] is False and not p.get('status',{}).get('activeIncident') and any(c.get('type')=='Quiesced' and c.get('status')=='True' and c.get('reason')=='FenceClosed' and c.get('observedGeneration')==p['metadata']['generation'] for c in p.get('status',{}).get('conditions',[])),'CLEANUP_STOP_UNCONFIRMED')
    return p


def inventory(api, cfg, incident_name, root, base_sequence):
    # This exhibition creates no per-run CM/Job. Other object's names are never
    # guessed from labels. Any optional additional inventory must be host-pinned.
    objects=[]
    path='/apis/opsloop.io/v1alpha1/namespaces/'+cfg['namespace']+'/recoveryincidents/'+incident_name
    inc=api.request(path,absent=True)
    identity_path=root/'incident-identity.json'
    identity=json.loads(identity_path.read_bytes()) if identity_path.exists() else None
    if identity is not None:
        require(identity.get('schema')=='opsloop-exhibition-incident-identity/v1' and identity.get('binding')==cfg['binding'] and identity.get('name')==incident_name and identity.get('namespace')==cfg['namespace'],'CLEANUP_INCIDENT_CUSTODY_CHANGED')
        require(inc is not None and inc['metadata']['uid']==identity['uid'],'CLEANUP_INCIDENT_MISSING_OR_REPLACED')
    else:
        require(inc is None,'CLEANUP_INCIDENT_CUSTODY_UNKNOWN')
        policy=stopped_policy(api,cfg)
        require(type(base_sequence) is int and policy.get('status',{}).get('incidentSequence',0)==base_sequence,'CLEANUP_UNOBSERVED_INCIDENT_CONSUMED')
    if inc is not None:
        require(inc['metadata'].get('annotations',{}).get('opsloop.io/execution-grant-binding')==identity['grantBinding'],'CLEANUP_INCIDENT_GRANT_CHANGED')
        a=inc['metadata'].get('annotations',{})
        require(a.get('opsloop.io/exhibition-run')==cfg['binding']['runID'] and a.get('opsloop.io/exhibition-epoch')==cfg['binding']['epoch'],'CLEANUP_FOREIGN_INCIDENT')
        require(inc['spec']['policyRef']['uid']==cfg['binding']['policyUID'] and inc['spec']['workload']['uid']==cfg['binding']['deploymentUID'],'CLEANUP_INCIDENT_TARGET_CHANGED')
        objects.append({'kind':'RecoveryIncident',**{k:inc['metadata'][k] for k in ('name','uid','resourceVersion')}})
    return objects


def finish_receipt(plan, cfg, root, api, incident_name, evidence, base_sequence):
    cached=root/'cleanup-input.json'
    if cached.exists():
        value=json.loads(cached.read_bytes());require(value['binding']==plan['binding'],'CLEANUP_RECEIPT_CHANGED')
        return value
    objects=inventory(api,cfg,incident_name,root,base_sequence)
    value={'binding':plan['binding'],'stopConfirmed':True,'modelStopped':True,'objects':objects,'evidence':[descriptor(p) for p in evidence]}
    if cfg.get('restore') is not None:value['restore']=cfg['restore']
    elif cfg.get('restorePatch') is not None:value['restore']={'patch':cfg['restorePatch']}
    new_file(cached,value)
    return value


def prearm_stop(plan,cfg,api,root,before,base_sequence):
    """A fresh cleanup-only pin can disable pre-arm ObserveOnly, never Enforce."""
    path='/apis/opsloop.io/v1alpha1/namespaces/'+cfg['namespace']+'/recoverypolicies/'+cfg['policy']
    p=api.request(path);m=p['metadata'];a=m.get('annotations',{})
    require(m['uid']==plan['binding']['policyUID'] and not m.get('deletionTimestamp') and not p.get('status',{}).get('activeIncident') and p.get('status',{}).get('incidentSequence',0)==base_sequence,'PREARM_IDENTITY_UNCONFIRMED')
    require(not a.get('opsloop.io/execution-grant') and not a.get('opsloop.io/exhibition-permit'),'PREARM_AUTHORITY_PRESENT')
    observe=copy.deepcopy(before['spec']);observe.update(enabled=True,mode='ObserveOnly')
    disabled=copy.deepcopy(observe);disabled['enabled']=False
    if p['spec']==observe and m['generation']==before['generation']+1:
        require(cfg.get('allowPreArmDisable') is True,'PREARM_DISABLE_APPROVAL_REQUIRED')
        from tools.exhibition_runtime import RuntimeAdapter
        api.request(path,RuntimeAdapter.preconditions(p)+[{'op':'replace','path':'/spec/enabled','value':False}])
    else:
        require((p['spec']==before['spec'] and m['generation']==before['generation']) or (p['spec']==disabled and m['generation']==before['generation']+2),'PREARM_POLICY_CHANGED')
    stopped_policy(api,cfg)
    marker=root/'prearm-stop.json'
    if not marker.exists():new_file(marker,{'binding':plan['binding'],'phase':'STOP_CONFIRMED_WITHOUT_ARM','baseIncidentSequence':base_sequence})


def reconcile_runtime(plan, adapter, emit):
    cfg,root=config(plan);r=plan['runtime']
    from tools.exhibition_runtime import pinned as runtime_pin
    guard=json.loads(runtime_pin(r['guardConfig']));runtime_pin(r['driver'],256*1024*1024)
    require(all(guard.get(k)==v for k,v in plan['binding'].items()),'CLEANUP_GUARD_BINDING_CHANGED')
    api=adapter.transport_factory(r)
    try:
        try:
            status=adapter.cli(r,'status')
            if status.get('state',{}).get('phase')!='STOP_CONFIRMED':
                status=adapter.cli(r,'stop')
            require(status.get('runID')==plan['binding']['runID'] and status.get('state',{}).get('binding')==r['guardBinding'] and status['state'].get('phase')=='STOP_CONFIRMED','CLEANUP_GUARD_STOP_UNCONFIRMED')
        except Exception:
            require(not (root/'fault-intent.json').exists(),'CLEANUP_GUARD_STOP_UNCONFIRMED')
            prearm_stop(plan,cfg,api,root,r['policyBefore'],r['policyBefore'].get('incidentSequence',0))
        stopped_policy(api,cfg)
        # No timer-only readiness claim. Source absence or current readiness is
        # checked, and lifecycle then restores/validates the full Ready baseline.
        source=api.request(adapter.paths(r)['pod'],absent=True)
        if source is not None and source['metadata']['uid']==r['sourcePod']['uid']:
            intent=root/'fault-intent.json'
            if intent.exists():
                confirmed=root/'fault-confirmed.json';require(confirmed.is_file(),'CLEANUP_FAULT_DISPATCH_UNCONFIRMED')
                fault=json.loads(confirmed.read_bytes())
                require(fault.get('binding')==plan['binding'] and fault.get('sourcePodUID')==r['sourcePod']['uid'] and fault.get('durationSeconds')==r['faultSeconds'] and fault.get('hostBoot')==Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'CLEANUP_FAULT_RECEIPT_CHANGED')
                require(type(fault.get('confirmedBootSeconds')) in (int,float) and time.clock_gettime(time.CLOCK_BOOTTIME)>=fault['confirmedBootSeconds']+r['faultSeconds'],'CLEANUP_FAULT_TTL_NOT_ENDED')
            require(source['metadata']['uid']==r['sourcePod']['uid'] and any(c.get('type')=='Ready' and c.get('status')=='True' for c in source.get('status',{}).get('conditions',[])),'CLEANUP_FAULT_STILL_PRESENT')
        stop=root/'runtime-stop.json'
        if not stop.exists():new_file(stop,{'binding':plan['binding'],'guardBinding':r['guardBinding'],'phase':'STOP_CONFIRMED','sourceUID':r['sourcePod']['uid']})
        evidence=[p for p in (root/'execution-start.json',root/'execution-outcome.json',root/'incident-identity.json',root/'fault-intent.json',root/'fault-confirmed.json',root/'prearm-stop.json',stop) if p.exists()]
        emit('guard','LIFECYCLE_STOP_CONFIRMED',{'stopConfirmed':True})
        return finish_receipt(plan,cfg,root,api,r['executionGrant']['scope']['incident'],evidence,r['policyBefore'].get('incidentSequence',0))
    finally:api.close()


def confirm_cgroup_empty(value, fields, unit):
    """MainPID=0 is not proof that descendants have stopped."""
    candidates={p for p in (value.get('cgroup'), fields.get('ControlGroup')) if p}
    if not candidates:
        require(fields.get('LoadState')=='not-found','MODEL_CGROUP_UNCONFIRMED')
        return
    for name in candidates:
        path=Path(name) if name.startswith('/sys/fs/cgroup/') else Path('/sys/fs/cgroup')/name.lstrip('/')
        require(path.is_relative_to('/sys/fs/cgroup') and '..' not in path.parts and path.name==unit+'.service' and not any(p.is_symlink() for p in (path,*path.parents)),'MODEL_CGROUP_IDENTITY_CHANGED')
        if path.exists():
            require((path/'cgroup.procs').read_text().strip()=='','MODEL_CGROUP_STILL_POPULATED')
            events=dict(line.split() for line in (path/'cgroup.events').read_text().splitlines())
            require(events.get('populated')=='0','MODEL_CGROUP_STILL_POPULATED')


def model_jobs_stopped(state, binding):
    """Read durable Runner claims; an interrupted job stays blocked, never rerun."""
    state=Path(state);dbfile=state/'ledger.sqlite3'
    if not dbfile.exists():return []
    require(not dbfile.is_symlink(),'RUNNER_LEDGER_UNSAFE')
    db=sqlite3.connect('file:'+str(dbfile)+'?mode=ro',uri=True)
    try:rows=db.execute('SELECT run_id,state,detail FROM jobs').fetchall()
    finally:db.close()
    evidence=[]
    for job,status,raw in rows:
        detail=json.loads(raw);incident=detail.get('incident',{})
        if incident.get('run_id')!=binding['runID'] or incident.get('epoch')!=binding['epoch']:
            require(status not in ('VALIDATING_INPUT','BASELINE_VERIFYING','ANALYZING','VERIFYING','UNCONFIRMED_TERMINATION'),'OTHER_MODEL_JOB_UNCONFIRMED')
            continue
        terminal = status in ('VERIFIED_PENDING_REVIEW','ENGINEERING_FAILED','CANCELLED')
        if not terminal:
            owner=detail.get('owner',{})
            require(type(owner.get('pid')) is int and owner['pid']>0 and isinstance(owner.get('startTicks'),str),'MODEL_OWNER_UNCONFIRMED')
            boot=Path('/proc/sys/kernel/random/boot_id').read_text().strip()
            proc=Path('/proc')/str(owner['pid'])/'stat'
            if boot==owner.get('boot') and proc.exists():
                require(proc.read_text().rsplit(')',1)[1].split()[19]!=owner['startTicks'],'MODEL_OWNER_STILL_ACTIVE')
        work=state/'runs'/job
        require(re.fullmatch('eb-[a-f0-9]{32}',job) and not work.is_symlink(),'MODEL_JOB_IDENTITY_INVALID')
        process_receipts=list(work.rglob('*.process.json'))
        for receipt in process_receipts:
            value=json.loads(receipt.read_bytes());unit=value.get('unit','')
            require(re.fullmatch('opsloop-eb-[a-f0-9]{32}',unit) and value.get('schema')=='opsloop-sandbox-process/v1','MODEL_UNIT_INVALID')
            if not terminal and value.get('hostBoot')==Path('/proc/sys/kernel/random/boot_id').read_text().strip():
                # Only a durable pre-launch random unit from this exact consumed job.
                stopped=subprocess.run(['/usr/bin/systemctl','--user','stop',unit+'.service'],capture_output=True,timeout=8)
                require(stopped.returncode==0,'MODEL_STOP_UNCONFIRMED')
            show=subprocess.run(['/usr/bin/systemctl','--user','show',unit+'.service','--property=ActiveState','--property=MainPID','--property=ControlGroup','--property=LoadState'],capture_output=True,timeout=5)
            fields=dict(line.split('=',1) for line in show.stdout.decode().splitlines() if '=' in line)
            require(show.returncode==0 and fields.get('ActiveState') in ('inactive','failed') and fields.get('MainPID')=='0','MODEL_PROCESS_STILL_ACTIVE')
            confirm_cgroup_empty(value, fields, unit)
        if not terminal:
            work.mkdir(mode=0o700,parents=True,exist_ok=True)
            cleanup_file=work/'cleanup-reconciliation.json'
            previous={'schema':'opsloop-runner-cleanup/v1','incident':incident,'previousState':status,'previousDetailSHA256':hashlib.sha256(raw.encode()).hexdigest(),'units':[json.loads(p.read_bytes())['unit'] for p in process_receipts],'nextState':'CANCELLED','descendantsConfirmedEmpty':True}
            if cleanup_file.exists():require(json.loads(cleanup_file.read_bytes())==previous,'MODEL_CLEANUP_RECEIPT_CHANGED')
            else:new_file(cleanup_file,previous)
            detail['state']='CANCELLED';detail['cleanupReconciled']=True;detail['cleanupProofSHA256']=hashlib.sha256(cleanup_file.read_bytes()).hexdigest()
            writer=sqlite3.connect(dbfile,isolation_level=None);writer.execute('PRAGMA synchronous=FULL')
            try:
                changed=writer.execute('UPDATE jobs SET state=?,detail=? WHERE run_id=? AND state=? AND detail=?',('CANCELLED',canonical(detail),job,status,raw))
                require(changed.rowcount==1,'MODEL_LEDGER_CHANGED_DURING_CLEANUP')
            finally:writer.close()
        process=detail.get('model_process')
        if process:
            reconciled=detail.get('cleanupReconciled') is True and bool(process_receipts)
            if reconciled:
                proof=work/'cleanup-reconciliation.json'
                require(proof.is_file() and hashlib.sha256(proof.read_bytes()).hexdigest()==detail.get('cleanupProofSHA256'),'MODEL_CLEANUP_PROOF_CHANGED')
            require(process.get('descendants_terminated') is True or reconciled,'MODEL_DESCENDANTS_UNCONFIRMED')
        for name in ('result.json','public-events.jsonl','candidate.patch','incident.json','cleanup-reconciliation.json'):
            if (work/name).is_file():evidence.append(work/name)
    return evidence


def wait_engineering_stop(call, args, binding, guard_binding, receipt, root, deadline):
    def status():
        value=call(['guard-workload','status',*args],5)
        require(value.get('runID')==binding['runID'] and value.get('state',{}).get('binding')==guard_binding,'CAPTURE_STOP_IDENTITY_CHANGED')
        return value
    value=status()
    intent=root/'cleanup-finish-intent.json'
    if value['state'].get('phase')!='STOP_CONFIRMED' and 'finishAttempt' not in receipt and not receipt.get('finishDispatched') and not intent.exists():
        require(time.time()<deadline,'CAPTURE_STOP_AUTHORITY_EXPIRED')
        new_file(intent,{'binding':binding,'guardBinding':guard_binding,'at':time.time()})
        try:call(['finish-workload',*args],20)
        except Exception:pass # durable intent prohibits retransmission; GET decides
    end=min(deadline,time.time()+45)
    while value['state'].get('phase')!='STOP_CONFIRMED' and time.time()<end:
        require(value['state'].get('phase')!='MANUAL_REQUIRED','CAPTURE_STOP_MANUAL_REQUIRED')
        time.sleep(.5);value=status()
    require(value['state'].get('phase')=='STOP_CONFIRMED','CAPTURE_STOP_UNCONFIRMED')
    return value


def reconcile_engineering(plan, emit):
    cfg,root=config(plan)
    require('runtimeCapture' in plan,'CURRENT_RUN_CAPTURE_REQUIRED_FOR_REPEAT')
    capture=Path(plan['runtimeCapture']['root'])
    require(capture==root,'CAPTURE_AND_CLEANUP_ROOT_MISMATCH')
    runtime=pinned(plan['runtimeCapture']['plan'],plan['runtimeCapture']['planSHA256'])
    receipt_file=capture/'runtime-receipt.json'
    receipt=json.loads(receipt_file.read_bytes()) if receipt_file.exists() else {'planSHA256':plan['runtimeCapture']['planSHA256']}
    require(receipt.get('planSHA256')==plan['runtimeCapture']['planSHA256'],'CAPTURE_RECEIPT_CHANGED')
    from tools.exhibition_runtime import process_json, pinned as runtime_pin, Transport
    helper=runtime['helpers']['guard'];runtime_pin(helper,256*1024*1024)
    try:
        guard_path=Path(receipt['guardConfig']);require(guard_path.is_relative_to(capture) and not guard_path.is_symlink(),'CAPTURE_GUARD_PATH_CHANGED')
        guard=json.loads(guard_path.read_bytes());require(all(guard.get(k)==v for k,v in plan['binding'].items()),'CAPTURE_GUARD_BINDING_CHANGED')
        args=['-config',str(guard_path),'-execute','-confirm',receipt['guardBinding']]
        wait_engineering_stop(lambda argv,timeout:process_json([helper['path'],*argv],timeout,1024*1024),args,plan['binding'],receipt['guardBinding'],receipt,root,instant(cfg['expiresAt']))
    except Exception:
        require(not receipt.get('faultIntent'),'CAPTURE_STOP_UNCONFIRMED')
        api=Transport(cfg)
        try:prearm_stop(plan,cfg,api,root,{'generation':runtime.get('initialPolicyGeneration',1),'spec':runtime['initialPolicySpec']},runtime.get('initialIncidentSequence',0))
        finally:api.close()
    model_evidence=model_jobs_stopped(plan['engineering']['state'],plan['binding'])
    # Copy exact selected, non-secret Runner evidence into this run's archive root.
    copied=[]
    for src in model_evidence:
        dest=root/('runner-'+hashlib.sha256(str(src).encode()).hexdigest()[:16]+'-'+src.name)
        raw=src.read_bytes()
        if dest.exists():require(dest.read_bytes()==raw,'RUNNER_EVIDENCE_CHANGED')
        else:
            from tools.exhibition_lifecycle import write
            write(dest,raw,exclusive=True)
        copied.append(dest)
    api=Transport(cfg)
    try:
        stopped_policy(api,cfg)
        evidence=[p for p in (root/'execution-start.json',root/'execution-outcome.json',root/'runtime-receipt.json',root/'incident-identity.json',root/'handoff-start.json',root/'prearm-stop.json',root/'sealed/bundle.json',root/'sealed/manifest.json') if p.exists()]+copied
        emit('guard','LIFECYCLE_STOP_CONFIRMED',{'stopConfirmed':True,'modelStopped':True})
        return finish_receipt(plan,cfg,root,api,runtime['executionScope']['incident'],evidence,runtime.get('initialIncidentSequence',0))
    finally:api.close()
