"""Fixed-Policy preparation primitives. No import-time I/O or implicit execution.

Archive/migration remains in exhibition_migration; experiments remain under the
Coordinator/Guard. These functions replace the obsolete new-Policy setup script.
Every external write needs the caller's exact approved plan and bounded session.
"""
from __future__ import annotations
import copy
import datetime as dt
import hashlib
import importlib.util
import json
from pathlib import Path
from tools.exhibition_hashes import encoded, verify, private_read, require
from tools.exhibition_lifecycle import write, private_dir


def authority(plan, approval_sha, session, *, now=None, cleanup=False):
    now=now or dt.datetime.now(dt.timezone.utc)
    require(plan.get('status')=='SEALED_EXECUTION_CANDIDATE','UNSEALED_EXECUTION_PLAN')
    def at(x): return dt.datetime.fromisoformat(x.replace('Z','+00:00'))
    require(session.get('approvedPlanSHA256')==approval_sha and session.get('planContentSHA256')==hashlib.sha256(encoded(plan)).hexdigest(), 'SESSION_APPROVAL_CHANGED')
    require((session.get('livePreflightState')=='LIVE_CHECKS_PASS' or session.get('livePreflightState')=='PREPARATION_INPUTS_PASS' and session.get('preflightPhase')=='PREPARATION') and session.get('livePreflightScope')=='CURRENT_GET_ONLY', 'LIVE_PREFLIGHT_REQUIRED')
    if session.get('delegatedWork') is not None:
        from tools.exhibition_b_work import work_auth
        require(session['delegatedWork']==plan['newInterval']['authorization'],'B_WORK_APPROVAL_CHANGED')
        a=work_auth(session['delegatedWork'],deadline=at(session['deadline']).timestamp(),now=now.timestamp())
        require(session['approvedAt']==a['receivedAt'] and at(a['receivedAt'])<=at(session['startedAt'])<=now,'B_WORK_START_CHANGED')
        require(at(session['preparationDeadline'])<=at(session['startedAt'])+dt.timedelta(minutes=20) and now<at(session['deadline'] if cleanup else session['preparationDeadline']),'B_PREPARATION_OR_CLEANUP_EXPIRED')
    else:
        require(at(session['approvedAt']) <= at(session['startedAt']) <= at(session['approvedAt'])+dt.timedelta(minutes=30), 'APPROVAL_START_WINDOW')
        require(at(session['preparationDeadline']) <= at(session['startedAt'])+dt.timedelta(minutes=20),'PREPARATION_WINDOW_EXPANDED')
        require(at(session['startedAt']) <= now < at(session['deadline'] if cleanup else session['preparationDeadline']) <= at(session['deadline'])
                <= min(at(session['startedAt'])+dt.timedelta(minutes=90),at(session['completionDeadline'])), 'SESSION_WINDOW_EXPIRED')
    require(plan['authority']['order']==['A1','A2','B1','B2'] and plan['authority']['maxNewPermits']==4, 'SESSION_SCOPE_CHANGED')


def reserve(root, stage, approval_sha):
    directory=private_dir(str(root));path=directory/(stage+'.json')
    write(path,encoded({'stage':stage,'state':'DISPATCH_RESERVED','approvedPlanSHA256':approval_sha}),exclusive=True)
    return path


