"""New bounded work interval, reusing completed r7 custody and publication.
No migration, publication, key generation, ConfigMap creation, or budget reset.
"""
import argparse,base64,copy,datetime as dt,hashlib,json,os,sqlite3,subprocess,time,uuid
from pathlib import Path
from tools.exhibition_hashes import require,private_read,verify,encoded
from tools.exhibition_inputs import pin,new,iso,fresh_payload,materialize
from tools.exhibition_execution import Reader,host_idle,trial_gate,deployed_receipt
from tools.exhibition_lifecycle import private_dir,write,verify_completion
from tools.exhibition_preparation import PreparationAPI,controller_transition,restore_controller_once,reserve
from tools.exhibition_permit import PUBLIC_DER_PREFIX,issue
from tools.exhibition_stages import ready,stopped


def can_start(label,now,deadline,reserve_seconds=1200):
    return now+(300 if label.startswith('A') else 900)+180+reserve_seconds<deadline


def current_identity(plan,objects):
    c=plan['deployment']['controller'];verify(objects['controller'],c['expectedSpecHash'],fields='/spec')
    rs={x['metadata']['uid'] for x in objects['controller-replicasets']['items'] if any(v.get('uid')==c['uid'] and v.get('controller') for v in x['metadata'].get('ownerReferences',[]))}
    pods=[x for x in objects['controller-pods']['items'] if any(v.get('uid') in rs and v.get('controller') for v in x['metadata'].get('ownerReferences',[])) and not x['metadata'].get('deletionTimestamp')]
    require(len(pods)==1,'CURRENT_CONTROLLER_POD_UNCONFIRMED');p=pods[0];cs=p.get('status',{}).get('containerStatuses',[])
    require(len(cs)==1 and cs[0].get('ready') is True,'CURRENT_CONTROLLER_NOT_READY')
    records=[]
    for line in objects.get('controller-log-'+p['metadata']['uid'],'').splitlines():
        try:v=json.loads(line)
        except ValueError:continue
        if v.get('schema')=='opsloop-controller-execution-identity/v1':records.append(v)
    require(len(records)==1 and records[0].get('instance'),'CURRENT_INSTANCE_UNCONFIRMED')
    return {'podUID':p['metadata']['uid'],'podName':p['metadata']['name'],'instance':records[0]['instance'],'restartCount':cs[0].get('restartCount'),'containerID':cs[0]['containerID']}


def unused_budget(i):
    # These four rounds have not started. Never discard existing reservations.
    for path,table in [(Path(i['controlState'])/'ledger.sqlite3','runs'),(Path(i['issuanceState'])/'issuance.sqlite3','permits')]:
        if path.exists():
            db=sqlite3.connect('file:'+str(path)+'?mode=ro',uri=True)
            try:require(db.execute('SELECT COUNT(*) FROM '+table).fetchone()[0]==0,'EXISTING_CONSUMED_ROUND_REQUIRES_RECONCILIATION')
            finally:db.close()
    host_idle(i)


def replacement_budget(i, continuation):
    """One explicitly approved replacement; prior consumption stays immutable."""
    expected=continuation['replacesRunID']
    receipt=verify_completion(continuation['completion']['path'],continuation['completion']['sha256'])
    require(receipt['binding']['runID']==expected,'REPLACEMENT_COMPLETION_CHANGED')
    db=sqlite3.connect('file:'+str(Path(i['controlState'])/'ledger.sqlite3')+'?mode=ro',uri=True)
    try:
        runs=[json.loads(r[0]) for r in db.execute('SELECT value FROM runs')]
        require(len(runs)==1 and runs[0]['runId']==expected and runs[0]['state']=='STOPPED' and runs[0].get('lifecycleComplete') is True,'REPLACEMENT_PRIOR_RUN_CHANGED')
        events=[json.loads(r[0]) for r in db.execute('SELECT value FROM events')]
        require(not any(e['kind'] in ('FAULT_DISPATCHED','READINESS_FAULT','MODEL_REQUESTED') for e in events),'REPLACEMENT_BUDGET_ALREADY_USED')
    finally:db.close()
    prior=private_read(continuation['priorOutcome']['path'],continuation['priorOutcome']['sha256'])
    require(prior['binding']==receipt['binding'] and prior['result']['faultMaximumSeconds']==0,'REPLACEMENT_FAULT_UNCONFIRMED')
    require(not (Path(continuation['priorOutcome']['path']).parent/'fault-intent.json').exists(),'REPLACEMENT_FAULT_INTENT_EXISTS')
    db=sqlite3.connect('file:'+str(Path(i['issuanceState'])/'issuance.sqlite3')+'?mode=ro',uri=True)
    try:
        rows=db.execute('SELECT run,epoch,state FROM permits').fetchall()
        require(rows==[(expected,receipt['binding']['epoch'],'ISSUED')],'REPLACEMENT_ISSUANCE_CHANGED')
    finally:db.close()
    host_idle(i)


