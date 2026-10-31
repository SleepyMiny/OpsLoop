"""Bounded fixed-Policy session: explicit host approval only, no import-time I/O.

Default command validates local pins. Execution never replays a reserved session.
All generated identities come from current API responses or newly issued grants.
"""
from __future__ import annotations
import argparse,base64,copy,datetime as dt,hashlib,importlib.util,json,os,sqlite3,subprocess,time,uuid
from pathlib import Path
from tools.exhibition_hashes import require,encoded,verify,describe,private_read
from tools.exhibition_inputs import pin,new,iso,migration_config,fresh_payload,materialize
from tools.exhibition_lifecycle import private_dir,write,verify_completion
from tools.exhibition_preparation import authority,reserve,PreparationAPI,RegistryTransport,publish_once,deploy_once,restore_controller_once,oci_payload
from tools.exhibition_stages import preparation_gate,experiment_gate,stopped,ready,pod_inventory


def load_module(ref,name):
    require(pin(ref['path'])==ref,'EXECUTION_HELPER_CHANGED')
    spec=importlib.util.spec_from_file_location(name,ref['path']);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m


def local_pins(plan):
    from tools.exhibition_preflight import verify_sources,contract_pins
    from tools.exhibition_readiness import verify_entrypoints
    if 'continuation' in plan:
        from tools.exhibition_deletion import continuation_config
        continuation_config(plan)
        for name in ('previousExecutionPlan','previousExecutionResult'):
            ref=plan['continuation'][name];private_read(ref['path'],ref['sha256'])
    contract_pins(plan);verify_sources(plan);oci_payload(plan['deployment']['imagePublication'])
    manifest=verify_entrypoints(plan['executionInputs']['entrypoints'])
    require(manifest.get('configurationStatus')=='SEALED_LOCAL_CONFIGURATION','EXECUTION_CONFIGURATION_UNSEALED')
    for ref in plan['executionInputs']['filePins']:require(pin(ref['path'])==ref,'EXECUTION_FILE_CHANGED')
    require(plan['authority']['order']==['A1','A2','B1','B2'] and plan['authority']['maxNewPermits']==4,'SESSION_ORDER_CHANGED')
    return {'state':'LOCAL_PINS_PASS','liveReadiness':False,'writes':0}


def host_idle(integration):
    """Read-only durable claims; never calls the cleanup routine as a check."""
    for root,table in [(integration['controlState'],'runs'),(integration['engineering']['state'],'jobs')]:
        path=Path(root)/'ledger.sqlite3'
        if not path.exists():continue
        require(not path.is_symlink(),'HOST_LEDGER_UNSAFE')
        db=sqlite3.connect('file:'+str(path)+'?mode=ro',uri=True)
        try:
            if table=='jobs':
                for status,raw in db.execute('SELECT state,detail FROM jobs'):
                    require(status in ('VERIFIED_PENDING_REVIEW','ENGINEERING_FAILED','CANCELLED','PR_CREATED'),'HOST_MODEL_TERMINATION_UNCONFIRMED')
                    # Existing terminal claims are not recycled by the new permit.
                    owner=json.loads(raw).get('owner',{})
                    if owner.get('pid'):
                        proc=Path('/proc')/str(owner['pid'])/'stat'
                        if proc.exists():require(proc.read_text().rsplit(')',1)[1].split()[19]!=str(owner.get('startTicks')),'HOST_OWNER_ACTIVE')
            else:
                for raw, in db.execute('SELECT value FROM runs'):
                    r=json.loads(raw);require(r.get('lifecycleComplete') is True,'HOST_CLEANUP_REQUIRED')
                    verify_completion(Path(root)/'lifecycle'/r['runId']/'completion.json',r['lifecycleReceiptSHA256'])
        finally:db.close()
    return {'state':'LOCAL_CLAIMS_TERMINAL'}