def controller_transition(plan, before, public_key):
    """Pure exact-field transform; preserves SA/RBAC, security and unrelated args."""
    import base64
    c=plan['deployment']['controller'];verify(before,c['expectedSpecHash'],fields='/spec')
    require(len(base64.b64decode(public_key,validate=True))==32,'PUBLIC_KEY_INVALID')
    after=copy.deepcopy(before);spec=after['spec'];ps=spec['template']['spec']
    require(spec.get('replicas')==1 and spec['strategy']['type']=='Recreate'
            and ps['serviceAccountName']==c['serviceAccountUnchanged'],'CONTROLLER_BASE_CHANGED')
    managers=[x for x in ps['containers'] if x['name']=='manager'];require(len(managers)==1,'CONTROLLER_CONTAINER_CHANGED')
    manager=managers[0];args=manager['args'];old=[i for i,a in enumerate(args) if a.startswith('--execution-scope-file=')]
    require(len(old)==1 and not any(a.startswith(('--exhibition-authority-file','--resource-conditions')) for a in args),'CONTROLLER_ARGUMENTS_CHANGED')
    args[old[0]]='--exhibition-authority-file=/etc/opsloop/execution/authority.json'
    publication=plan['deployment']['imagePublication'];manager['image']=publication['repository']+'@'+publication['digest']
    mounts=[x for x in manager.get('volumeMounts',[]) if x.get('mountPath')=='/etc/opsloop/execution']
    require(len(mounts)==1 and mounts[0].get('readOnly') is True,'CONTROLLER_MOUNT_CHANGED')
    volumes=[v for v in ps['volumes'] if v['name']==mounts[0]['name']];require(len(volumes)==1 and 'configMap' in volumes[0],'CONTROLLER_VOLUME_CHANGED')
    name=c['staticConfigMap']['name']
    volumes[0]['configMap'].update(name=name,items=[{'key':'authority.json','path':'authority.json'}])
    config={'schema':'opsloop-exhibition-authority/v1','publicKey':public_key,
            'targets':[{'clusterUID':plan['clusterUID'],'namespaceUID':t['namespaceUID'],'namespace':t['namespace'],
                        'policy':t['policy'],'policyUID':t['policyUID'],'deployment':t['deployment'],'deploymentUID':t['deploymentUID']}
                       for t in plan['fixedTargets'].values()]}
    cm={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':name,'namespace':c['namespace']},'immutable':True,
        'data':{'authority.json':json.dumps(config,sort_keys=True,separators=(',',':'))}}
    patches=[{'op':'test','path':'/metadata/uid','value':before['metadata']['uid']},
             {'op':'test','path':'/metadata/resourceVersion','value':before['metadata']['resourceVersion']},
             {'op':'test','path':'/spec','value':before['spec']},
             {'op':'replace','path':'/spec/template/spec/containers','value':ps['containers']},
             {'op':'replace','path':'/spec/template/spec/volumes','value':ps['volumes']}]
    return {'configMap':cm,'patch':patches,'before':before,'after':after}


def deploy_once(plan, approval_sha, session, root, api, public_key):
    """api supplies authenticated get/create/patch; never create Policy/CRD/RBAC."""
    authority(plan,approval_sha,session);c=plan['deployment']['controller']
    path='/apis/apps/v1/namespaces/'+c['namespace']+'/deployments/'+c['name']
    change=controller_transition(plan,api.get(path),public_key)
    cm=change['configMap'];cm_path='/api/v1/namespaces/'+c['namespace']+'/configmaps'
    require(api.get(cm_path+'/'+cm['metadata']['name'],absent=True) is None,'AUTHORITY_CONFIGMAP_EXISTS')
    marker=reserve(root,'controller-deployment',approval_sha)
    # Caller transport must audit stdout/stderr/exit code/body hash independently.
    write(marker.with_name('controller-restore-input.json'),encoded(change),exclusive=True)
    authority(plan,approval_sha,session)
    made=api.create(cm_path,cm)
    require(made['metadata'].get('uid') and made['metadata']['name']==cm['metadata']['name'] and
            made['metadata']['namespace']==cm['metadata']['namespace'] and made.get('data')==cm['data']
            and made.get('immutable') is True,'AUTHORITY_CREATE_UNCONFIRMED')
    authority(plan,approval_sha,session)
    result=api.patch(path,change['patch'])
    verify(result,__import__('tools.exhibition_hashes',fromlist=['describe']).describe(change['after'],'/spec'),fields='/spec')
    write(marker,encoded({'state':'PATCH_RESPONSE_CONFIRMED_INSTANCE_PENDING','configMapUID':made['metadata']['uid'],
                         'deploymentUID':result['metadata']['uid'],'generation':result['metadata']['generation']}))
    return result


