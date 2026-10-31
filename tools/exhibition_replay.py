"""Expected history is evidence, never authority. Explicit one-run circuit replay.
All reads are pinned local files. No Kubernetes, signing, model, or browser API.
"""
import re,time,hashlib,json
from pathlib import Path
from tools.exhibition_control import pinned,instant,ControlError
from tools.exhibition_lifecycle import verify_completion

def require(ok,reason):
 if not ok:raise ControlError(reason)

def evidence(refs,target):
 require(set(refs)=={'incident','bundle','completion'},'REPLAY_EVIDENCE_REFS_INVALID')
 inc=pinned(refs['incident']['path'],refs['incident']['sha256']);bundle=pinned(refs['bundle']['path'],refs['bundle']['sha256']);done=verify_completion(refs['completion']['path'],refs['completion']['sha256'])
 b=done['binding'];bi=bundle['incident'];work=bundle['workload']
 require(inc['status']['runtime']['phase']=='FAILED' and inc['spec']['trigger']['class']=='APPLICATION_FAILURE','REPLAY_NOT_TERMINAL_FAILURE')
 require(b['runID']==bi['run_id'] and b['epoch']==bi['epoch'] and inc['metadata']['uid']==bi['uid'],'REPLAY_PRIOR_RUN_MISMATCH')
 require(inc['spec']['trigger']['signature']==bi['runtime_signature'],'REPLAY_SIGNATURE_MISMATCH')
 require(inc['metadata']['annotations'].get('opsloop.io/exhibition-run')==b['runID'] and inc['metadata']['annotations'].get('opsloop.io/exhibition-epoch')==b['epoch'],'REPLAY_INCIDENT_RUN_MISMATCH')
 for field,rawfield,bundlefield in [('policyUID','policyRef','policy_uid'),('deploymentUID','workload','deployment_uid')]:
  require(b[field]==target[field]==inc['spec'][rawfield]['uid']==work[bundlefield],'REPLAY_TARGET_MISMATCH')
 require(inc['metadata']['namespace']==target['namespace']==work['namespace'],'REPLAY_NAMESPACE_MISMATCH')
 # Raw Incident and sealed bundle must themselves be in the verified archive.
 archive=pinned(done['archiveManifest'],done['archiveSHA256']);hashes={x['sha256'] for x in [*archive['files'],*archive['objects']]}
 require(refs['incident']['sha256'] in hashes and refs['bundle']['sha256'] in hashes,'REPLAY_EVIDENCE_NOT_ARCHIVED')
 return {'signature':bi['runtime_signature'],'priorRunID':bi['run_id'],'priorEpoch':bi['epoch'],'priorIncidentUID':bi['uid'],'incidentSHA256':refs['incident']['sha256'],'bundleSHA256':refs['bundle']['sha256'],'completionSHA256':refs['completion']['sha256'],'imageDigest':bundle['source']['image_digest']}

def expected_history(plan,key,policy):
 contract=plan['executionInputs']['expectedCircuitHistory'][key];target=plan['fixedTargets'][key]
 require(set(contract)=={'initialSignatures','additions'} and isinstance(contract['additions'],list) and len(contract['additions'])<=1,'HISTORY_CONTRACT_INVALID')
 expected=list(contract['initialSignatures'])
 require(len(expected)==len(set(expected)) and all(re.fullmatch('[a-f0-9]{64}',v) for v in expected),'HISTORY_INITIAL_INVALID')
 require(policy['metadata']['uid']==target['policyUID'],'HISTORY_POLICY_UID_CHANGED')
 for addition in contract['additions']:
  proof=evidence(addition['evidence'],target)
  require(proof['signature']==addition['signature'] and proof['signature'] not in expected,'HISTORY_ADDITION_MISMATCH');expected.append(proof['signature'])
 actual=[x['signature'] for x in policy.get('status',{}).get('circuits',[])]
 require(len(actual)==len(set(actual)) and set(actual)==set(expected),'UNEXPECTED_CIRCUIT_HISTORY')
 return expected