def a2_budget(i,c):
    receipt=verify_completion(c['completion']['path'],c['completion']['sha256'])
    require(receipt['binding']['runID']==c['previousRunID'],'A2_PREDECESSOR_CHANGED')
    outcome=private_read(c['priorOutcome']['path'],c['priorOutcome']['sha256'])
    require(outcome['binding']==receipt['binding'] and outcome['result']['serviceRecovered'] is True and outcome['result']['faultMaximumSeconds']==90,'A1_RECOVERY_UNCONFIRMED')
    db=sqlite3.connect('file:'+str(Path(i['controlState'])/'ledger.sqlite3')+'?mode=ro',uri=True)
    try:
        runs=[json.loads(r[0]) for r in db.execute('SELECT value FROM runs ORDER BY rowid')]
        require([r['runId'] for r in runs]==c['priorRunIDs'] and all(r.get('lifecycleComplete') is True for r in runs),'A2_PRIOR_LEDGER_CHANGED')
        events=[json.loads(r[0]) for r in db.execute('SELECT value FROM events')]
        require(sum(e['kind']=='FAULT_DISPATCHED' for e in events)==1 and sum(e['kind']=='READINESS_FAULT' for e in events)==1 and not any(e['actor']=='model' for e in events),'A2_REMAINING_BUDGET_UNCONFIRMED')
    finally:db.close()
    db=sqlite3.connect('file:'+str(Path(i['issuanceState'])/'issuance.sqlite3')+'?mode=ro',uri=True)
    try:require(db.execute('SELECT run,state FROM permits ORDER BY rowid').fetchall()==[(r,'ISSUED') for r in c['priorRunIDs']],'A2_ISSUANCE_CHANGED')
    finally:db.close()
    host_idle(i)


def rounds_for(cfg,auth):
    if auth.get('schema')=='opsloop-b2-circuit-work-approval/v1':
        from tools.exhibition_b_work import work_auth
        require(work_auth(cfg['authorization'])==auth and cfg.get('allowedRounds')==['B2'],'B2_ONLY_APPROVAL_REQUIRED')
        return [(4,'B2')]
    if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
        if cfg.get('allowedRounds')==['B2']:
            from tools.exhibition_b_work import b2_supplement
            b2_supplement(cfg);return [(4,'B2')]
        require(cfg.get('allowedRounds')==auth.get('allowedRounds')==['B1','B2'],'B_ONLY_APPROVAL_REQUIRED')
        return [(3,'B1'),(4,'B2')]
    if 'allowedRounds' in cfg:
        require(cfg['allowedRounds']==auth.get('allowedRounds')==['A2'],'A2_ONLY_APPROVAL_REQUIRED')
        return [(2,'A2')]
    return list(enumerate(('A1','A2','B1','B2'),1))


def transition_budget(label,now,deadline):
    # Include maximum rollout and UI barrier before a full round and cleanup.
    return can_start(label,now+300+90,deadline)


def session_deadline(now,auth,continuation=None):
    if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
        return dt.datetime.fromisoformat(auth['workDeadline'])
    deadline=min(now+dt.timedelta(seconds=5400),dt.datetime.fromisoformat(auth['workDeadline']))
    if continuation:
        previous=private_read(continuation['priorSession']['path'],continuation['priorSession']['sha256'])
        original=dt.datetime.fromisoformat(previous['startedAt'])
        deadline=min(deadline,dt.datetime.fromisoformat(previous['deadline']),original+dt.timedelta(seconds=5400))
    return deadline