def restore_controller_once(plan, approval_sha, session, root, api, model_checker):
    # This is a preparation-failure restore only; session cleanup window, not grant TTL.
    authority(plan,approval_sha,session,cleanup=True)
    require(not (Path(root)/'controller-restore.json').exists(),'RESTORE_ALREADY_RESERVED')
    for t in plan['fixedTargets'].values():
        pol=api.get('/apis/opsloop.io/v1alpha1/namespaces/'+t['namespace']+'/recoverypolicies/'+t['policy'])
        require(pol['metadata']['uid']==t['policyUID'] and not pol['spec']['enabled'] and not pol.get('status',{}).get('activeIncident')
                and any(c.get('type')=='Quiesced' and c.get('status')=='True' and c.get('reason')=='FenceClosed' and c.get('observedGeneration')==pol['metadata']['generation'] for c in pol.get('status',{}).get('conditions',[])), 'RESTORE_STOP_REQUIRED')
        require(not any(k in pol['metadata'].get('annotations',{}) for k in ('opsloop.io/exhibition-permit','opsloop.io/execution-grant')),'RESTORE_RETIRED_AUTHORITY_REQUIRED')
    require(bool(session.get('modelChecks')),'RESTORE_MODEL_INVENTORY_REQUIRED')
    for check in session['modelChecks']: model_checker(check['state'],check['binding'])
    path=Path(root)/'controller-restore-input.json';change=private_read(path,hashlib.sha256(path.read_bytes()).hexdigest())
    c=plan['deployment']['controller'];endpoint='/apis/apps/v1/namespaces/'+c['namespace']+'/deployments/'+c['name']
    current=api.get(endpoint)
    from tools.exhibition_hashes import describe
    verify(change['before'],c['expectedSpecHash'],fields='/spec')
    import base64
    expected=controller_transition(plan,change['before'],base64.b64encode(bytes(32)).decode())
    require(change['after']['spec']==expected['after']['spec'],'RESTORE_INPUT_CHANGED')
    if current.get('spec') == change['before']['spec']:
        verify(current,c['expectedSpecHash'],fields='/spec')
        marker=reserve(root,'controller-restore',approval_sha)
        write(marker,encoded({'state':'ORIGINAL_SPEC_CONFIRMED_ROLLOUT_PENDING','deploymentUID':c['uid']}))
        return current
    verify(current,describe(change['after'],'/spec'),fields='/spec')
    original=change['before']['spec']['template']['spec']
    patch=[{'op':'test','path':'/metadata/uid','value':c['uid']},
           {'op':'test','path':'/metadata/resourceVersion','value':current['metadata']['resourceVersion']},
           {'op':'test','path':'/spec','value':current['spec']},
           {'op':'replace','path':'/spec/template/spec/containers','value':original['containers']},
           {'op':'replace','path':'/spec/template/spec/volumes','value':original['volumes']}]
    marker=reserve(root,'controller-restore',approval_sha)
    result=api.patch(endpoint,patch);verify(result,c['expectedSpecHash'],fields='/spec')
    write(marker,encoded({'state':'RESTORE_RESPONSE_CONFIRMED_ROLLOUT_PENDING','deploymentUID':c['uid']}))
    return result


def oci_payload(publication):
    layout=Path(publication['localOCI'])
    def get(digest):
        require(digest.startswith('sha256:') and len(digest)==71,'OCI_DIGEST_INVALID')
        path=layout/'blobs/sha256'/digest[7:];require(path.is_file() and not path.is_symlink(),'OCI_BLOB_MISSING')
        raw=path.read_bytes();require('sha256:'+hashlib.sha256(raw).hexdigest()==digest,'OCI_BLOB_CHANGED');return raw
    index=get(publication['digest']);doc=json.loads(index);manifests=[];blobs={}
    for desc in doc['manifests']:
        raw=get(desc['digest']);require(len(raw)==desc['size'],'OCI_SIZE_CHANGED');manifests.append((desc['digest'],raw))
        m=json.loads(raw)
        for b in [m['config'],*m['layers']]:
            raw=get(b['digest']);require(len(raw)==b['size'],'OCI_SIZE_CHANGED');blobs[b['digest']]=raw
    return index,manifests,blobs


