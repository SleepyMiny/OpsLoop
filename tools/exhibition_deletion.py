"""Read-only completion of an already journaled UID/RV DELETE.

This module never sends DELETE/PATCH/POST. Dispatch markers and earlier failed
observations remain immutable; completion is an additional hash-linked receipt.
"""
import json,math,time,uuid
from pathlib import Path
from tools.exhibition_control import canonical,instant,ControlError
from tools.exhibition_hashes import private_read,require,verify


def pending_record(marker,identity,record_key,observed):
    """After dispatch, RV/finalizers may change; custody comes from the archive."""
    from tools.exhibition_lifecycle import sha
    marker=Path(marker);v=private_read(marker,sha(marker.read_bytes()))
    require(v.get('identity')==identity and v.get('state') in ('DELETE_DISPATCHED','DELETED'),'DELETE_RECEIPT_MISMATCH')
    r=v.get(record_key);require(isinstance(r,dict) and r.get('identity')==identity,'DELETE_ARCHIVE_IDENTITY_CHANGED')
    if observed is not None:
        require(observed.get('metadata',{}).get('uid')==identity['uid'],'OBJECT_REPLACED')
    return r


def confirmed_deletion(marker,identity,path):
    """Validate an existing immutable proof; never creates a new wait window."""
    from tools.exhibition_lifecycle import sha
    marker=Path(marker);file=marker.with_suffix('.confirmed.json')
    proof=private_read(file,sha(file.read_bytes()))
    require(proof.get('schema')=='opsloop-delete-confirmation/v1' and proof.get('state')=='DELETED'
        and proof.get('identity')==identity and proof.get('path')==path
        and proof.get('dispatchSHA256')==sha(marker.read_bytes()),'DELETE_CONFIRMATION_CHANGED')
    obs=private_read(proof['observation']['path'],proof['observation']['sha256'])
    require(obs.get('state')=='ABSENCE_CONFIRMED' and obs.get('identity')==identity and obs.get('path')==path
        and obs.get('dispatchSHA256')==proof['dispatchSHA256'] and obs.get('observations',[])[-1]['result']=='ABSENT'
        and obs['completedAt']<obs['deadline']<=obs['authorizationExpiresAt'],'DELETE_CONFIRMATION_CHANGED')
    return {'path':str(file),'sha256':sha(file.read_bytes())}


def wait_deleted(api,path,identity,marker,expires_at,*,window_seconds=30,poll_seconds=.5,clock=None,monotonic=None,sleep=None):
    from tools.exhibition_lifecycle import KINDS,sha,write,private_dir
    clock=clock or time.time;monotonic=monotonic or time.monotonic;sleep=sleep or time.sleep
    require(type(window_seconds) in (int,float) and math.isfinite(window_seconds) and 0<window_seconds<=60,'DELETE_WAIT_BOUND_INVALID')
    require(type(poll_seconds) in (int,float) and math.isfinite(poll_seconds) and .1<=poll_seconds<=2,'DELETE_POLL_BOUND_INVALID')
    namespace=path.split('/namespaces/')[-1].split('/')[0]
    require(path==KINDS[identity['kind']].format(ns=namespace)+'/'+identity['name'] and '?' not in path,'DELETE_WAIT_PATH_CHANGED')
    marker=Path(marker);dispatch_sha=sha(marker.read_bytes());dispatch=private_read(marker,dispatch_sha)
    require(dispatch.get('identity')==identity and dispatch.get('state') in ('DELETE_DISPATCHED','DELETED'),'DELETE_RECEIPT_MISMATCH')
    authorization_end=instant(expires_at);start=clock();until=min(authorization_end,start+window_seconds)
    require(start<until,'DELETE_WAIT_DEADLINE')
    # Persist the first deadline before GET: restart never buys another window.
    window=marker.with_suffix('.wait-window.json')
    if window.exists():
        prior=private_read(window,sha(window.read_bytes()))
        require(prior.get('dispatchSHA256')==dispatch_sha and prior.get('authorizationExpiresAt')==authorization_end,'DELETE_WAIT_AUTHORITY_CHANGED')
        until=min(until,prior['deadline'])
    else:
        write(window,canonical({'dispatchSHA256':dispatch_sha,'authorizationExpiresAt':authorization_end,'startedAt':start,'deadline':until}).encode(),exclusive=True)
    require(start<until,'DELETE_WAIT_DEADLINE')
    mono_end=monotonic()+until-start
    attempts=private_dir(str(marker.with_suffix('.wait')));attempt=attempts/(uuid.uuid4().hex+'.json')
    receipt={'schema':'opsloop-delete-observation/v1','method':'GET','path':path,'identity':identity,'dispatchSHA256':dispatch_sha,
        'startedAt':start,'deadline':until,'authorizationExpiresAt':authorization_end,'state':'WAITING','observations':[]}
    confirmed=marker.with_suffix('.confirmed.json')
    def save():write(attempt,canonical(receipt).encode())
    save()
    try:
        while True:
            require(clock()<until and monotonic()<mono_end,'DELETE_WAIT_DEADLINE')
            observed=api.request(path,absent=True)  # Only a transport-confirmed 404 is None.
            at=clock();entry={'at':at}
            if observed is None:entry['result']='ABSENT'
            else:
                require(isinstance(observed,dict),'DELETE_GET_RESPONSE_INVALID')
                m=observed.get('metadata',{});entry.update(result='PRESENT',uid=m.get('uid'),resourceVersion=m.get('resourceVersion'),deletionTimestamp=m.get('deletionTimestamp'))
            receipt['observations'].append(entry);save()
            require(at<until and monotonic()<mono_end,'DELETE_WAIT_DEADLINE')
            if observed is None:
                receipt.update(state='ABSENCE_CONFIRMED',completedAt=at);save()
                proof={'schema':'opsloop-delete-confirmation/v1','state':'DELETED','identity':identity,'path':path,'dispatchSHA256':dispatch_sha,
                    'observation':{'path':str(attempt),'sha256':sha(attempt.read_bytes())}}
                if confirmed.exists():
                    prior=private_read(confirmed,sha(confirmed.read_bytes()))
                    require(all(prior.get(k)==proof[k] for k in ('schema','state','identity','path','dispatchSHA256')),'DELETE_CONFIRMATION_CHANGED')
                    old=private_read(prior['observation']['path'],prior['observation']['sha256'])
                    require(old.get('state')=='ABSENCE_CONFIRMED' and old.get('identity')==identity and old.get('path')==path,'DELETE_CONFIRMATION_CHANGED')
                else:write(confirmed,canonical(proof).encode(),exclusive=True)
                return {'path':str(confirmed),'sha256':sha(confirmed.read_bytes())}
            m=observed.get('metadata',{})
            require(m.get('uid')==identity['uid'],'OBJECT_REPLACED')
            require(m.get('name')==identity['name'] and m.get('namespace')==namespace,'DELETE_GET_IDENTITY_CHANGED')
            require(not confirmed.exists(),'DELETE_CONFIRMED_OBJECT_PRESENT')
            sleep(min(poll_seconds,max(0,until-clock()),max(0,mono_end-monotonic())))
    except BaseException as exc:
        receipt.update(state='UNCONFIRMED',errorType=type(exc).__name__,reason=str(exc) if isinstance(exc,ControlError) else 'GET_OR_PROCESS_INTERRUPTED',completedAt=clock());save()
        raise


