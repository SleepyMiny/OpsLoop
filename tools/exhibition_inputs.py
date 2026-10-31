"""Fixed-Policy input assembly from approved constants and current response bindings.

No Kubernetes requests, signing, model calls or publication. Dynamic UID/instance
values must come from the observed deployment receipt and are checked by the
existing adapters again before execution. No historical Runtime bundle is used.
"""
from __future__ import annotations
import copy,datetime as dt,hashlib,json,os,subprocess
from pathlib import Path
from tools.exhibition_hashes import require,verify,describe,digest,encoded
from tools.exhibition_lifecycle import write,private_dir
from tools.exhibition_stages import stopped,ready,experiment_gate


def pin(path):return {'path':str(path),'sha256':hashlib.sha256(Path(path).read_bytes()).hexdigest()}
def new(root,name,value):
    path=Path(root)/name;write(path,encoded(value),exclusive=True);return pin(path)
def local(tool,args=(),payload=None):
    require(hashlib.sha256(Path(tool['path']).read_bytes()).hexdigest()==tool['sha256'],'INPUT_HELPER_CHANGED')
    r=subprocess.run([tool['path'],*args],input=None if payload is None else encoded(payload),capture_output=True,timeout=15)
    require(r.returncode==0,'LOCAL_INPUT_HELPER_REJECTED')
    return json.loads(r.stdout)
def iso(value):return value.astimezone(dt.timezone.utc).isoformat()


def approved_circuit_signatures(plan,key,policy):
    # Compatibility name; return expected history only, never execution exceptions.
    from tools.exhibition_replay import expected_history
    return expected_history(plan,key,policy)


def fresh_payload(plan,key,objects,controller_receipt,*,policy_hash,run_id,epoch,authorization_id,ordinal,previous_completion_sha,now):
    replay_ref=plan['executionInputs'].get('circuitReplayApproval',{}).get(key)
    intent=None
    if replay_ref is not None:
        from tools.exhibition_replay import replay_intent
        intent=replay_intent(replay_ref)
    finite=intent is not None and intent['schema']=='opsloop-circuit-replay-intent/v4'
    require((finite and key=='b' and ordinal==intent['ordinal']) or (not finite and key in ('a','b') and 1<=ordinal<=4 and ('a' if ordinal<=2 else 'b')==key),'SESSION_ORDER_CHANGED')
    t=plan['fixedTargets'][key];dep=objects[key+'-deployment'];pol=objects[key+'-policy'];c=controller_receipt
    experiment_gate(plan,key,dep,pol,objects[key+'-replicasets'],objects[key+'-quotas'],c['instance'],c['expectedInstance'])
    require(c['state']=='STATIC_CONTROLLER_READY' and c['deploymentUID']==plan['deployment']['controller']['uid']
            and (finite or (c['podUID']!=c['priorPodUID'] and c['instance']!=c['priorInstance'])),'NEW_CONTROLLER_RECEIPT_REQUIRED')
    scope={'clusterUID':plan['clusterUID'],'namespaceUID':t['namespaceUID'],'namespace':t['namespace'],'policy':t['policy'],
           'policyUID':t['policyUID'],'policyGeneration':pol['metadata']['generation']+2,'policyHash':policy_hash,
           'deployment':t['deployment'],'deploymentUID':t['deploymentUID'],
           'incident':t['policy']+'-'+t['policyUID'][:8]+'-'+str(pol['status'].get('incidentSequence',0)+1).zfill(5),'runID':run_id,'epoch':epoch}
    grant={'version':'opsloop-execution-grant/v1','scope':scope,'instance':c['instance'],'notBefore':iso(now),'notAfter':iso(now+dt.timedelta(seconds=600))}
    # B passes grants through the Go wire helper, which emits RFC3339 UTC Z.
    # Normalize before signing; never change an issued payload or time budget.
    if key=='b':
        for field in ('notBefore','notAfter'):grant[field]=grant[field].replace('+00:00','Z')
    signatures=approved_circuit_signatures(plan,key,pol)
    result={'schema':'opsloop-exhibition-permit/v1','grant':grant,'authorizationID':authorization_id,'ordinal':ordinal,
        'baseIncidentSequence':pol['status'].get('incidentSequence',0),'maxIncidents':1,'maxActions':1,
        'previousCompletionSHA256':previous_completion_sha,'allowedCircuitSignatures':[]}
    binding={k:scope[k] for k in ('runID','epoch','policyUID','deploymentUID')}
    if key=='a':
        n=plan['trials']['A']['resourceConditions']
        result['resourceConditions']={'schema':'opsloop-resource-conditions/v1',**binding,'notBefore':iso(now),'expiresAt':iso(now+dt.timedelta(seconds=300)),
            'nodes':[{k:n[k] for k in ('nodeName','nodeUID','cpuUtilizationFloorPermille')}]}
    else:result['modelBudget']={'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt','maxModelCalls':1,'totalSeconds':900,'expiresAt':iso(now+dt.timedelta(seconds=900))}
    replay_ref=plan['executionInputs'].get('circuitReplayApproval',{}).get(key)
    if replay_ref is not None:
        from tools.exhibition_replay import approved_replay,replay_intent,v2_roles
        intent=replay_intent(replay_ref)
        if intent['schema'] in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3'):result['schema']=intent['schema'].replace('opsloop-circuit-replay-intent/','opsloop-exhibition-permit/')
        if finite:result['schema']='opsloop-exhibition-permit/v3'
        if intent['schema'] in ('opsloop-circuit-replay-intent/v3','opsloop-circuit-replay-intent/v4'):require(intent['approvedReproduction']['baselineTemplate']==dep['spec']['template'],'REPRODUCTION_BASELINE_CHANGED')
        containers=dep['spec']['template']['spec']['containers']
        require(len(containers)==1,'REPLAY_SINGLE_IMAGE_REQUIRED')
        replay=approved_replay(replay_ref,result,containers[0]['image'],c)
        require(replay['signature'] in signatures,'REPLAY_SIGNATURE_NOT_IN_EXPECTED_HISTORY')
        result['circuitReplay']=replay;result['allowedCircuitSignatures']=[replay['signature']]
        if result['schema'] in ('opsloop-exhibition-permit/v2','opsloop-exhibition-permit/v3'):result.update(v2_roles(replay_ref,result,containers[0]['image']))
    from tools.exhibition_permit import validate_payload
    validate_payload(result,model=key=='b');return result