def publish_once(plan, approval_sha, session, root, transport):
    """Existing registry helper adapter; exact tag absence and once-only journal."""
    authority(plan,approval_sha,session);p=plan['deployment']['imagePublication']
    require(p['maxPushAttempts']==1,'PUBLICATION_LIMIT_CHANGED')
    index,manifests,blobs=oci_payload(p)
    require(transport.head(p['repository'],p['newTag'])==404,'REGISTRY_TAG_EXISTS_OR_UNKNOWN')
    marker=reserve(root,'image-publication',approval_sha)
    # No retry on exception: reservation remains and blocks re-entry.
    authority(plan,approval_sha,session)
    transport.publish_digest(p,index,manifests,blobs)
    authority(plan,approval_sha,session)
    require(transport.head(p['repository'],p['newTag'])==404,'REGISTRY_TAG_RACED')
    response=transport.create_tag(p,index,if_none_match='*')
    require(response=={'status':201,'digest':p['digest']},'REGISTRY_TAG_CREATE_UNCONFIRMED')
    require(transport.get_tag(p)==index,'REGISTRY_TAG_READBACK_MISMATCH')
    write(marker,encoded({'state':'PUBLISHED_CONFIRMED','repository':p['repository'],'tag':p['newTag'],'digest':p['digest']}))
    return {'digest':p['digest'],'tag':p['newTag']}


class PreparationAPI:
    """Dedicated audited kubectl for the two preparation endpoints only."""
    def __init__(self, plan, approval_sha, session, root, audit_ref, kubectl, kubeconfig, *, cleanup=False):
        authority(plan,approval_sha,session,cleanup=cleanup)
        self.cleanup=cleanup
        self.plan,self.approval,self.session,self.root=plan,approval_sha,session,Path(root)
        require(hashlib.sha256(Path(kubectl['path']).read_bytes()).hexdigest()==kubectl['sha256'],'KUBECTL_CHANGED')
        require(hashlib.sha256(Path(audit_ref['path']).read_bytes()).hexdigest()==audit_ref['sha256'],'AUDIT_HELPER_CHANGED')
        spec=importlib.util.spec_from_file_location('_opsloop_preparation_audit',audit_ref['path']);self.audit=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.audit)
        require(kubeconfig['path']=='/home/sleepyminy/.kube/opsloop.yaml' and hashlib.sha256(Path(kubeconfig['path']).read_bytes()).hexdigest()==kubeconfig['sha256'],'KUBECONFIG_IDENTITY_CHANGED')
        self.argv=[kubectl['path'],'--kubeconfig',kubeconfig['path'],'--context','default','--request-timeout=5s']
        c=plan['deployment']['controller'];self.dep='/apis/apps/v1/namespaces/'+c['namespace']+'/deployments/'+c['name']
        self.cm='/api/v1/namespaces/'+c['namespace']+'/configmaps'
        self.cm_name=c['staticConfigMap']['name']
        self.readable={self.dep,self.cm+'/'+self.cm_name}|{'/apis/opsloop.io/v1alpha1/namespaces/'+t['namespace']+'/recoverypolicies/'+t['policy'] for t in plan['fixedTargets'].values()}
    def command(self, argv, path, body=None, absent=False):
        authority(self.plan,self.approval,self.session,cleanup=self.cleanup)
        result,receipt=self.audit.audited_run(self.root/'commands',self.argv+argv,target=path,
                    input_bytes=encoded(body) if body is not None else None,timeout=8)
        if absent and result.returncode and b'NotFound' in result.stderr:return None
        require(result.returncode==0,'PREPARATION_COMMAND_REJECTED_OR_UNCONFIRMED')
        from tools.exhibition_hashes import parse
        return parse(result.stdout)
    def get(self,path,absent=False):
        require(path in self.readable,'PREPARATION_GET_OUT_OF_SCOPE')
        return self.command(['get','--raw',path],path,absent=absent)
    def create(self,path,body):
        require(not self.cleanup and path==self.cm and body['kind']=='ConfigMap' and body['metadata']['name']==self.cm_name,'PREPARATION_CREATE_OUT_OF_SCOPE')
        return self.command(['create','--raw',path,'-f','-'],path,body)
    def patch(self,path,body):
        require(path==self.dep,'PREPARATION_PATCH_OUT_OF_SCOPE')
        allowed={'/metadata/uid','/metadata/resourceVersion','/spec','/spec/template/spec/containers','/spec/template/spec/volumes'}
        require(all(o['path'] in allowed and (o['op']=='test' or o['op']=='replace' and o['path'].startswith('/spec/template/spec/')) for o in body),'PREPARATION_PATCH_FIELDS_CHANGED')
        c=self.plan['deployment']['controller']
        if self.cleanup:
            from tools.exhibition_lifecycle import patched
            verify(patched(self.get(path),body),c['expectedSpecHash'],fields='/spec')
        return self.command(['patch','deployment',c['name'],'-n',c['namespace'],'--type=json','--patch-file=/dev/stdin','-o','json'],path,body)