def preparation_report(plan,objects):
    """Collect independent checks; no network and no write transport available."""
    from tools.exhibition_readiness import Checks,historical_jobs,auxiliary_check,pointer,MissingObservation
    from tools.exhibition_preflight import preflight,object_path
    checks=Checks(lambda path: by_path[path]);e=plan['executionInputs'];by_path={r['path']:objects[r['key']] for r in e['readRequests'] if r['key'] in objects}
    def read(path):return by_path[path]
    def check(name,basis,fn):checks.check(name,basis,'Preparation gate; never authorizes Enforce',fn)
    check('raw.contract','/objectHashContract',lambda:preflight(plan,read))
    def cluster():
        if 'cluster' not in objects:raise MissingObservation()
        require(objects['cluster']['metadata']['uid']==plan['clusterUID'],'CLUSTER_UID_CHANGED')
    check('cluster.identity','/clusterUID',cluster)
    check('stages','/executionInputs/stages',lambda:preparation_gate(plan,objects))
    def resource_floor():
        floor=plan['trials']['A']['resourceConditions']
        require(floor['cpuUtilizationFloorPermille']==900 and 900>objects['a-policy']['spec']['filter']['maxCPUUtilizationPercent']*10,'CPU_FLOOR_DOES_NOT_EXCLUDE')
        require(any(n['metadata']['name']==floor['nodeName'] and n['metadata']['uid']==floor['nodeUID'] for n in objects['nodes']['items']),'RESOURCE_NODE_UID_CHANGED')
    check('a.cpu-floor','/trials/A/resourceConditions',resource_floor)
    for key,t in plan['fixedTargets'].items():
        def identity(k=key,t=t):
            ns=objects['namespace-'+t['namespace']];require(ns['metadata']['uid']==t['namespaceUID'] and ns['status']['phase']=='Active','NAMESPACE_CHANGED')
            ps=objects[k+'-policies'];require(not ps.get('metadata',{}).get('continue') and len(ps['items'])==1 and ps['items'][0]['metadata']['uid']==t['policyUID'],'FIXED_POLICY_COUNT_CHANGED')
            service=objects[k+'-service'];require(service['metadata']['uid']==e['services'][k]['uid'] and service['spec']==e['services'][k]['spec'],'SERVICE_CHANGED')
        check(key+'.identity','/fixedTargets/'+key,identity)
    def historical():
        jobs={'items':[objects[r['object']['object']['name']] for r in plan['readinessContract']['historicalJobs']]}
        historical_jobs(plan['readinessContract']['historicalJobs'],jobs,objects['a-pods'],set(),())
    check('historical','/readinessContract/historicalJobs',historical)
    for c in plan['readinessContract']['comparisons']:
        def compare(c=c):
            obj=read(c['path'])
            for eq in c['equals']:
                if eq.get('absent'):require(eq['pointer'].strip('/') not in obj,'UNEXPECTED_RBAC_FIELD')
                else:require(pointer(obj,eq['pointer'])==eq['value'],'COMPARISON_CHANGED')
        check(c['id'],c['basis'],compare)
    for a in plan['readinessContract']['requiredEvidence']:
        if a['kind'] in ('nodes','controller-instance'):
            check(a['id'],a['basis'],lambda a=a:auxiliary_check(a,read,plan))
    def system():
        require(objects[e['authorityConfigName']].get('absent') is True,'AUTHORITY_CONFIG_EXISTS')
        # Empty ResourceQuota list is affirmative evidence of no namespace quota.
        q=objects['system-quotas'];require(not q.get('metadata',{}).get('continue'),'SYSTEM_QUOTA_INCOMPLETE')
        require(q.get('items')==[],'SYSTEM_QUOTA_REQUIRES_EXPLICIT_CAPACITY_REVIEW')
    check('system.quota','/deployment/controller',system)
    def operator():
        role=objects['clusterroles-cluster-admin'];binding=objects['clusterrolebindings-cluster-admin']
        require(role['metadata']['uid']==e['operatorRoleUID'] and role['rules']==e['operatorRules'] and binding['metadata']['uid']==e['operatorBindingUID']
            and binding['roleRef']=={'apiGroup':'rbac.authorization.k8s.io','kind':'ClusterRole','name':'cluster-admin'}
            and any(s=={'apiGroup':'rbac.authorization.k8s.io','kind':'Group','name':'system:masters'} for s in binding['subjects']),'OPERATOR_RULES_CHANGED')
        require(pin(e['integration']['kubeconfig']['path'])==e['integration']['kubeconfig'],'OPERATOR_IDENTITY_CHANGED')
    check('operator.permissions','/executionInputs/operatorIdentity',operator)
    check('host.idle','/cleanupAndRollback',lambda:host_idle(e['integration']))
    rows=checks.rows
    return {'state':'PREPARATION_INPUTS_PASS' if all(r['status']=='PASS' for r in rows) else 'PREPARATION_BLOCKED','checks':rows,
        'mutationRequests':0,'scope':'Saved-response local analysis unless called with fresh execution capture',
        'pending':['Authenticated exact ACR tag HEAD must be 404 before any change','B baseline restoration and Ready','New Controller response/instance','Per-run signed authority and baseline/quota recheck','Actual model account availability before B']}