def require_start_window(auth, now=None):
    now=time.time() if now is None else now
    cutoff=auth.get("noNewStartsAfter")
    if cutoff is not None:
        require(now<dt.datetime.fromisoformat(cutoff).timestamp(),"NEW_START_CUTOFF_REACHED")


def authenticate(plan,root,session,label):
    cfg={'schema':'opsloop-private-pull-operation/v1','authorization':plan['newInterval']['authorization'],
         'deadline':session['deadline'],'secretUID':'0e9e2750-4f99-48c2-8417-6b9cb8b004ac','repository':'opsloop/demo-a/controller',
         'receiptRoot':str(root/('pull-'+label)),'minimumValiditySeconds':3000}
    ref=new(root,'pull-operation-'+label+'.json',cfg)
    out=root/('pull-'+label+'.stdout');err=root/('pull-'+label+'.stderr')
    with out.open('xb') as stdout,err.open('xb') as stderr:
        os.chmod(out,0o600);os.chmod(err,0o600)
        r=subprocess.run(['python3','-m','tools.exhibition_pull_recovery','--operation',ref['path'],'--sha256',ref['sha256']],stdout=stdout,stderr=stderr,timeout=180)
    result=json.loads((Path(cfg['receiptRoot'])/'result.json').read_text())
    require(r.returncode==0 and result['state']=='PULL_AUTH_VERIFIED_HOST_ONLY','PULL_AUTH_UNCONFIRMED')
    return result


def restore_and_confirm(plan,approval_sha,session,root,reader):
    from tools.exhibition_reconcile import model_jobs_stopped
    i=plan['executionInputs']['integration']
    if not (root/'controller-restore.json').exists():
        api=PreparationAPI(plan,approval_sha,session,root,i['audit'],i['kubectl'],i['kubeconfig'],cleanup=True)
        restore_controller_once(plan,approval_sha,session,root,api,model_jobs_stopped)
    # A dispatched restore is only read back; never retransmit an uncertain PATCH.
    while time.time()<reader.deadline:
        observed=reader.snapshot()
        verify(observed['controller'],plan['deployment']['controller']['expectedSpecHash'],fields='/spec')
        if ready(observed['controller']):
            identity=current_identity(plan,observed)
            if not (root/'controller-restore-readback.json').exists():
                new(root,'controller-restore-readback.json',{'state':'ORIGINAL_SPEC_READY','uid':observed['controller']['metadata']['uid'],'generation':observed['controller']['metadata']['generation'],**identity})
            return
        time.sleep(2)
    raise RuntimeError('CONTROLLER_RESTORE_ROLLOUT_TIMEOUT')


def require_live_view(root,deadline):
    """GET-only readiness BEFORE consuming a candidate transition."""
    import urllib.request
    require(time.time()<deadline,'VIEW_WINDOW_EXPIRED')
    require((Path(root)/'view-ready.json').is_file(),'LIVE_VIEW_NOT_CONFIRMED_BEFORE_TRANSITION')
    values={}
    for path in ('/api/exhibition/capabilities','/api/exhibition/state','/api/a/api/v1/snapshot','/api/b/api/v1/snapshot'):
        require(time.time()<deadline,'VIEW_WINDOW_EXPIRED')
        with urllib.request.urlopen('http://127.0.0.1:4174'+path,timeout=4) as response:
            require(response.status==200,'LIVE_VIEW_GET_FAILED');values[path]=json.load(response)
    capabilities=values['/api/exhibition/capabilities']
    require(capabilities.get('control') is False and capabilities.get('observationConfigured') is True,'LIVE_VIEW_MODE_UNCONFIRMED')
    require(values['/api/exhibition/state'].get('active') is None,'LIVE_VIEW_ACTIVE_RUN_UNEXPECTED')
    require(time.time()<deadline,'VIEW_WINDOW_EXPIRED')
    return {'state':'LIVE_GET_AND_LEDGER_CONFIRMED_BEFORE_TRANSITION','at':time.time(),'GETs':4,'control':False}


