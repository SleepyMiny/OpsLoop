"""Single-target private credential recovery. Never logs auth bytes or HTTP bodies.

Kubernetes Secret and ACR tokens remain in process memory. No command_audit,
credential command arguments, credential file output, registry content writes,
identity fallback, password revocation, or automatic PATCH retry.
"""
import base64,copy,datetime as dt,hashlib,importlib.util,json,resource,time,urllib.request,urllib.error
from pathlib import Path
from urllib.parse import urlencode
from tools.exhibition_runtime import Transport
from tools.exhibition_hashes import private_read
from tools.exhibition_lifecycle import write

class Stop(Exception):pass
def need(ok,code):
    if not ok:raise Stop(code)
def claims(token):
    try:
        bits=token.split('.');need(len(bits)==3,'TOKEN_FORMAT_UNSUPPORTED')
        return json.loads(base64.urlsafe_b64decode(bits[1]+'='*(-len(bits[1])%4)))
    except (ValueError,IndexError,TypeError):raise Stop('TOKEN_FORMAT_UNSUPPORTED') from None
def scope(c,repo):
    a=c.get('access',[])
    need(len(a)==1 and a[0].get('type')=='repository' and a[0].get('name')==repo and a[0].get('actions')==['pull'],'PULL_SCOPE_CHANGED')
    need(isinstance(c.get('sub'),str) and c['sub'] and isinstance(c.get('iss'),str) and c['iss'],'IDENTITY_UNCONFIRMED')
    need(type(c.get('exp')) in (int,float),'EXPIRY_UNCONFIRMED')
    return {k:c[k] for k in ('iss','sub')}
def secret_auth(secret,key):
    need(secret.get('type')=='kubernetes.io/dockerconfigjson' and not secret.get('immutable'),'SECRET_TYPE_OR_IMMUTABLE')
    try:
        cfg=json.loads(base64.b64decode(secret['data']['.dockerconfigjson'],validate=True));entry=cfg['auths'][key]
        need(set(entry)=={'auth'},'AUTH_ENTRY_UNSUPPORTED')
        username,password=base64.b64decode(entry['auth'],validate=True).decode().split(':',1)
        need(username=='00000000-0000-0000-0000-000000000000','EXISTING_AUTH_TYPE_UNSUPPORTED')
        return cfg,username,password
    except (KeyError,ValueError,UnicodeError,TypeError):raise Stop('SECRET_AUTH_UNCONFIRMED') from None
def replacement(secret,key,username,token):
    cfg,olduser,old=secret_auth(secret,key);need(username==olduser,'AUTH_USERNAME_CHANGED')
    before=claims(old);after=claims(token);repo=key.split('/',1)[1]
    need(scope(before,repo)==scope(after,repo),'AUTH_IDENTITY_CHANGED')
    need(after['exp']>time.time()+1800,'NEW_AUTH_LIFETIME_INSUFFICIENT')
    cfg['auths'][key]['auth']=base64.b64encode((username+':'+token).encode()).decode()
    value=base64.b64encode(json.dumps(cfg,sort_keys=True,separators=(',',':')).encode()).decode()
    expected=copy.deepcopy(secret);expected['data']['.dockerconfigjson']=value
    patch=[{'op':'test','path':'/metadata/uid','value':secret['metadata']['uid']},
           {'op':'test','path':'/metadata/resourceVersion','value':secret['metadata']['resourceVersion']},
           {'op':'test','path':'/type','value':secret['type']},
           {'op':'test','path':'/data/.dockerconfigjson','value':secret['data']['.dockerconfigjson']},
           {'op':'replace','path':'/data/.dockerconfigjson','value':value}]
    return expected,patch