def trial_gate(plan,key,objects,receipt):
    from tools.exhibition_inputs import approved_circuit_signatures
    approved_circuit_signatures(plan,key,objects[key+'-policy'])
    from tools.exhibition_readiness import auxiliary_check,historical_jobs
    e=plan['executionInputs']
    for k,t in plan['fixedTargets'].items():
        pol=objects[k+'-policy'];stopped(pol,t['policyUID']);pod_inventory(plan,k,objects,require_ready=True)
        ps=objects[k+'-policies'];require(not ps.get('metadata',{}).get('continue') and len(ps['items'])==1 and ps['items'][0]['metadata']['uid']==t['policyUID'],'FIXED_POLICY_COUNT_CHANGED')
        require(pol['status'].get('incidentSequence',0)>=t['observedIncidentSequence'],'POLICY_HISTORY_RESET')
        inc=objects[k+'-incidents'];require(not inc.get('metadata',{}).get('continue') and inc['items']==[],'PREVIOUS_OR_FOREIGN_INCIDENT_PRESENT')
        for annotation in ('opsloop.io/exhibition-permit','opsloop.io/execution-grant'):
            require(annotation not in pol['metadata'].get('annotations',{}),'RETIRED_AUTHORITY_REQUIRED')
        for q in objects[k+'-quotas']['items']:
            hard=q['spec']['hard'];used=q['status']['used']
            require(hard.get('count/recoverypolicies.opsloop.io')=='1' and used.get('count/recoverypolicies.opsloop.io')=='1','FIXED_POLICY_QUOTA_CHANGED')
            if 'count/recoveryincidents.opsloop.io' in hard:require(int(used['count/recoveryincidents.opsloop.io'])+1<=int(hard['count/recoveryincidents.opsloop.io']),'INCIDENT_QUOTA_INSUFFICIENT')
    records=plan['readinessContract']['historicalJobs']
    historical_jobs(records,{'items':[objects[r['object']['object']['name']] for r in records]},objects['a-pods'],set(),())
    for a in plan['readinessContract']['requiredEvidence']:
        if a['kind']=='nodes':auxiliary_check(a,lambda path:objects['nodes'],plan)
    return experiment_gate(plan,key,objects[key+'-deployment'],objects[key+'-policy'],objects[key+'-replicasets'],objects[key+'-quotas'],receipt['instance'],receipt['expectedInstance'])