def cleanup_config(plan,key,binding,root,deadline,integration):
    t=plan['fixedTargets'][key];baseline=plan['executionInputs']['baselineSpecs'][key]
    template=baseline['template'];ops=[{'op':'add','path':'/spec/template/spec/nodeSelector','value':template['spec']['nodeSelector']},
         {'op':'add','path':'/spec/template/metadata/annotations/opsloop.io~1attempt-id','value':template['metadata']['annotations']['opsloop.io/attempt-id']}]
    if key=='b':ops.append({'op':'remove','path':'/spec/template/spec/containers/0/env/5/value'})
    return {'schema':'opsloop-exhibition-lifecycle/v1','binding':binding,'namespace':t['namespace'],'policy':t['policy'],'deployment':t['deployment'],
            'expiresAt':deadline,'apiOrigin':integration['apiOrigin'],'kubeconfig':integration['kubeconfig'],
            'baselineSpecHash':t['baselineSpecHash'],'baselineSpecSHA256':t['readyBaselineSpecSHA256'],
            'allowedRestorePaths':plan['cleanupAndRollback']['deploymentRestorePaths'][:2]+(['/spec/template/spec/containers/0/env/5/value'] if key=='b' else []),
            'restore':{'patch':ops},'baselineWaitSeconds':120,'allowPreArmDisable':True,
            'evidenceRoot':str(root),'archiveRoot':integration['archiveRoot']}