def continuation_inputs(cfg):
    """Validate immutable historical custody/dispatch/absence evidence, no I/O API."""
    from tools.exhibition_lifecycle import sha,KINDS,safe_bytes
    require(cfg.get('schema')=='opsloop-delete-continuation/v1' and cfg.get('allowedMethods')==['GET'],'DELETE_CONTINUATION_SCOPE')
    def read(name):
        ref=cfg[name];return private_read(ref['path'],ref['sha256'])
    files=cfg.get('executionFiles',[])
    required={str(Path(__file__).resolve().parent/name) for name in ('exhibition_deletion.py','exhibition_lifecycle.py','exhibition_runtime.py','exhibition_hashes.py','exhibition_control.py')}
    require({f['path'] for f in files}==required,'DELETE_CONTINUATION_CODE_REQUIRED')
    for f in files:require(sha(safe_bytes(f['path'],Path(__file__).resolve().parent))==f['sha256'],'DELETE_CONTINUATION_CODE_CHANGED')
    original=read('originalMigrationPlan');manifest=read('archiveManifest');dispatch=read('dispatchMarker');identity=cfg['identity']
    target=original['target'];path=KINDS[identity['kind']].format(ns=target['namespace'])+'/'+identity['name']
    require(cfg['path']==path and manifest['binding']==target and manifest['planSHA256']==cfg['originalMigrationPlan']['sha256'],'DELETE_CONTINUATION_BINDING')
    require(dispatch['identity']==identity and dispatch['state']=='DELETE_DISPATCHED','DELETE_CONTINUATION_DISPATCH')
    record=dispatch['record'];require(record in manifest['objects'] and record['identity']==identity,'DELETE_CONTINUATION_ARCHIVE')
    desc=[v for v in original['objects'] if v['uid']==identity['uid'] and v['name']==identity['name']]
    require(len(desc)==1 and desc[0]['hash']==record['hash'] and desc[0]['sha256']==record['sha256'],'DELETE_CONTINUATION_OBJECT')
    directory=Path(cfg['archiveManifest']['path']).parent
    for item in manifest['files']+manifest['objects']:
        raw=safe_bytes(directory/item['sha256'],directory);require(sha(raw)==item['sha256'],'ARCHIVE_INTEGRITY_FAILED')
    obj=verify(json.loads(safe_bytes(directory/record['sha256'],directory)),record['hash'],fields='/')
    require(all(obj['metadata'].get(k)==identity[k] for k in ('name','uid','resourceVersion')) and obj['metadata']['namespace']==target['namespace'],'DELETE_CONTINUATION_OBJECT_IDENTITY')
    audit=read('followupGET');stdout=Path(cfg['followupGET']['path']).parent/'stdout.bin';stderr=stdout.with_name('stderr.bin')
    out=safe_bytes(stdout,stdout.parent);err=safe_bytes(stderr,stderr.parent)
    require(sha(out)==audit['stdoutSHA256'] and sha(err)==audit['stderrSHA256'],'DELETE_FOLLOWUP_BYTES_CHANGED')
    require(audit['target']==path and audit['argv'][-3:]==['get','--raw',path] and audit['stdinBytes']==0 and audit['exitCode']==1
        and not out and b'Error from server (NotFound):' in err,'DELETE_FOLLOWUP_NOT_ABSENCE')
    return original,{'state':'HISTORICAL_CUSTODY_AND_ABSENCE_VERIFIED','identity':identity,'path':path,'currentAbsenceConfirmed':False,'nextRunAllowed':False,'writes':0}