class Reader:
    def __init__(self,plan,root,deadline):
        self.plan,self.root,self.deadline=plan,Path(root),deadline;e=plan['executionInputs'];i=e['integration']
        self.audit=load_module(i['audit'],'_execution_audit');self.argv=[i['kubectl']['path'],'--kubeconfig',i['kubeconfig']['path'],'--context','default','--request-timeout=5s']
        self.allowed={r['path']:r for r in e['readRequests']}
    def get(self,path,*,log=False,absent=False):
        require(time.time()<self.deadline,'SESSION_READ_WINDOW_EXPIRED')
        require(path in self.allowed,'SESSION_GET_OUT_OF_SCOPE')
        result,receipt=self.audit.audited_run(self.root/'commands',self.argv+['get','--raw',path],target=path,timeout=8)
        require(time.time()<self.deadline,'SESSION_READ_WINDOW_EXPIRED')
        if absent and result.returncode==1 and not result.stdout and b'Error from server (NotFound):' in result.stderr:return {'absent':True}
        require(result.returncode==0,'SESSION_GET_FAILED')
        return result.stdout.decode() if log else json.loads(result.stdout)
    def snapshot(self,*,initial=False):
        result={}
        for path,r in list(self.allowed.items()):
            if r.get('log'):continue
            if not initial and r.get('initialOnly'):continue
            result[r['key']]=self.get(path,log=r.get('log',False),absent=r.get('absent',False))
        # New logs are scoped to the actual owned Pod returned by this GET.
        c=self.plan['deployment']['controller'];owned={x['metadata']['uid'] for x in result['controller-replicasets']['items'] if any(o.get('uid')==c['uid'] and o.get('controller') is True for o in x['metadata'].get('ownerReferences',[]))}
        for p in result['controller-pods']['items']:
            if any(o.get('uid') in owned and o.get('controller') is True for o in p['metadata'].get('ownerReferences',[])):
                path='/api/v1/namespaces/'+c['namespace']+'/pods/'+p['metadata']['name']+'/log?container=manager&limitBytes=65536'
                self.allowed[path]={'key':'controller-log-'+p['metadata']['uid'],'log':True}
        # Pending/image-pull containers have no logs yet. Preserve their actual
        # status in the capture; rollout readiness remains mandatory downstream.
        current_log_keys={'controller-log-'+p['metadata']['uid'] for p in result['controller-pods']['items']
            if any(c.get('name')=='manager' and c.get('state',{}).get('running') for c in p.get('status',{}).get('containerStatuses',[]))}
        for path,r in list(self.allowed.items()):
            if r.get('log') and r['key'] in current_log_keys and r['key'] not in result:result[r['key']]=self.get(path,log=True)
        new(self.root,'capture-'+uuid.uuid4().hex+'.json',{'at':iso(dt.datetime.now(dt.timezone.utc)),'objects':result})
        return result


def deployed_receipt(plan,objects,before,public_key,prior):
    from tools.exhibition_preparation import controller_transition
    expected=controller_transition(plan,before,public_key)
    verify(objects['controller'],describe(expected['after'],'/spec'),fields='/spec')
    require(ready(objects['controller']),'CONTROLLER_ROLLOUT_NOT_READY')
    c=plan['deployment']['controller'];rs={r['metadata']['uid'] for r in objects['controller-replicasets']['items'] if any(o.get('uid')==c['uid'] and o.get('controller') for o in r['metadata'].get('ownerReferences',[]))}
    pods=[p for p in objects['controller-pods']['items'] if any(o.get('uid') in rs and o.get('controller') for o in p['metadata'].get('ownerReferences',[]))]
    require(len(pods)==1 and pods[0]['metadata']['uid']!=prior['podUID'] and not pods[0]['metadata'].get('deletionTimestamp'),'CONTROLLER_ROLLOUT_NOT_READY')
    p=pods[0];cs=p.get('status',{}).get('containerStatuses',[])
    require(len(cs)==1 and cs[0].get('ready') is True and cs[0].get('restartCount')==0,'CONTROLLER_CONTAINER_UNCONFIRMED')
    identities=[]
    for line in objects['controller-log-'+p['metadata']['uid']].splitlines():
        try:v=json.loads(line)
        except ValueError:continue
        if v.get('schema')=='opsloop-controller-execution-identity/v1':identities.append(v)
    require(len(identities)==1 and identities[0]['instance']!=prior['instance'] and identities[0]['enforceDefault']=='deny-without-fresh-bound-grant','NEW_INSTANCE_UNCONFIRMED')
    image=cs[0]['imageID'];require(any(image.endswith(d) for d in plan['executionInputs']['controllerImageIDs']),'CONTROLLER_IMAGE_ID_CHANGED')
    cm=objects[plan['executionInputs']['authorityConfigName']]
    require(cm.get('data')==expected['configMap']['data'] and cm.get('immutable') is True and cm['metadata'].get('uid'),'NEW_AUTHORITY_CONFIG_UNCONFIRMED')
    return {'state':'STATIC_CONTROLLER_READY','deploymentUID':c['uid'],'podUID':p['metadata']['uid'],'podName':p['metadata']['name'],
        'instance':identities[0]['instance'],'expectedInstance':identities[0]['instance'],'priorInstance':prior['instance'],'priorPodUID':prior['podUID'],
        'imageID':image,'containerID':cs[0]['containerID'],'node':p['spec']['nodeName'],'configMapUID':cm['metadata']['uid']}