def execute(plan,approval_sha):
    from tools.exhibition_finite_cli import reject_legacy_mix
    reject_legacy_mix(plan)
    now=dt.datetime.now(dt.timezone.utc);cfg=plan['newInterval'];auth=private_read(cfg['authorization']['path'],cfg['authorization']['sha256'])
    require(dt.datetime.fromisoformat(auth['receivedAt'])<=now<dt.datetime.fromisoformat(auth['workDeadline']),'NEW_INTERVAL_EXPIRED')
    for ref in cfg['sourcePins']:require(pin(ref['path'])==ref,'RESUME_SOURCE_CHANGED')
    if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
        from tools.exhibition_b_work import work_auth
        work_auth(cfg['authorization'])
    rounds=rounds_for(cfg,auth);require_start_window(auth)
    root=private_dir(cfg['sessionRoot']);reserve(root,'session-reserved',approval_sha)
    deadline=session_deadline(now,auth,cfg.get('continuation'))
    require(transition_budget(rounds[0][1],time.time(),deadline.timestamp()),'NO_TRANSITION_ROUND_AND_FINAL_CLEANUP_BUDGET')
    e=plan['executionInputs'];i=e['integration'];require(i['sessionRoot']==str(root),'SESSION_ROOT_CHANGED')
    # Plan is already sealed with this exact root; no mutation of approved inputs.
    session={'approvedPlanSHA256':approval_sha,'planContentSHA256':hashlib.sha256(encoded(plan)).hexdigest(),
      'approvedAt':auth['receivedAt'],'approvalReceivedAt':auth['receivedAt'],'startedAt':iso(now),'preparationDeadline':iso(min(now+dt.timedelta(minutes=20),deadline)),
      'deadline':iso(deadline),'completionDeadline':auth['workDeadline'],'livePreflightState':'NOT_CHECKED','livePreflightScope':'CURRENT_GET_ONLY',
      'modelChecks':[c for v in e['migrationModelChecks'].values() for c in v]}
    if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):session['delegatedWork']=cfg['authorization']
    if cfg.get('continuation'):
        prior_session=private_read(cfg['continuation']['priorSession']['path'],cfg['continuation']['priorSession']['sha256'])
        session.update(priorExecutionSession=cfg['continuation']['priorSession'])
        if auth.get('schema') not in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
            session.update(originalActualStartedAt=prior_session['startedAt'],supplementalApprovalAt=auth['receivedAt'])
    session_file=root/'session.json';write(session_file,encoded(session));reader=Reader(plan,root,deadline.timestamp())
    objects=reader.snapshot();prior=current_identity(plan,objects);new(root,'before-controller-identity.json',prior)
    if cfg.get('allowedRounds')==['B2']:
        old=private_read(cfg['continuation']['priorRestoration']['path'],cfg['continuation']['priorRestoration']['sha256'])['controller']
        require(all(prior[k]==old[k] for k in ('podUID','instance','containerID')),'B2_CONTROLLER_DIFFERS_FROM_RESTORE')
    require(objects['cluster']['metadata']['uid']==plan['clusterUID'],'CLUSTER_UID_CHANGED') if 'cluster' in objects else require(reader.get('/api/v1/namespaces/kube-system')['metadata']['uid']==plan['clusterUID'],'CLUSTER_UID_CHANGED')
    for key in ('a','b'):
        trial_gate(plan,key,objects,{'instance':prior['instance'],'expectedInstance':prior['instance']})
    if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
        from tools.exhibition_b_work import previous_completed
        previous_completed(i,cfg['continuation'])
    elif cfg.get('allowedRounds')==['A2']:a2_budget(i,cfg['continuation'])
    elif cfg.get('continuation'):replacement_budget(i,cfg['continuation'])
    else:unused_budget(i)
    # Explicit replay authorization is checked BEFORE any transition. History cannot grant it.
    host_idle(i)
    replay_authorizations={}
    for ordinal,label in rounds:
        if label=='B2':
            from tools.exhibition_replay import pretransition_replay
            enf=copy.deepcopy(objects['b-policy']['spec']);enf.update(enabled=True,mode='Enforce')
            helper=i['policyHash'];require(pin(helper['path'])==helper,'POLICY_HASH_HELPER_CHANGED')
            spec_ref=new(root,'pretransition-enforce-spec.json',enf)
            result=subprocess.run([helper['path'],spec_ref['path']],capture_output=True,timeout=10)
            require(result.returncode==0,'POLICY_HASH_FAILED')
            replay_authorizations['b']=pretransition_replay(plan,'b',objects,prior,result.stdout.decode().strip())
            require(dt.datetime.fromisoformat(replay_authorizations['b']['notAfter']).timestamp()-time.time()>=1200,'REPLAY_INTENT_WINDOW_INSUFFICIENT')
            from tools.exhibition_account import read_once
            new(root,'pretransition-account.json',read_once(i['engineering'],authorize=lambda: require(time.time()+2280<reader.deadline,'B_WINDOW_INSUFFICIENT')))
    # Repeated GET stability is measured, not assumed from past Wi-Fi/Node state.
    probes=[]
    for _ in range(3):
        start=time.monotonic();nodes=reader.get('/api/v1/nodes?limit=100');require(all(any(c.get('type')=='Ready' and c.get('status')=='True' for c in n.get('status',{}).get('conditions',[])) for n in nodes['items']),'NODE_CONNECTION_UNSTABLE')
        require({n['metadata']['uid'] for n in nodes['items']}=={n['metadata']['uid'] for n in objects['nodes']['items']},'NODE_IDENTITY_CHANGED')
        probes.append(time.monotonic()-start);time.sleep(1)
    new(root,'connection.json',{'GETs':3,'seconds':probes,'readyUIDsStable':True})
    require(max(probes)<5,'CONNECTION_TOO_SLOW')
    oldroot=Path(cfg['priorSession']);migrations={}
    for key in ('a','b'):
        m=json.loads((oldroot/('migration-'+key+'.json')).read_text());receipt=Path(m['stateRoot'])/m['migrationID']/'completion.json';ref=pin(receipt);verify_completion(ref['path'],ref['sha256']);migrations[key]={'completion':ref['path'],'sha256':ref['sha256']}
    pub=private_read(cfg['publication']['path'],cfg['publication']['sha256']);require(pub['state']=='PUBLISHED_CONFIRMED' and pub['digest']==plan['deployment']['imagePublication']['digest'],'PUBLICATION_RECEIPT_CHANGED')
    keyfile=oldroot/'issuer-private.pem';require(keyfile.stat().st_mode&0o077==0,'KEY_PERMISSION')
    r=subprocess.run(['/usr/bin/openssl','pkey','-in',str(keyfile),'-pubout','-outform','DER'],capture_output=True,timeout=5)
    require(r.returncode==0 and r.stdout.startswith(PUBLIC_DER_PREFIX) and len(r.stdout)==44,'EXISTING_PUBLIC_KEY_FAILED');public=base64.b64encode(r.stdout[-32:]).decode()
    before=objects['controller'];change=controller_transition(plan,before,public);cm=objects[e['authorityConfigName']]
    require(cm['metadata']['uid']==cfg['configMapUID'] and cm.get('immutable') is True and cm['data']==change['configMap']['data'],'EXISTING_STATIC_AUTHORITY_CHANGED')
    session.update(livePreflightState='PREPARATION_INPUTS_PASS',preflightPhase='PREPARATION');write(session_file,encoded(session))
    view_end=min(time.time()+90,reader.deadline-1200)
    while not (root/'view-ready.json').exists() and time.time()<view_end:time.sleep(1)
    new(root,'view-pretransition.json',require_live_view(root,view_end))
    authenticate(plan,root,session,'before-transition')
    completed=[];controls=[];current=None;deployed=False;receipt=None
    try:
        require_start_window(auth)
        require(transition_budget(rounds[0][1],time.time(),reader.deadline),'NO_TRANSITION_BUDGET_BEFORE_TRANSITION')
        api=PreparationAPI(plan,approval_sha,session,root,i['audit'],i['kubectl'],i['kubeconfig'])
        actual=api.get('/apis/apps/v1/namespaces/opsloop-system/deployments/opsloop-controller');change=controller_transition(plan,actual,public)
        write(root/'controller-restore-input.json',encoded(change),exclusive=True)
        if auth.get('schema') in ('opsloop-b-work-approval/v1','opsloop-b2-circuit-work-approval/v1'):
            from tools.exhibition_b_work import reserve_transition
            reserve_transition(cfg,root,approval_sha,plan['deployment']['imagePublication']['digest'])
        reserve(root,'controller-transition',approval_sha);deployed=True
        api.patch('/apis/apps/v1/namespaces/opsloop-system/deployments/opsloop-controller',change['patch'])
        rollout_end=min(time.time()+300,reader.deadline-1200)
        while time.time()<rollout_end:
            observed=reader.snapshot()
            try:receipt=deployed_receipt(plan,observed,before,public,prior);break
            except Exception as exc:
                if str(exc) not in ('CONTROLLER_ROLLOUT_NOT_READY','CONTROLLER_CONTAINER_UNCONFIRMED','NEW_INSTANCE_UNCONFIRMED'):raise
            time.sleep(2)
        require(receipt is not None,'CONTROLLER_ROLLOUT_TIMEOUT');new(root,'controller-receipt.json',receipt)
        event_path='/api/v1/namespaces/opsloop-system/events?fieldSelector=involvedObject.uid%3D'+receipt['podUID']
        reader.allowed[event_path]={'key':'candidate-start-events'}
        events=reader.get(event_path);new(root,'candidate-start-events.json',events)
        new(root,'node-pull-observation.json',{'imageID':receipt['imageID'],'podUID':receipt['podUID'],'ready':True,'cacheReported':any('already present' in x.get('message','') for x in events.get('items',[]))})
        view_end=min(time.time()+90,reader.deadline-1200)
        while not (root/'view-ready.json').exists() and time.time()<view_end:time.sleep(1)
        require((root/'view-ready.json').exists(),'LIVE_VIEW_NOT_CONFIRMED')
        from tools.exhibition_account import read_once
        previous=cfg['continuation'].get('predecessorCompletion',cfg['continuation']['completion']) if cfg.get('continuation') else {'path':migrations['a']['completion'],'sha256':migrations['a']['sha256']}
        for ordinal,label in rounds:
            require_start_window(auth)
            key=label[0].lower()
            if (root/'operator-stop').exists() or not can_start(label,time.time(),reader.deadline):
                new(root,'no-next-round.json',{'label':label,'reason':'TIME_OR_OPERATOR_STOP','deadline':session['deadline']});break
            host_idle(i);verify_completion(previous['path'],previous['sha256'])
            objects=reader.snapshot();objects['authority-config']=objects[e['authorityConfigName']]
            current_receipt=deployed_receipt(plan,objects,before,public,prior);require(current_receipt==receipt,'CONTROLLER_RESTART_BLOCKS_NEXT_RUN')
            t=plan['fixedTargets'][key];trial_gate(plan,key,objects,receipt)
            if key=='b':
                authenticate(plan,root,session,label)
                def account_authorize():require(time.time()+1080<reader.deadline,'B_WINDOW_INSUFFICIENT')
                new(root,label+'-account.json',read_once(i['engineering'],authorize=account_authorize))
            enf=copy.deepcopy(objects[key+'-policy']);enf['spec'].update(enabled=True,mode='Enforce')
            helper=i['policyHash'];require(pin(helper['path'])==helper,'POLICY_HASH_HELPER_CHANGED')
            spec_ref=new(root,'enforce-spec-'+label+'.json',enf['spec'])
            r=subprocess.run([helper['path'],spec_ref['path']],capture_output=True,timeout=10);policy_hash=r.stdout.decode().strip()
            require(r.returncode==0 and len(policy_hash)==64,'POLICY_HASH_FAILED')
            runid='live-'+label.lower()+'-'+uuid.uuid4().hex;epoch=uuid.uuid4().hex;authorization_id='approved-'+uuid.uuid4().hex
            if key in replay_authorizations:
                selected=replay_authorizations[key];runid=selected['scope']['runID'];epoch=selected['scope']['epoch'];authorization_id=selected['authorizationID']
            runroot=private_dir(str(root/runid))
            payload=fresh_payload(plan,key,objects,receipt,policy_hash=policy_hash,run_id=runid,epoch=epoch,authorization_id=authorization_id,
                ordinal=ordinal,previous_completion_sha=previous['sha256'],now=dt.datetime.now(dt.timezone.utc))
            if label=='A1' and cfg.get('continuation'):new(runroot,'replacement-of.json',{'replacesRunID':cfg['continuation']['replacesRunID'],'previousCompletion':previous,'newRunID':runid,'priorActualFaults':0,'newAuthorizationID':payload['authorizationID']})
            pref=new(runroot,'permit-payload.json',payload)
            issuance_approval={'schema':'opsloop-permit-issuance-approval/v1','approved':True,'payloadSHA256':pref['sha256'],
                'authorizationID':payload['authorizationID'],'expiresAt':payload['grant']['notAfter'],'previousCompletion':previous['path'],'previousCompletionSHA256':previous['sha256'],
                'targetMigration':{'path':migrations[key]['completion'],'sha256':migrations[key]['sha256']}}
            if payload.get('circuitReplay') is not None:
                issuance_approval['circuitReplayApproval']=e['circuitReplayApproval'][key]
                issuance_approval['controllerReceipt']=pin(root/'controller-receipt.json')
            approval=new(runroot,'permit-approval.json',issuance_approval)
            out=runroot/'permit.json';checksum=issue(pref['path'],pref['sha256'],approval['path'],approval['sha256'],keyfile,i['issuanceState'],out)
            outer,ref=materialize(plan,key,objects,receipt,payload,{'path':str(out),'sha256':checksum,'publicKey':public},runroot,i,approval_sha=approval_sha,session_deadline=session['deadline'])
            controls.append(ref);control=new(root,'control-'+label+'.json',{'schema':'opsloop-exhibition-control/v1','plans':controls})
            current=(control,outer);session['modelChecks'].append({'state':i['engineering']['state'],'binding':outer['binding']});write(session_file,encoded(session))
            from tools.exhibition_session import run_once
            if key=='b':
                from tools.exhibition_session import run_in_process
                result=run_in_process(control,i['controlState'],outer['planId'],'once-'+runid,stop_at=session['deadline'],output_root=runroot,total_seconds=outer['totalSeconds'])
            else:
                result=run_once(control,i['controlState'],outer['planId'],'once-'+runid,stop_at=session['deadline'])
            new(root,label+'-result.json',result)
            require(result.get('lifecycleComplete') is True,'RUN_CLEANUP_UNCONFIRMED')
            require(result.get('executionState',result.get('state')) == 'COMPLETED' and (key!='b' or result.get('trialSucceeded') is True),'RUN_FAILED_NO_NEXT_TRIAL')
            previous={'path':str(Path(i['controlState'])/'lifecycle'/result['runId']/'completion.json'),'sha256':result['lifecycleReceiptSHA256']}
            verify_completion(previous['path'],previous['sha256']);completed.append(label);current=None
        restore_and_confirm(plan,approval_sha,session,root,reader)
        final=reader.snapshot()
        for key,t in plan['fixedTargets'].items():stopped(final[key+'-policy'],t['policyUID']);verify(final[key+'-deployment'],t['baselineSpecHash'],fields='/spec');require(ready(final[key+'-deployment']),'FINAL_BASELINE_UNCONFIRMED')
        new(root,'result.json',{'state':'APPROVED_ROUNDS_COMPLETE' if len(completed)==len(rounds) else 'PARTIAL_TIME_STOP','completed':completed,'controller':receipt,'nextRunAllowed':True})
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
                restore_and_confirm(plan,approval_sha,session,root,reader)
            except BaseException as cleanup:failures.append(type(cleanup).__name__)
        new(root,'result.json',{'state':'STOPPED_NO_REPLAY','completed':completed,'errorType':type(exc).__name__,'cleanupErrors':failures,'rollbackRollout':'CONFIRMED' if (root/'controller-restore-readback.json').exists() else 'NOT_CONFIRMED','nextRunAllowed':False})
        raise


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--plan',required=True);parser.add_argument('--sha256',required=True)
    actions=parser.add_mutually_exclusive_group();actions.add_argument('--finite-slot',metavar='AUTHORIZATION_ID');actions.add_argument('--finite-close',action='store_true')
    args=parser.parse_args();plan=private_read(args.plan,args.sha256)
    if args.finite_slot is not None or args.finite_close:
        from tools.exhibition_finite_cli import execute as finite_execute
        result=finite_execute(plan,args.sha256,authorization_id=args.finite_slot,close=args.finite_close)
        print(json.dumps(result))
    else:execute(plan,args.sha256)
if __name__=='__main__':main()