def main():
    resource.setrlimit(resource.RLIMIT_CORE,(0,0))
    import argparse
    parser=argparse.ArgumentParser();parser.add_argument('--operation');parser.add_argument('--sha256');args=parser.parse_args()
    R=Path.cwd();B=R/'.opsloop/live-integration';O=B/'pull-recovery'
    deadline=dt.datetime.fromisoformat('2026-10-29T07:55:17+00:00').timestamp();minimum=1800
    if args.operation:
        operation=private_read(args.operation,args.sha256)
        need(operation.get('schema')=='opsloop-private-pull-operation/v1','OPERATION_SCHEMA')
        authorization=private_read(operation['authorization']['path'],operation['authorization']['sha256'])
        issued=dt.datetime.fromisoformat(authorization['receivedAt']).timestamp();end=dt.datetime.fromisoformat(authorization['workDeadline']).timestamp()
        deadline=dt.datetime.fromisoformat(operation['deadline']).timestamp()
        if authorization.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
            from tools.exhibition_b_work import work_auth
            work_auth(operation['authorization'],deadline=deadline)
        else:need(issued<=time.time()<deadline<=end<=issued+7200,'OPERATION_WINDOW')
        need(operation['secretUID']=='0e9e2750-4f99-48c2-8417-6b9cb8b004ac' and operation['repository']=='opsloop/demo-a/controller','OPERATION_TARGET')
        O=Path(operation['receiptRoot']);need(O.is_absolute() and O.is_relative_to(B/'evening-r8'),'OPERATION_ROOT');O.mkdir(mode=0o700,parents=True,exist_ok=True)
        minimum=operation['minimumValiditySeconds'];need(1800<=minimum<=3300,'AUTH_VALIDITY_BOUND')
    def bounded():need(time.time()<deadline,'ORIGINAL_EXECUTION_DEADLINE')
    record={'schema':'opsloop-private-pull-recovery/v1','startedAt':dt.datetime.now(dt.timezone.utc).isoformat(),'state':'DIAGNOSING','credentialBytesPersisted':False,'secretPatched':False,'nodePullVerified':False,'registryContentWrites':0}
    def save():write(O/'result.json',json.dumps(record,sort_keys=True).encode())
    # Exactly one invocation, including authentication and any uncertain mutation.
    write(O/'attempt.json',b'{"state":"RESERVED","maxSecretPatches":1}',exclusive=True);save();api=None
    try:
        bounded();p=private_read(B/'delegated-execution/execution-proposal-r7.json','129a65984a7d1026f1adfb792e87bad015a5327eb20e4f9194155772ead59a65')
        i=p['executionInputs']['integration'];pub=copy.deepcopy(p['deployment']['imagePublication'])
        if args.operation and authorization.get('schema')=='opsloop-b2-circuit-work-approval/v1':
            need(authorization['controllerRepository']==pub['repository'],'CONTROLLER_REPOSITORY_CHANGED');pub['digest']=authorization['candidateDigest']
        registry,repo=pub['repository'].split('/',1)
        registry_key=registry+'/'+repo
        api=Transport(i)
        dep_path='/apis/apps/v1/namespaces/opsloop-system/deployments/opsloop-controller'
        dep=api.request(dep_path);need(dep['metadata']['uid']==p['deployment']['controller']['uid'],'CONTROLLER_UID_CHANGED')
        ps=dep['spec']['template']['spec'];name='opsloop-demo-a-controller-pull'
        need({'name':name} in ps.get('imagePullSecrets',[]),'PULL_SECRET_NOT_ATTACHED')
        sa=api.request('/api/v1/namespaces/opsloop-system/serviceaccounts/'+ps['serviceAccountName'])
        pods=api.request('/api/v1/namespaces/opsloop-system/pods?limit=100');need(not pods.get('metadata',{}).get('continue'),'POD_INVENTORY_INCOMPLETE')
        refs=[{'name':x['metadata']['name'],'uid':x['metadata']['uid']} for x in pods['items'] if {'name':name} in x.get('spec',{}).get('imagePullSecrets',[])]
        need(bool(refs),'POD_PULL_ATTACHMENT_UNCONFIRMED')
        path='/api/v1/namespaces/opsloop-system/secrets/'+name
        bounded();secret=api.request(path);need(secret['metadata']['uid']=='0e9e2750-4f99-48c2-8417-6b9cb8b004ac','SECRET_UID_CHANGED');cfg,username,old=secret_auth(secret,registry_key);oldc=claims(old);identity=scope(oldc,repo)
        need(secret['metadata']['namespace']=='opsloop-system' and secret['metadata']['name']==name,'SECRET_TARGET_CHANGED')
        record.update(secret={'namespace':'opsloop-system','name':name,'uid':secret['metadata']['uid'],'resourceVersionBefore':secret['metadata']['resourceVersion'],'type':secret['type']},
                      registry=registry,repository=repo,scope=['pull'],authenticationKind='ACR repository access JWT in dockerconfigjson auth',
                      identitySHA256=hashlib.sha256(json.dumps(identity,sort_keys=True).encode()).hexdigest(),
                      oldExpiry=dt.datetime.fromtimestamp(oldc['exp'],dt.timezone.utc).isoformat(),oldExpired=oldc['exp']<=time.time(),
                      observedPodReferences=refs,serviceAccount={'name':ps['serviceAccountName'],'uid':sa['metadata']['uid']},
                      otherEntriesPreserved=True,sharedPasswordRevoked=False)
        save()
        ref=i['registryHelper'];need(hashlib.sha256(Path(ref['path']).read_bytes()).hexdigest()==ref['sha256'],'REGISTRY_HELPER_CHANGED')
        spec=importlib.util.spec_from_file_location('_private_pull_helper',ref['path']);helper=importlib.util.module_from_spec(spec);spec.loader.exec_module(helper)
        opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),helper.NoRedirect())
        def token_for_pod(password):
            bounded()
            request=urllib.request.Request('https://'+registry+'/oauth2/token?'+urlencode({'service':registry,'scope':'repository:'+repo+':pull'}),headers={'Authorization':'Basic '+base64.b64encode((username+':'+password).encode()).decode()},method='GET')
            try:
                with opener.open(request,timeout=15) as r:data=r.read(1048577)
                need(len(data)<=1048576,'AUTH_RESPONSE_LIMIT');v=json.loads(data);token=v.get('token') or v.get('access_token');need(isinstance(token,str),'POD_AUTH_RESPONSE_INVALID');scope(claims(token),repo);return 200,token
            except urllib.error.HTTPError as exc:
                status=exc.code;exc.close();return status,None
        status,token=token_for_pod(old);record['oldPodAuthHTTP']=status;save()
        selected=old
        if status!=200 or oldc['exp']<time.time()+minimum:
            need(status in (200,401),'POD_AUTH_OTHER_ERROR');bounded()
            selected=helper.authenticate([repo],('pull',))[repo]
            need(scope(claims(selected),repo)==identity,'AUTH_IDENTITY_CHANGED')
            need(claims(selected)['exp']>=time.time()+minimum,'NEW_AUTH_LIFETIME_INSUFFICIENT')
            expected,patch=replacement(secret,registry_key,username,selected)
            status,token=token_for_pod(selected);need(status==200,'NEW_POD_AUTH_REJECTED')
        def images(token):
            result=[]
            rollback=dep['spec']['template']['spec']['containers'][0]['image'];need(rollback.startswith(pub['repository']+'@sha256:'),'ROLLBACK_REPOSITORY_CHANGED')
            restore_digest=authorization['restoreDigest'] if args.operation and authorization.get('schema')=='opsloop-b2-circuit-work-approval/v1' else rollback.split('@')[1]
            for digest in (pub['digest'],restore_digest):
                bounded();status,headers,raw=helper.api('GET','/v2/'+repo+'/manifests/'+digest,token=token)
                need(status==200 and helper.header(headers,'Docker-Content-Digest')==digest and 'sha256:'+hashlib.sha256(raw).hexdigest()==digest,'EXACT_IMAGE_PULL_UNCONFIRMED')
                doc=json.loads(raw);children=doc.get('manifests',[])
                for child in children:
                    bounded();cs,ch,body=helper.api('GET','/v2/'+repo+'/manifests/'+child['digest'],token=token)
                    need(cs==200 and 'sha256:'+hashlib.sha256(body).hexdigest()==child['digest'],'PLATFORM_MANIFEST_UNCONFIRMED')
                    manifest=json.loads(body)
                    for blob in [manifest['config'],*manifest['layers']]:
                        bounded();bs,bh,_=helper.api('HEAD','/v2/'+repo+'/blobs/'+blob['digest'],token=token)
                        need(bs==200 and helper.header(bh,'Docker-Content-Digest')==blob['digest'],'IMAGE_BLOB_PULL_UNCONFIRMED')
                result.append({'digest':digest,'manifestGET':200,'platformManifests':len(children)})
            return result
        record['prePatchImages']=images(token);record['hostPodCredentialFlowVerified']=True;save()
        if selected!=old:
            bounded();write(O/'patch-dispatch.json',json.dumps({'state':'PATCH_DISPATCHED_ONCE','uid':secret['metadata']['uid'],'resourceVersion':secret['metadata']['resourceVersion'],'target':path,'field':'/data/.dockerconfigjson'}).encode(),exclusive=True)
            try:api.request(path,patch=patch)
            except Exception:record['patchResponse']='UNCONFIRMED_READING_ONCE';save()
            bounded();after=api.request(path)
            need(after['metadata']['uid']==secret['metadata']['uid'] and after['type']==secret['type'] and after.get('data')==expected['data'],'SECRET_PATCH_READBACK_UNCONFIRMED')
            for field in ('labels','annotations','ownerReferences','finalizers'):need(after['metadata'].get(field)==secret['metadata'].get(field),'SECRET_METADATA_CHANGED')
            record.update(secretPatched=True,resourceVersionAfter=after['metadata']['resourceVersion'])
            _,user,stored=secret_auth(after,registry_key);need(user==username and stored==selected,'SECRET_STORED_AUTH_CHANGED')
            status,token=token_for_pod(stored);need(status==200,'STORED_POD_AUTH_REJECTED');record['postPatchImages']=images(token)
        record.update(state='PULL_AUTH_VERIFIED_HOST_ONLY',newExpiry=dt.datetime.fromtimestamp(claims(selected)['exp'],dt.timezone.utc).isoformat(),completedAt=dt.datetime.now(dt.timezone.utc).isoformat());save()
    except BaseException as exc:
        record.update(state='STOPPED',errorType=type(exc).__name__,reason=str(exc) if isinstance(exc,Stop) else 'PRIVATE_OPERATION_UNCONFIRMED');save()
    finally:
        if api:api.close()
    print(json.dumps(record))
if __name__=='__main__':main()