def approved_replay(ref,payload,image,controller_receipt=None):
 approval=pinned(ref['path'],ref['sha256']);scope=payload['grant']['scope'];grant=payload['grant']
 if approval.get('schema') in ('opsloop-circuit-replay-intent/v1','opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3'):
  return bind_intent(ref,payload,image,controller_receipt)
 require(set(approval)=={'schema','approved','purpose','grant','authorizationID','evidence','workloadImage','signature','previousCompletionSHA256'},'REPLAY_APPROVAL_SCHEMA')
 require(approval['schema']=='opsloop-circuit-replay-approval/v1' and approval['approved'] is True and approval['purpose']=='ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION','EXPLICIT_REPLAY_APPROVAL_REQUIRED')
 require(approval['grant']==grant and approval['authorizationID']==payload['authorizationID'],'REPLAY_APPROVAL_SCOPE_CHANGED')
 require(approval['workloadImage']==image and re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',image),'REPLAY_IMAGE_CHANGED')
 proof=evidence(approval['evidence'],scope)
 require(proof['signature']==approval['signature'] and image.endswith('@'+proof.pop('imageDigest')),'REPLAY_APPROVAL_EVIDENCE_CHANGED')
 require(proof['completionSHA256']==approval['previousCompletionSHA256']==payload['previousCompletionSHA256'],'REPLAY_PREDECESSOR_CHANGED')
 require(scope['runID']!=proof['priorRunID'] and scope['epoch']!=proof['priorEpoch'],'REPLAY_OLD_RUN')
 return {'purpose':approval['purpose'],'workloadImage':image,**proof,'approvalSHA256':ref['sha256']}

def validate_replay_shape(payload):
 replay=payload.get('circuitReplay');signatures=payload['allowedCircuitSignatures']
 if replay is None:
  require(not signatures,'EXPLICIT_REPLAY_APPROVAL_REQUIRED');return
 fields={'purpose','workloadImage','signature','priorRunID','priorEpoch','priorIncidentUID','incidentSHA256','bundleSHA256','completionSHA256','approvalSHA256'}
 require(isinstance(replay,dict) and set(replay)==fields and replay['purpose']=='ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION','REPLAY_PAYLOAD_INVALID')
 require(signatures==[replay['signature']] and all(re.fullmatch('[a-f0-9]{64}',replay[k]) for k in ('signature','incidentSHA256','bundleSHA256','completionSHA256','approvalSHA256')),'REPLAY_HASH_BINDING_INVALID')
 require(re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',replay['workloadImage']) and (payload.get('schema') in ('opsloop-exhibition-permit/v2','opsloop-exhibition-permit/v3') or replay['completionSHA256']==payload['previousCompletionSHA256']),'REPLAY_IMAGE_OR_COMPLETION_INVALID')
 s=payload['grant']['scope']
 require(all(isinstance(replay[k],str) and 0<len(replay[k])<=253 for k in ('priorRunID','priorEpoch','priorIncidentUID')) and replay['priorRunID']!=s['runID'] and replay['priorEpoch']!=s['epoch'],'REPLAY_PRIOR_IDENTITY_INVALID')


def replay_intent(ref):
 """Host-only operator intent, fixed before deployment; never inferred from history."""
 a=pinned(ref['path'],ref['sha256'])
 fields={'schema','approved','purpose','scope','authorizationID','ordinal','baseIncidentSequence','maxIncidents','maxActions','evidence','workloadImage','signature','previousCompletionSHA256','notBefore','notAfter','controller'}
 v2=a.get('schema') in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3')
 if v2:fields=(fields-{'evidence','previousCompletionSHA256'})|{'circuitEvidence','predecessorCompletion'}
 if a.get('schema')=='opsloop-circuit-replay-intent/v3':fields.add('approvedReproduction')
 require(set(a)==fields and a['schema'] in ('opsloop-circuit-replay-intent/v1','opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3') and a['approved'] is True and a['purpose']=='ONE_APPROVED_EXHIBITION_CIRCUIT_EXCEPTION','EXPLICIT_REPLAY_APPROVAL_REQUIRED')
 require(a['ordinal']==4 and a['maxIncidents']==a['maxActions']==1,'REPLAY_ONE_B2_ONLY')
 require(instant(a['notBefore'])<=time.time()<instant(a['notAfter']) and 0<instant(a['notAfter'])-instant(a['notBefore'])<=21600,'REPLAY_INTENT_EXPIRED')
 c=a['controller']
 require(set(c)=={'deploymentUID','imageID','priorInstance','priorPodUID'} and all(c.values()),'REPLAY_CONTROLLER_BINDING_REQUIRED')
 require(re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',c['imageID']) and re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}',a['workloadImage']),'REPLAY_IMMUTABLE_IMAGE_REQUIRED')
 proof=evidence(a['circuitEvidence'] if v2 else a['evidence'],a['scope'])
 if v2:verified_predecessor(a['predecessorCompletion'],a['scope'])
 if a['schema']=='opsloop-circuit-replay-intent/v3':
  validate_reproduction(a['approvedReproduction'],a['scope'],a['workloadImage'])
  inc=pinned(a['circuitEvidence']['incident']['path'],a['circuitEvidence']['incident']['sha256'])
  require(all(inc['spec']['trigger'].get(k)==v for k,v in a['approvedReproduction']['classification'].items()),'REPRODUCTION_SOURCE_CLASSIFICATION_CHANGED')
 require(proof['signature']==a['signature'] and (v2 or proof['completionSHA256']==a['previousCompletionSHA256']) and a['workloadImage'].endswith('@'+proof['imageDigest']),'REPLAY_INTENT_EVIDENCE_CHANGED')
 require(a['scope']['runID']!=proof['priorRunID'] and a['scope']['epoch']!=proof['priorEpoch'],'REPLAY_OLD_RUN')
 return a


def pretransition_replay(plan,key,objects,prior,policy_hash):
 """No writes. All decidable replay conditions before Controller transition."""
 ref=plan['executionInputs'].get('circuitReplayApproval',{}).get(key)
 require(ref is not None,'EXPLICIT_REPLAY_APPROVAL_REQUIRED')
 a=replay_intent(ref);t=plan['fixedTargets'][key];p=objects[key+'-policy'];d=objects[key+'-deployment'];s=a['scope'];c=a['controller']
 expected=expected_history(plan,key,p)
 require(a['signature'] in expected,'REPLAY_SIGNATURE_NOT_IN_EXPECTED_HISTORY')
 fields=('namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')
 require(all(s[k]==t[k] for k in fields) and s['clusterUID']==plan['clusterUID'] and d['metadata']['uid']==t['deploymentUID'],'REPLAY_TARGET_CHANGED')
 require(s['policyGeneration']==p['metadata']['generation']+2 and s['policyHash']==policy_hash and a['baseIncidentSequence']==p['status'].get('incidentSequence',0),'REPLAY_POLICY_STATE_CHANGED')
 require(s['incident']==t['policy']+'-'+t['policyUID'][:8]+'-'+str(a['baseIncidentSequence']+1).zfill(5),'REPLAY_INCIDENT_CHANGED')
 require(len(d['spec']['template']['spec']['containers'])==1 and not d['spec']['template']['spec'].get('initContainers') and d['spec']['template']['spec']['containers'][0]['image']==a['workloadImage'],'REPLAY_IMAGE_CHANGED')
 if a['schema']=='opsloop-circuit-replay-intent/v3':require(d['spec']['template']==a['approvedReproduction']['baselineTemplate'],'REPRODUCTION_BASELINE_TEMPLATE_CHANGED')
 require(c['deploymentUID']==plan['deployment']['controller']['uid'] and c['priorInstance']==prior['instance'] and c['priorPodUID']==prior['podUID'] and c['imageID'].endswith('@'+plan['deployment']['imagePublication']['digest']),'REPLAY_CONTROLLER_CHANGED')
 if a['schema'] in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3'):
  require(a['predecessorCompletion']==plan['newInterval']['continuation']['predecessorCompletion'],'REPLAY_PREDECESSOR_CHANGED')
 else:require(a['previousCompletionSHA256']==plan['newInterval']['continuation']['completion']['sha256'],'REPLAY_PREDECESSOR_CHANGED')
 unconsumed_intent(ref,plan['executionInputs']['integration']['issuanceState'])
 return a


def bind_intent(ref,payload,image,receipt):
 a=replay_intent(ref);g=payload['grant'];s=g['scope'];c=a['controller']
 v2=a['schema'] in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3')
 require(payload['schema']==a['schema'].replace('opsloop-circuit-replay-intent/','opsloop-exhibition-permit/'),'REPLAY_CONTRACT_VERSION_CHANGED')
 fields=('authorizationID','ordinal','baseIncidentSequence','maxIncidents','maxActions')
 require(s==a['scope'] and all(payload[k]==a[k] for k in fields) and payload['previousCompletionSHA256']==(a['predecessorCompletion']['sha256'] if v2 else a['previousCompletionSHA256']),'REPLAY_APPROVAL_SCOPE_CHANGED')
 require(instant(a['notBefore'])<=instant(g['notBefore'])<instant(g['notAfter'])<=instant(a['notAfter']) and instant(g['notAfter'])-instant(g['notBefore'])<=600,'REPLAY_GRANT_WINDOW_CHANGED')
 require(image==a['workloadImage'],'REPLAY_IMAGE_CHANGED')
 budget=payload.get('modelBudget',{});require(budget.get('model')=='gpt-6-luna' and budget.get('reasoningEffort')=='medium' and budget.get('authMode')=='chatgpt' and budget.get('maxModelCalls')==1 and budget.get('totalSeconds')==900 and instant(budget['expiresAt'])<=instant(a['notAfter']),'REPLAY_MODEL_BUDGET_CHANGED')
 require(isinstance(receipt,dict) and receipt.get('state')=='STATIC_CONTROLLER_READY','REPLAY_CONTROLLER_RECEIPT_REQUIRED')
 require(all(receipt.get(k)==v for k,v in c.items()) and receipt['instance']==receipt['expectedInstance']==g['instance'] and receipt['instance']!=c['priorInstance'] and receipt['podUID']!=c['priorPodUID'],'REPLAY_CONTROLLER_RECEIPT_CHANGED')
 proof=evidence(a['circuitEvidence'] if v2 else a['evidence'],s);proof.pop('imageDigest')
 return {'purpose':a['purpose'],'workloadImage':image,**proof,'approvalSHA256':ref['sha256']}


def unconsumed_intent(ref,ledger_path):
 """Read-only admission check; issuer's existing atomic transaction remains decisive."""
 import sqlite3
 from pathlib import Path
 a=replay_intent(ref);s=a['scope'];path=Path(ledger_path)/'issuance.sqlite3'
 v2=a['schema'] in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3')
 predecessor=verified_predecessor(a['predecessorCompletion'],s) if v2 else None
 completion=a['predecessorCompletion']['sha256'] if v2 else a['previousCompletionSHA256']
 require(path.is_file(),'REPLAY_ISSUANCE_LEDGER_REQUIRED')
 db=sqlite3.connect('file:'+str(path.resolve())+'?mode=ro',uri=True)
 try:
  require(db.execute('SELECT 1 FROM permits WHERE authorization=? OR run=? OR epoch=?',(a['authorizationID'],s['runID'],s['epoch'])).fetchone() is None,'REPLAY_INTENT_ALREADY_CONSUMED')
  require(db.execute('SELECT 1 FROM completion_chain WHERE completion=?',(completion,)).fetchone() is None,'REPLAY_PREDECESSOR_ALREADY_CONSUMED')
  if db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='circuit_replays'").fetchone():
   require(db.execute('SELECT 1 FROM circuit_replays WHERE approvalSHA256=?',(ref['sha256'],)).fetchone() is None,'REPLAY_INTENT_ALREADY_CONSUMED')
  previous=db.execute('SELECT run,epoch,state FROM permits ORDER BY rowid DESC LIMIT 1').fetchone();proof=evidence(a['circuitEvidence'] if v2 else a['evidence'],s)
  expected=(predecessor['runID'],predecessor['epoch'],'ISSUED') if v2 else (proof['priorRunID'],proof['priorEpoch'],'ISSUED')
  require(previous==expected,'REPLAY_PREDECESSOR_NOT_LATEST')
 finally:db.close()


def projection_hash(value):
 return hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':'),ensure_ascii=True,allow_nan=False).encode()).hexdigest()