class RegistryTransport:
    """Reuse the pinned registry helper; secrets remain in memory, no reflection."""
    def __init__(self, publication, helper_ref, root, *, authorize):
        authorize();self.authorize=authorize;self.pub=publication;self.root=private_dir(str(root))
        path=Path(helper_ref['path']);require(hashlib.sha256(path.read_bytes()).hexdigest()==helper_ref['sha256'],'REGISTRY_HELPER_CHANGED')
        spec=importlib.util.spec_from_file_location('_opsloop_fixed_registry',path);self.helper=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.helper)
        self.registry,self.repo=publication['repository'].split('/',1)
        require(self.registry==self.helper.REGISTRY and self.repo in self.helper.REPOSITORIES,'REGISTRY_TARGET_CHANGED')
        self.tokens=self.helper.authenticate([self.repo]);self.token=self.tokens[self.repo]
        self.record={'operations':[]};self.receipt=self.root/'registry-operations.json'
        require(not self.receipt.exists(),'REGISTRY_ATTEMPT_EXISTS');self.save()
    def save(self):
        raw=encoded(self.record)
        require(not any(t.encode() in raw for t in self.helper.TOKENS),'REGISTRY_CREDENTIAL_REFLECTION')
        write(self.receipt,raw)
    def head(self,repository,tag):
        self.authorize();require(repository==self.pub['repository'] and tag==self.pub['newTag'],'REGISTRY_TARGET_CHANGED')
        status,_,_=self.helper.api('HEAD','/v2/'+self.repo+'/manifests/'+tag,token=self.token)
        return status
    def publish_digest(self,p,index,manifests,blobs):
        self.authorize();require(p==self.pub,'REGISTRY_TARGET_CHANGED')
        image={'repository':self.repo,'item':{'indexDigest':p['digest'],'imageRef':p['repository']+'@'+p['digest']},'index':index,'manifests':manifests,'blobs':blobs}
        # Every helper request rechecks the fixed session deadline, including uploads.
        original=self.helper.api
        def bounded(*args,**kwargs): self.authorize();return original(*args,**kwargs)
        self.helper.api=bounded
        try:self.helper.publish([image],self.tokens,self.record,self.save)
        finally:self.helper.api=original
    def create_tag(self,p,index,if_none_match):
        from urllib.request import Request,build_opener,ProxyHandler
        from urllib.error import HTTPError
        self.authorize();require(p==self.pub and if_none_match=='*','REGISTRY_TAG_CONDITION_CHANGED')
        self.record['operations'].append({'operation':'create-tag','tag':p['newTag'],'digest':p['digest'],'state':'DISPATCHING'});self.save()
        req=Request(self.helper.ORIGIN+'/v2/'+self.repo+'/manifests/'+p['newTag'],data=index,
            headers={'Authorization':'Bearer '+self.token,'Content-Type':self.helper.MEDIA_INDEX,'Accept':self.helper.ACCEPT,'If-None-Match':'*'},method='PUT')
        try:
            with build_opener(ProxyHandler({}),self.helper.NoRedirect()).open(req,timeout=120) as response:
                status=response.status;headers=dict(response.headers);response.read(65536)
        except HTTPError as exc:status=exc.code;headers=dict(exc.headers);exc.close()
        result={'status':status,'digest':self.helper.header(headers,'Docker-Content-Digest')}
        self.record['operations'][-1]['response']=result;self.save();return result
    def get_tag(self,p):
        self.authorize();require(p==self.pub,'REGISTRY_TARGET_CHANGED')
        status,headers,data=self.helper.api('GET','/v2/'+self.repo+'/manifests/'+p['newTag'],token=self.token)
        require(status==200 and self.helper.header(headers,'Docker-Content-Digest')==p['digest'],'REGISTRY_TAG_UNCONFIRMED')
        return data