def execute(plan,approval_sha,*,approved_at,completion_deadline):
    """The only mutating entrypoint. Must be invoked once after separate approval."""
    local_pins(plan);e=plan['executionInputs'];i=e['integration'];start=dt.datetime.now(dt.timezone.utc)
    approved=dt.datetime.fromisoformat(approved_at);limit=dt.datetime.fromisoformat(completion_deadline)
    require(approved<=start<=approved+dt.timedelta(minutes=30),'APPROVAL_START_WINDOW')
    require(limit>=start+dt.timedelta(minutes=90),'INSUFFICIENT_FULL_SESSION_BUDGET')
    root=private_dir(i['sessionRoot']);reserve(root,'session',approval_sha)
    session={'approvedPlanSHA256':approval_sha,'planContentSHA256':hashlib.sha256(encoded(plan)).hexdigest(),
        'approvedAt':approved_at,'startedAt':iso(start),'preparationDeadline':iso(start+dt.timedelta(minutes=20)),
        'deadline':iso(start+dt.timedelta(minutes=90)),'completionDeadline':completion_deadline,
        'livePreflightState':'NOT_CHECKED','livePreflightScope':'CURRENT_GET_ONLY',
        'modelChecks':[c for v in e['migrationModelChecks'].values() for c in v]}
    session_file=root/'session.json';write(session_file,encoded(session));reader=Reader(plan,root,(start+dt.timedelta(minutes=20)).timestamp())
    if 'continuation' in plan:
        from tools.exhibition_deletion import confirm_prior_deletion
        try:confirm_prior_deletion(plan,reader,root,session)
        except BaseException as exc:
            new(root,'result.json',{'state':'NO_GO_PRIOR_DELETION_UNCONFIRMED','completed':[],'errorType':type(exc).__name__,'nextRunAllowed':False})
            raise
    objects=reader.snapshot(initial=True);report=preparation_report(plan,objects);new(root,'preparation-report.json',report)
    if report['state']!='PREPARATION_INPUTS_PASS':
        new(root,'result.json',{'state':'NO_GO_BEFORE_MUTATION','completed':[],'reason':'PREPARATION_BLOCKED','nextRunAllowed':False})
        raise ValueError('PREPARATION_BLOCKED')
    # This label covers preparation only. Neither Ready nor a new instance is
    # asserted; adapters/experiment_gate must establish both after mutations.
    session.update(livePreflightState='PREPARATION_INPUTS_PASS',preflightPhase='PREPARATION');write(session_file,encoded(session))
    authority(plan,approval_sha,session)
    from tools.exhibition_account import read_once
    try:
        new(root,'account-before-preparation.json',read_once(i['engineering'],authorize=lambda:authority(plan,approval_sha,session)))
        reg=RegistryTransport(plan['deployment']['imagePublication'],i['registryHelper'],root/'registry',authorize=lambda:authority(plan,approval_sha,session))
        pub=plan['deployment']['imagePublication'];require(reg.head(pub['repository'],pub['newTag'])==404,'REGISTRY_TAG_EXISTS_OR_UNKNOWN')
    except BaseException as exc:
        new(root,'result.json',{'state':'NO_GO_BEFORE_CLUSTER_CHANGE','completed':[],'errorType':type(exc).__name__,'reason':'ACCOUNT_OR_REGISTRY_PREFLIGHT_FAILED','nextRunAllowed':False})
        raise
    completed=[];controls=[];current=None;deployed=False
    try:
        from tools.exhibition_migration import migrate
        migrations={}
        for key in ('a','b'):
            authority(plan,approval_sha,session)
            cfg=new(root,'migration-'+key+'.json',migration_config(plan,key,approval_sha=approval_sha,deadline=session['preparationDeadline'],integration=i))
            migrations[key]=migrate(cfg['path'],cfg['sha256'])
        publish_once(plan,approval_sha,session,root,reg)
        keyfile=root/'issuer-private.pem';require(not keyfile.exists(),'ISSUER_KEY_ALREADY_EXISTS')
        fd=os.open(keyfile,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
        with os.fdopen(fd,'wb') as out:
            r=subprocess.run(['/usr/bin/openssl','genpkey','-algorithm','ED25519'],stdout=out,stderr=subprocess.PIPE,timeout=5)
        require(r.returncode==0,'ISSUER_KEY_CREATION_FAILED')
        r=subprocess.run(['/usr/bin/openssl','pkey','-in',str(keyfile),'-pubout','-outform','DER'],capture_output=True,timeout=5)
        from tools.exhibition_permit import PUBLIC_DER_PREFIX,issue
        require(r.returncode==0 and r.stdout.startswith(PUBLIC_DER_PREFIX) and len(r.stdout)==44,'ISSUER_PUBLIC_KEY_FAILED')
        public=base64.b64encode(r.stdout[-32:]).decode()
        api=PreparationAPI(plan,approval_sha,session,root,i['audit'],i['kubectl'],i['kubeconfig'])
        # Reserve before call: an ambiguous PATCH must also take the restore path.
        deployed=True;deploy_once(plan,approval_sha,session,root,api,public)
        before=objects['controller'];prior=e['priorController'];receipt=None
        while time.time()<reader.deadline:
            observed=reader.snapshot()
            try:receipt=deployed_receipt(plan,observed,before,public,prior);break
            except Exception as exc:
                if str(exc) not in ('CONTROLLER_ROLLOUT_NOT_READY','CONTROLLER_CONTAINER_UNCONFIRMED','NEW_INSTANCE_UNCONFIRMED'):raise
            time.sleep(2)
        require(receipt is not None,'CONTROLLER_READY_TIMEOUT');new(root,'controller-receipt.json',receipt)
        reader.deadline=dt.datetime.fromisoformat(session['deadline']).timestamp()
        previous={'path':migrations['a']['completion'],'sha256':migrations['a']['sha256']}
        for ordinal,label in enumerate(('A1','A2','B1','B2'),1):
            key=label[0].lower();remaining=sum((300 if n<=2 else 900)+180 for n in range(ordinal,5))+180
            require(time.time()+remaining<reader.deadline,'NEXT_RUN_BUDGET_INSUFFICIENT')
            host_idle(i);verify_completion(previous['path'],previous['sha256'])
            objects=reader.snapshot();objects['authority-config']=objects[e['authorityConfigName']]
            current_receipt=deployed_receipt(plan,objects,before,public,prior);require(current_receipt==receipt,'CONTROLLER_RESTART_BLOCKS_NEXT_RUN')
            t=plan['fixedTargets'][key];trial_gate(plan,key,objects,receipt)
            if key=='b':
                def account_authorize():require(time.time()+1080<reader.deadline,'B_WINDOW_INSUFFICIENT')
                new(root,label+'-account.json',read_once(i['engineering'],authorize=account_authorize))
            enf=copy.deepcopy(objects[key+'-policy']);enf['spec'].update(enabled=True,mode='Enforce')
            helper=i['policyHash'];require(pin(helper['path'])==helper,'POLICY_HASH_HELPER_CHANGED')
            spec_ref=new(root,'enforce-spec-'+label+'.json',enf['spec'])
            r=subprocess.run([helper['path'],spec_ref['path']],capture_output=True,timeout=10);policy_hash=r.stdout.decode().strip()
            require(r.returncode==0 and len(policy_hash)==64,'POLICY_HASH_FAILED')
            runid='live-'+label.lower()+'-'+uuid.uuid4().hex;epoch=uuid.uuid4().hex;runroot=private_dir(str(root/runid))
            payload=fresh_payload(plan,key,objects,receipt,policy_hash=policy_hash,run_id=runid,epoch=epoch,authorization_id='approved-'+uuid.uuid4().hex,
                ordinal=ordinal,previous_completion_sha=previous['sha256'],now=dt.datetime.now(dt.timezone.utc))
            pref=new(runroot,'permit-payload.json',payload)
            approval=new(runroot,'permit-approval.json',{'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],
                'authorizationID':payload['authorizationID'],'expiresAt':payload['grant']['notAfter'],'previousCompletion':previous['path'],'previousCompletionSHA256':previous['sha256'],
                'targetMigration':{'path':migrations[key]['completion'],'sha256':migrations[key]['sha256']}})
            out=runroot/'permit.json';checksum=issue(pref['path'],pref['sha256'],approval['path'],approval['sha256'],keyfile,i['issuanceState'],out)
            outer,ref=materialize(plan,key,objects,receipt,payload,{'path':str(out),'sha256':checksum,'publicKey':public},runroot,i,approval_sha=approval_sha,session_deadline=session['deadline'])
            controls.append(ref);control=new(root,'control-'+label+'.json',{'schema':'opsloop-exhibition-control/v1','plans':controls})
            current=(control,outer);session['modelChecks'].append({'state':i['engineering']['state'],'binding':outer['binding']});write(session_file,encoded(session))
            from tools.exhibition_session import run_once
            result=run_once(control,i['controlState'],outer['planId'],'once-'+runid,stop_at=session['deadline'])
            new(root,label+'-result.json',result)
            require(result.get('lifecycleComplete') is True,'RUN_CLEANUP_UNCONFIRMED')
            require(result.get('executionState',result.get('state')) == 'COMPLETED','RUN_FAILED_NO_NEXT_TRIAL')
            previous={'path':str(Path(i['controlState'])/'lifecycle'/result['runId']/'completion.json'),'sha256':result['lifecycleReceiptSHA256']}
            verify_completion(previous['path'],previous['sha256']);completed.append(label);current=None
        final=reader.snapshot()
        for key,t in plan['fixedTargets'].items():stopped(final[key+'-policy'],t['policyUID']);verify(final[key+'-deployment'],t['baselineSpecHash'],fields='/spec');require(ready(final[key+'-deployment']),'FINAL_BASELINE_UNCONFIRMED')
        new(root,'result.json',{'state':'COMPLETE','completed':completed,'controller':receipt,'nextRunAllowed':True})
    except BaseException as exc:
        # No replay. Coordinator stop/reconcile only; restore is gated by fences.
        failures=[]
        if current is not None:
            try:
                from tools.exhibition_session import run_once
                run_once(current[0],i['controlState'],None,None,stop_at=session['deadline'],reconcile_run=current[1]['binding']['runID'])
            except BaseException as cleanup:failures.append(type(cleanup).__name__)
        if deployed:
            try:
                from tools.exhibition_reconcile import model_jobs_stopped
                api=PreparationAPI(plan,approval_sha,session,root,i['audit'],i['kubectl'],i['kubeconfig'],cleanup=True)
                restore_controller_once(plan,approval_sha,session,root,api,model_jobs_stopped)
                reader.deadline=dt.datetime.fromisoformat(session['deadline']).timestamp()
                restored=False
                while time.time()<reader.deadline:
                    observed=reader.snapshot();verify(observed['controller'],plan['deployment']['controller']['expectedSpecHash'],fields='/spec')
                    if ready(observed['controller']):restored=True;break
                    time.sleep(2)
                require(restored,'CONTROLLER_RESTORE_ROLLOUT_TIMEOUT')
                new(root,'controller-restore-readback.json',{'state':'ORIGINAL_SPEC_READY','uid':observed['controller']['metadata']['uid'],'generation':observed['controller']['metadata']['generation']})
            except BaseException as cleanup:failures.append(type(cleanup).__name__)
        new(root,'result.json',{'state':'STOPPED_NO_REPLAY','completed':completed,'errorType':type(exc).__name__,'cleanupErrors':failures,'rollbackRollout':'CONFIRMED' if (root/'controller-restore-readback.json').exists() else 'NOT_CONFIRMED','nextRunAllowed':False})
        raise


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--plan',required=True);p.add_argument('--sha256',required=True)
    p.add_argument('--execute',action='store_true');p.add_argument('--approved-at');p.add_argument('--completion-deadline')
    a=p.parse_args();plan=private_read(a.plan,a.sha256)
    if not a.execute:print(json.dumps(local_pins(plan)));return
    require(a.approved_at and a.completion_deadline,'SEPARATE_EXECUTION_APPROVAL_REQUIRED')
    execute(plan,a.sha256,approved_at=a.approved_at,completion_deadline=a.completion_deadline)
if __name__=='__main__':main()