def verified_predecessor(ref,target):
 """Safety completion, not experiment success. ISSUED is immutable history;
 archived RETIRED and matching binding prove that its authority has ended."""
 require(isinstance(ref,dict) and set(ref)=={'path','sha256'},'PREDECESSOR_REF_INVALID')
 done=verify_completion(ref['path'],ref['sha256'])
 require(done.get('schema')=='opsloop-exhibition-completion/v1','PREDECESSOR_RUN_COMPLETION_REQUIRED')
 binding=done['binding'];fields=('policyUID','deploymentUID')
 require(all(binding.get(k)==target.get(k) for k in fields),'PREDECESSOR_TARGET_CHANGED')
 require(binding.get('runID')!=target.get('runID') and binding.get('epoch')!=target.get('epoch'),'PREDECESSOR_IS_CURRENT_RUN')
 # The archive hashes the pre-removal annotation custody. The terminal marker
 # is a separate immutable file beside completion, never an ISSUED rewrite.
 marker=Path(ref['path']).parent/'authority-retirement.json'
 require(marker.is_file() and not marker.is_symlink(),'PREDECESSOR_RETIREMENT_MISSING_OR_SYMLINK')
 retirement_sha=hashlib.sha256(marker.read_bytes()).hexdigest()
 retired=pinned(str(marker),retirement_sha)
 require(retired.get('state')=='RETIRED' and retired.get('binding')==binding,'PREDECESSOR_AUTHORITY_NOT_RETIRED')
 return {**{k:binding[k] for k in ('runID','epoch','policyUID','deploymentUID')},'completionSHA256':ref['sha256'],'retirementSHA256':retirement_sha}