def migration_config(plan,key,*,approval_sha,deadline,integration):
    if key=='a' and 'continuation' in plan:
        from tools.exhibition_deletion import continuation_config,continuation_inputs
        cfg=continuation_config(plan);original,_=continuation_inputs(cfg)
        original=copy.deepcopy(original)
        original.update(expiresAt=deadline,approvalReference=approval_sha,resumeCustody=cfg['originalMigrationPlan'])
        return original
    from tools.exhibition_plan import migration_hash_inputs
    t=plan['fixedTargets'][key];cfg=migration_hash_inputs(plan,key)
    cfg.update(schema='opsloop-exhibition-migration/v1',approved=True,approvalReference=approval_sha,
        migrationID=integration['sessionID']+'-migration-'+key,expiresAt=deadline,
        target={'clusterUID':plan['clusterUID'],**{k:t[k] for k in ('namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}},
        evidenceRoot=plan['executionInputs']['captureRoot'],archiveRoot=integration['archiveRoot'],stateRoot=integration['migrationState'],
        evidence=plan['executionInputs']['migrationEvidence'][key],modelChecks=plan['executionInputs']['migrationModelChecks'][key],
        apiOrigin=integration['apiOrigin'],kubeconfig=integration['kubeconfig'],legacyAuthoritySHA256=t['legacyAuthoritySHA256'],retireAuthority=True,baselineWaitSeconds=120)
    if key=='b':
        cfg['restore']={'expectedSpecHash':t['observedDeploymentSpecHash'],'expectedSpecSHA256':t['observedDeploymentSpecSHA256'],'patch':t['baselineDerivation']['patch']}
        cfg['allowedRestorePaths']=['/spec/template/spec/containers/0/env/5/value']
    return cfg


def materialize(plan,key,objects,receipt,payload,permit_ref,root,integration,*,approval_sha,session_deadline):
    """Called in the same host process that owns the Guard and Coordinator."""
    root=private_dir(str(root));t=plan['fixedTargets'][key];dep=objects[key+'-deployment'];pol=objects[key+'-policy']
    scope=payload['grant']['scope'];binding={k:scope[k] for k in ('runID','epoch','policyUID','deploymentUID')}
    require(all(scope[k]==v for k,v in {'namespace':t['namespace'],'policyUID':t['policyUID'],'deploymentUID':t['deploymentUID']}.items()),'INPUT_TARGET_CHANGED')
    verify(dep,t['baselineSpecHash'],fields='/spec');stopped(pol,t['policyUID']);require(ready(dep),'BASELINE_REQUIRED')
    from tools.exhibition_permit import verify as permit_verify
    envelope=json.loads(Path(permit_ref['path']).read_bytes())
    require(pin(permit_ref['path'])['sha256']==permit_ref['sha256'] and permit_verify(envelope,permit_ref['publicKey'],model=key=='b')==payload,'ISSUED_PERMIT_CHANGED')
    before=copy.deepcopy(pol['spec']);observe={**copy.deepcopy(before),'enabled':True,'mode':'ObserveOnly'};enforce={**copy.deepcopy(before),'enabled':True,'mode':'Enforce'}
    total=300 if key=='a' else 900
    expires=payload['grant']['notAfter'] if key=='a' else payload['modelBudget']['expiresAt']
    outer={'schema':'opsloop-exhibition-plan/v1','planId':binding['runID'],'authorizationId':payload['authorizationID'],'approvalReference':approval_sha,
           'source':'measured','track':'runtime' if key=='a' else 'engineering','binding':binding,'expiresAt':expires,'totalSeconds':total,'exhibitionPermit':permit_ref}
    cfg=cleanup_config(plan,key,binding,root,session_deadline,integration);outer['lifecycle']=new(root,'cleanup-config.json',cfg)
    if key=='a':
        # Guard requires an existing private root; it never creates its trust root.
        private_dir(str(root/'guard'))
        g=payload['grant'];hard=dt.datetime.fromisoformat(g['notAfter']);auth_end=hard+dt.timedelta(seconds=60)
        driver=integration['driver'];owner=local(driver,['guard','owner','-pid',str(os.getpid())])
        auth={'version':'opsloop-workload-authorization/v1','approvalReference':approval_sha,**binding,'apiOrigin':integration['apiOrigin'],
            'clusterUID':plan['clusterUID'],'namespace':t['namespace'],'namespaceUID':t['namespaceUID'],'policy':t['policy'],'policyGeneration':scope['policyGeneration'],
            'policySpecSHA256':digest(enforce),'workload':t['deployment'],'maxTrialMs':300000,'notAfter':iso(auth_end+dt.timedelta(seconds=1)),'allowed':'get-and-disable-owned-policy-only'}
        auth_ref=new(root,'stop-authorization.json',auth)
        guard={'version':'opsloop-workload-safety/v1','mode':'hybrid-live','evidenceKind':'observed',**binding,'apiOrigin':integration['apiOrigin'],
            'kubeconfigFile':integration['kubeconfig']['path'],'clusterUID':plan['clusterUID'],'namespace':t['namespace'],'namespaceUID':t['namespaceUID'],
            'policy':t['policy'],'policyGeneration':scope['policyGeneration'],'policySpec':enforce,'workload':t['deployment'],'root':str(root/'guard'),
            'owner':owner,'hardDeadline':iso(hard),'authorityExpires':iso(auth_end),'maxTrialMs':300000,'heartbeatMs':10000,'timeoutMs':2000,
            'authorizationFile':auth_ref['path'],'authorizationSHA256':auth_ref['sha256']}
        guard_ref=new(root,'guard-config.json',guard);guard_binding=local(driver,['guard-workload','plan','-config',guard_ref['path']])['configBinding']
        wire=local(integration['grant'],payload={**g,'heartbeatSequence':1,'heartbeatAt':g['notBefore']})
        rs={v['metadata']['uid'] for v in objects['a-replicasets']['items'] if any(o.get('uid')==t['deploymentUID'] and o.get('controller') for o in v['metadata'].get('ownerReferences',[]))}
        pods=[v for v in objects['a-pods']['items'] if any(o.get('uid') in rs and o.get('controller') for o in v['metadata'].get('ownerReferences',[])) and v.get('status',{}).get('phase')=='Running' and not v['metadata'].get('deletionTimestamp')]
        require(len(pods)==1,'BASELINE_SOURCE_POD_AMBIGUOUS');pod=pods[0];cs=pod['status']['containerStatuses'];require(len(cs)==1 and cs[0]['ready'] and cs[0]['restartCount']==0,'SOURCE_CONTAINER_UNCONFIRMED')
        outer['runtime']={'namespace':t['namespace'],'policy':t['policy'],'workload':t['deployment'],'service':t['deployment'],'serviceUID':objects['a-service']['metadata']['uid'],
            'clusterUID':plan['clusterUID'],'namespaceUID':t['namespaceUID'],'container':cs[0]['name'],'sourcePod':{'name':pod['metadata']['name'],'uid':pod['metadata']['uid']},
            'nodes':[{'name':n['metadata']['name'],'uid':n['metadata']['uid']} for n in objects['nodes']['items']],
            'faultSeconds':90,'baselineSamples':3,'recoverySamples':5,'stableSeconds':30,'serviceURL':integration['serviceURLA'],'apiOrigin':integration['apiOrigin'],
            'guardConfig':guard_ref,'guardBinding':guard_binding,'grantBinding':wire['binding'],'driver':driver,'kubectl':integration['kubectl'],'kubeconfig':integration['kubeconfig'],
            'executionGrant':g,'policyBefore':{'generation':pol['metadata']['generation'],'spec':pol['spec'],'incidentSequence':payload['baseIncidentSequence']},
            'sourceImageID':cs[0]['imageID'],'sourcePodSpecSHA256':digest(pod['spec']),'deploymentSpecSHA256':digest(dep['spec']),
            'resourceConditions':new(root,'resource-conditions.json',payload['resourceConditions'])}
    else:
        verification=private_dir(str(root/'verification'))
        provenance_ref=integration['provenance'];provenance=json.loads(Path(provenance_ref['path']).read_bytes());require(pin(provenance_ref['path'])==provenance_ref,'PROVENANCE_CHANGED')
        prov=new(verification,'image-provenance.json',provenance)
        initial=new(verification,'initial-deployment-result-v2.json',{'source':'CURRENT_READY_BASELINE','deployment':dep})
        snap=new(root,'runtime-snapshot.json',{'objects':objects,'origin':'CURRENT_RUN_OBSERVATION'})
        fault=copy.deepcopy(dep['spec']['template']);fault['spec']['containers'][0]['env'][5]['value']='empty-order-after-30s'
        if payload['schema']=='opsloop-exhibition-permit/v3':require(payload['approvedReproduction']['baselineTemplate']==dep['spec']['template'] and payload['approvedReproduction']['template']==fault,'SIGNED_REPRODUCTION_RUNTIME_INPUT_CHANGED')
        ctrl=objects['controller'];cm=objects['authority-config'];p={
            'schema':'opsloop-demo-b-runtime-plan/v1','namespace':t['namespace'],'apiOrigin':integration['apiOrigin'],'clusterUID':plan['clusterUID'],
            'runID':binding['runID'],'epoch':binding['epoch'],'executionScope':scope,'scopeSHA256':digest(scope),'exhibitionPermit':permit_ref,'authorizationId':payload['authorizationID'],
            'initialPolicyGeneration':pol['metadata']['generation'],'initialIncidentSequence':payload['baseIncidentSequence'],'initialPolicySpec':before,'observePolicySpec':observe,'enforcePolicySpec':enforce,
            'controllerUID':ctrl['metadata']['uid'],'controllerSpec':ctrl['spec'],'controllerTemplate':ctrl['spec']['template'],'controllerImageRef':ctrl['spec']['template']['spec']['containers'][0]['image'],
            'controllerImageID':receipt['imageID'],'controllerNode':receipt['node'],'priorControllerPodUIDs':[receipt['priorPodUID']],
            'configMap':cm,'staticAuthorityKey':'authority.json','sourceDeploymentSpec':dep['spec'],'faultTemplate':fault,'serviceUID':objects['b-service']['metadata']['uid'],'serviceSpec':objects['b-service']['spec'],
            'demoA':{'policyName':plan['fixedTargets']['a']['policy'],'policyUID':objects['a-policy']['metadata']['uid'],'policySpec':objects['a-policy']['spec'],'policyGeneration':objects['a-policy']['metadata']['generation'],
                     'deploymentUID':objects['a-deployment']['metadata']['uid'],'deploymentSpec':objects['a-deployment']['spec']},
            'maximumTrialMS':600000,'heartbeatMS':10000,'renewEveryMS':2000,'expiresAt':expires,'snapshotFile':snap['path'],'snapshotSHA256':snap['sha256'],
            'runtimeScriptSHA256':integration['runtimeB']['sha256'],'helpers':{k:integration[k] for k in ('kubernetes','grant','quoteProbe')},
            'provenanceSHA256':prov['sha256'],'initialDeploymentSHA256':initial['sha256'],'conditionalModelAuthorization':integration['modelAuthorization']}
        p['helpers']['guard']=integration['driver'];runtime_ref=new(root,'runtime-plan.json',p)
        setup=new(root,'setup.json',{'state':'STATIC_CONTROLLER_READY_POLICY_DISABLED','planSHA256':runtime_ref['sha256'],'controllerGeneration':ctrl['metadata']['generation'],
            'authorityController':{'instance':receipt['instance'],'podName':receipt['podName'],'podUID':receipt['podUID'],'containerID':receipt['containerID'],
                                 'imageID':receipt['imageID'],'scopeConfigMapUID':cm['metadata']['uid']}})
        outer.update(model='gpt-6-luna',maxModelCalls=1,runtimeCapture={'root':str(root),'plan':runtime_ref['path'],'planSHA256':runtime_ref['sha256'],
            'setupReceipt':setup['path'],'setupReceiptSHA256':setup['sha256'],'provenance':prov['path'],'provenanceSHA256':prov['sha256']})
        outer['engineering']={**integration['engineering'],'bundle':str(root/'sealed/bundle.json'),'bundle_sha256':'',
            'execution_plan':str(root/'execution-plan.json'),'execution_plan_sha256':''}
    return outer,new(root,'exhibition-plan.json',outer)