def continuation_config(plan):
    """An explicitly pinned successor uses old evidence, never old authority."""
    import copy
    c=plan['continuation'];cfg=private_read(c['priorDeletionPlan']['path'],c['priorDeletionPlan']['sha256'])
    cfg=copy.deepcopy(cfg);cfg['executionFiles']=c['executionFiles']
    continuation_inputs(cfg)
    return cfg


def archived_incident(plan,key,observed):
    """Validate an absent A against historical custody; do not label it current."""
    if key!='a' or 'continuation' not in plan:return observed
    cfg=continuation_config(plan)
    require(observed=={'absent':True},'PRIOR_A_ABSENCE_REQUIRED')
    pin=plan['objectHashContract']['objects']['a-incident']
    manifest=private_read(cfg['archiveManifest']['path'],cfg['archiveManifest']['sha256'])
    record=next(r for r in manifest['objects'] if r['identity']==cfg['identity'])
    require(record['hash']==pin,'CONTINUATION_CUSTODY_CHANGED')
    return private_read(Path(cfg['archiveManifest']['path']).parent/record['sha256'],record['sha256'])


def confirm_prior_deletion(plan,reader,root,session):
    """First integrated stage. Read-only, before registry/cluster mutations."""
    from tools.exhibition_inputs import new
    cfg=continuation_config(plan);original,_=continuation_inputs(cfg);t=original['target']
    require(reader.get('/api/v1/namespaces/kube-system')['metadata']['uid']==t['clusterUID'],'CONTINUATION_CLUSTER_CHANGED')
    require(reader.get('/api/v1/namespaces/'+t['namespace'])['metadata']['uid']==t['namespaceUID'],'CONTINUATION_NAMESPACE_CHANGED')
    class ExactReader:
        def request(self,path,absent=False):
            require(path==cfg['path'] and absent,'CONTINUATION_GET_SCOPE')
            v=reader.get(path,absent=True)
            return None if v=={'absent':True} else v
    proof=wait_deleted(ExactReader(),cfg['path'],cfg['identity'],cfg['dispatchMarker']['path'],session['preparationDeadline'])
    return new(root,'a-prior-deletion-confirmed.json',{'schema':'opsloop-prior-deletion-completed/v1',
        'priorDeletionPlan':plan['continuation']['priorDeletionPlan'],'confirmation':proof,
        'archiveManifest':cfg['archiveManifest'],'dispatchMarker':cfg['dispatchMarker'],
        'state':'DELETION_CONFIRMED_ONLY','migrationComplete':False,'nextRunAllowed':False})


def main():
    import argparse
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--plan',required=True);p.add_argument('--sha256',required=True)
    p.add_argument('--confirm-read-only',action='store_true');p.add_argument('--expires-at')
    a=p.parse_args();cfg=private_read(a.plan,a.sha256);original,result=continuation_inputs(cfg)
    if a.confirm_read_only:
        require(a.expires_at and time.time()<instant(a.expires_at)<=time.time()+900,'NEW_BOUNDED_READ_APPROVAL_REQUIRED')
        from tools.exhibition_lifecycle import CleanupTransport
        target=original['target'];cluster_path='/api/v1/namespaces/kube-system';namespace_path='/api/v1/namespaces/'+target['namespace']
        allowed={cfg['path'],cluster_path,namespace_path}
        class ReadOnlyTransport(CleanupTransport):
            def request(self,path,patch=None,absent=False):
                require(path in allowed and patch is None and time.time()<instant(a.expires_at),'READ_RECONCILIATION_OUT_OF_SCOPE');return super().request(path,absent=absent)
            def delete(self,*args):raise ControlError('READ_RECONCILIATION_DELETE_FORBIDDEN')
        api=ReadOnlyTransport(original)
        try:
            require(api.request(cluster_path)['metadata']['uid']==target['clusterUID'] and api.request(namespace_path)['metadata']['uid']==target['namespaceUID'],'DELETE_CONTINUATION_CLUSTER_CHANGED')
            result=wait_deleted(api,cfg['path'],cfg['identity'],cfg['dispatchMarker']['path'],a.expires_at)
        finally:api.close()
    print(json.dumps(result))
if __name__=='__main__':main()