def v2_roles(ref,payload,image):
 a=replay_intent(ref);require(a['schema'] in ('opsloop-circuit-replay-intent/v2','opsloop-circuit-replay-intent/v3'),'REPLAY_V2_INTENT_REQUIRED')
 scope=payload['grant']['scope'];proof=evidence(a['circuitEvidence'],scope);proof.pop('imageDigest')
 circuit={**proof,'workloadImage':image,'policyUID':scope['policyUID'],'deploymentUID':scope['deploymentUID']}
 predecessor=verified_predecessor(a['predecessorCompletion'],scope)
 authorization={'authorizationID':payload['authorizationID'],'approvalSHA256':ref['sha256'],'circuitEvidenceSHA256':projection_hash(circuit),'predecessorCompletionSHA256':projection_hash(predecessor)}
 result={'circuitEvidence':circuit,'predecessorCompletion':predecessor,'newExecutionAuthorization':authorization}
 if a['schema']=='opsloop-circuit-replay-intent/v3':
  result['approvedReproduction']=a['approvedReproduction']
  authorization['approvedReproductionSHA256']=projection_hash(a['approvedReproduction'])
 return result

def validate_v2_roles(payload):
 fields={'circuitEvidence','predecessorCompletion','newExecutionAuthorization'}
 v3=payload['schema']=='opsloop-exhibition-permit/v3'
 if v3:fields.add('approvedReproduction')
 else:require('approvedReproduction' not in payload,'REPRODUCTION_CONTRACT_VERSION_REQUIRED')
 if payload['schema']=='opsloop-exhibition-permit/v1':
  require(not fields.intersection(payload),'PERMIT_V1_ROLES_FORBIDDEN');return
 require(payload['schema'] in ('opsloop-exhibition-permit/v2','opsloop-exhibition-permit/v3') and fields<=set(payload),'PERMIT_V2_ROLES_REQUIRED')
 c=payload['circuitEvidence'];p=payload['predecessorCompletion'];a=payload['newExecutionAuthorization'];r=payload.get('circuitReplay');s=payload['grant']['scope']
 require(isinstance(r,dict),'PERMIT_V2_REPLAY_REQUIRED')
 require(isinstance(c,dict) and set(c)=={'signature','priorRunID','priorEpoch','priorIncidentUID','incidentSHA256','bundleSHA256','completionSHA256','workloadImage','policyUID','deploymentUID'},'CIRCUIT_EVIDENCE_SHAPE')
 require(isinstance(p,dict) and set(p)=={'runID','epoch','policyUID','deploymentUID','completionSHA256','retirementSHA256'},'PREDECESSOR_SHAPE')
 require(isinstance(a,dict) and set(a)==({'authorizationID','approvalSHA256','circuitEvidenceSHA256','predecessorCompletionSHA256'}|({'approvedReproductionSHA256'} if v3 else set())),'NEW_AUTHORIZATION_SHAPE')
 require(all(c[k]==r[k] for k in c if k not in ('policyUID','deploymentUID')),'CIRCUIT_EVIDENCE_PROJECTION_CHANGED')
 require(all(c[k]==p[k]==s[k] for k in ('policyUID','deploymentUID')),'ROLE_TARGET_CHANGED')
 require(all(isinstance(p[k],str) and 0<len(p[k])<=253 for k in ('runID','epoch')) and p['runID']!=s['runID'] and p['epoch']!=s['epoch'],'PREDECESSOR_IDENTITY_INVALID')
 require(all(isinstance(p[k],str) and re.fullmatch('[a-f0-9]{64}',p[k]) for k in ('completionSHA256','retirementSHA256')) and p['completionSHA256']==payload['previousCompletionSHA256'],'PREDECESSOR_HASH_CHANGED')
 expected={'authorizationID':payload['authorizationID'],'approvalSHA256':r['approvalSHA256'],'circuitEvidenceSHA256':projection_hash(c),'predecessorCompletionSHA256':projection_hash(p)}
 if v3:
  validate_reproduction(payload['approvedReproduction'],s,c['workloadImage'])
  expected['approvedReproductionSHA256']=projection_hash(payload['approvedReproduction'])
 require(a==expected,'NEW_AUTHORIZATION_BINDING_CHANGED')


def validate_reproduction(value,scope,image):
 """Signed exact reproduction recipe, never a signature normalization rule."""
 fields={'schema','kind','policyUID','deploymentUID','workloadImage','containerName','input','classification','baselineTemplate','template'}
 require(isinstance(value,dict) and set(value)==fields and value['schema']=='opsloop-approved-reproduction/v1' and value['kind']=='DEMO_B_EMPTY_ORDER_READINESS','REPRODUCTION_SCOPE_INVALID')
 require(value['policyUID']==scope['policyUID'] and value['deploymentUID']==scope['deploymentUID'] and value['workloadImage']==image,'REPRODUCTION_TARGET_CHANGED')
 require(value['classification']=={'class':'APPLICATION_FAILURE','reason':'POD_NOT_READY','ruleVersion':'classify-readiness-v1'} and value['input']=={'name':'DEMO_B_REPLAY','value':'empty-order-after-30s'},'REPRODUCTION_CLASS_OR_INPUT_CHANGED')
 import copy
 baseline=value['baselineTemplate'];fault=value['template']
 require(isinstance(baseline,dict) and isinstance(fault,dict),'REPRODUCTION_TEMPLATE_REQUIRED')
 containers=baseline.get('spec',{}).get('containers',[])
 require(len(containers)==1 and not baseline['spec'].get('initContainers') and containers[0]['name']==value['containerName'] and containers[0]['image']==image and containers[0].get('readinessProbe') is not None,'REPRODUCTION_CONTAINER_CHANGED')
 expected=copy.deepcopy(baseline);inputs=[x for x in expected['spec']['containers'][0].get('env',[]) if x.get('name')=='DEMO_B_REPLAY']
 require(len(inputs)==1 and set(inputs[0])<= {'name','value'} and inputs[0].get('value','')=='','REPRODUCTION_BASELINE_INPUT_CHANGED')
 inputs[0]['value']='empty-order-after-30s'
 require(expected==fault,'REPRODUCTION_UNAPPROVED_TEMPLATE_CHANGE')
