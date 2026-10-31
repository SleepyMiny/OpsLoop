"""Pinned B-only work window and cumulative host ledger checks.
No grant, signature, Guard, cluster permission or per-round budget changes.
"""
import datetime as dt,json,sqlite3,time
from pathlib import Path
from tools.exhibition_hashes import private_read,require
from tools.exhibition_lifecycle import private_dir,write,verify_completion

def work_auth(ref, *, deadline=None, now=None):
 a=private_read(ref['path'],ref['sha256']);now=time.time() if now is None else now
 start=dt.datetime.fromisoformat(a['receivedAt']).timestamp();end=dt.datetime.fromisoformat(a['workDeadline']).timestamp()
 replay=a.get('schema')=='opsloop-b2-circuit-work-approval/v1'
 require((replay and a.get('allowedRounds')==['B2'] or a.get('schema')=='opsloop-b-work-approval/v1' and a.get('allowedRounds')==['B1','B2']),'B_WORK_SCOPE_INVALID')
 if replay:
  if a.get('candidatePublicationsMaximum')==0:
   require(a.get('imageRepublishAllowed') is False and a.get('newReviewedImagePublicationAllowed') is False,'B2_PUBLICATION_NOT_AUTHORIZED')
  require(a.get('modelCallsConsumed')==1 and a.get('candidatePublicationsMaximum') in (0,1) and a.get('circuitSignature')=='bb2a8692d8404ac8ac67ce0f5baec2cfadd0f65106542bf0c5b17823267fd337','B2_CIRCUIT_SCOPE_CHANGED')
  review=private_read(a['independentReview']['path'],a['independentReview']['sha256']);require(review.get('verdict')=='APPROVE' and review.get('confirmedBlockers')==0,'B2_INDEPENDENT_REVIEW_REQUIRED')
  require(a.get('controllerRepository')=='opsloopp1b43a5cde9729.azurecr.io/opsloop/demo-a/controller' and a.get('restoreDigest')=='sha256:82e91893d67ff64a868485d971134aeda8aed8fa1979e609926ac45e75e7eaed','B2_IMAGE_SCOPE_CHANGED')
 require(a.get('candidateTransitionsMaximum')==(1 if replay else 2) and a.get('modelCallsPerRound')==1 and a.get('modelCallsTotalMaximum')==2 and a.get('runtimeEngineeringSecondsPerRound')==900 and a.get('AAdditionalInjections')==0 and a.get('finalReserveSeconds')==1200,'B_WORK_LIMIT_CHANGED')
 require(start<=now<end<=start+21600 and (deadline is None or now<deadline<=end),'B_WORK_WINDOW_EXPIRED')
 return a

def previous_completed(i,c):
 if c.get('completedRounds')==['B1']:return completed_b1(i,c)
 for ref in c.get('preFaultAttempts',[]):
  prior=private_read(ref['path'],ref['sha256'])
  require(not prior.get('faultIntent') and prior.get('stopReceipt',{}).get('state',{}).get('phase')=='STOP_CONFIRMED','B_PRIOR_FAULT_OR_STOP_UNCONFIRMED')
 receipt=verify_completion(c['completion']['path'],c['completion']['sha256'])
 require(receipt['binding']['runID']==c['previousRunID'],'B_PREDECESSOR_CHANGED')
 db=sqlite3.connect('file:'+str(Path(i['controlState'])/'ledger.sqlite3')+'?mode=ro',uri=True)
 try:
  runs=[json.loads(x[0]) for x in db.execute('SELECT value FROM runs ORDER BY rowid')]
  require([r['runId'] for r in runs]==c['priorRunIDs'] and all(r.get('lifecycleComplete') is True for r in runs),'B_PRIOR_LEDGER_CHANGED')
  prior_b={r['runId'] for r in runs if r.get('track')=='engineering'}
  checked_b={private_read(ref['path'],ref['sha256']).get('binding',{}).get('runID') for ref in c.get('preFaultAttempts',[])}
  require(prior_b==checked_b,'B_PRIOR_ATTEMPT_CUSTODY_MISSING')
  events=[json.loads(x[0]) for x in db.execute('SELECT value FROM events')]
  require(sum(e['kind']=='FAULT_DISPATCHED' for e in events)==2 and not any(e['actor']=='model' for e in events),'B_INITIAL_BUDGET_CHANGED')
 finally:db.close()
 db=sqlite3.connect('file:'+str(Path(i['issuanceState'])/'issuance.sqlite3')+'?mode=ro',uri=True)
 try:require(db.execute('SELECT run,state FROM permits ORDER BY rowid').fetchall()==[(r,'ISSUED') for r in c['priorRunIDs']],'B_PRIOR_ISSUANCE_CHANGED')
 finally:db.close()
 from tools.exhibition_execution import host_idle
 host_idle(i)

def reserve_transition(cfg,root,approval_sha,digest):
 a=work_auth(cfg['authorization']);require(digest==a['candidateDigest'],'B_CANDIDATE_CHANGED')
 if a.get('schema')=='opsloop-b2-circuit-work-approval/v1':
  require(cfg.get('allowedRounds')==['B2'] and cfg['transitionLedger']==a['transitionLedger'],'B2_TRANSITION_LEDGER_CHANGED')
  ledger=private_dir(a['transitionLedger'])
  write(ledger/'transition-1.json',json.dumps({'digest':digest,'sessionRoot':str(root),'planSHA256':approval_sha,'authorization':cfg['authorization'],'reservedAt':dt.datetime.now(dt.timezone.utc).isoformat(),'dispatchCountMaximum':1}).encode(),exclusive=True)
  return 1
 ledger=private_dir(cfg['transitionLedger'])
 if cfg.get('supplement'):
  b2_supplement(cfg)
  for n in (1,2):
   old=json.loads((ledger/f'transition-{n}.json').read_bytes())
   require(old['digest']==digest,'B_PREVIOUS_IMAGE_CHANGED')
   prior=Path(old['sessionRoot'])
   require(json.loads((prior/'result.json').read_bytes()).get('rollbackRollout')=='CONFIRMED' and (prior/'controller-restore-readback.json').is_file(),'B_PREVIOUS_TRANSITION_UNRESOLVED')
  write(ledger/'transition-3.json',json.dumps({'digest':digest,'sessionRoot':str(root),'planSHA256':approval_sha,'supplement':cfg['supplement'],'reservedAt':dt.datetime.now(dt.timezone.utc).isoformat(),'dispatchCountMaximum':1}).encode(),exclusive=True)
  return 3
 for n in (1,2):
  path=ledger/f'transition-{n}.json'
  if path.exists():
   old=json.loads(path.read_bytes());require(old['digest']==digest,'B_PREVIOUS_IMAGE_CHANGED')
   if n==1:
    result=Path(old['sessionRoot'])/'result.json';receipt=Path(old['sessionRoot'])/'controller-restore-readback.json'
    require(result.is_file() and receipt.is_file() and json.loads(result.read_bytes()).get('rollbackRollout')=='CONFIRMED','B_PREVIOUS_TRANSITION_UNRESOLVED')
   continue
  write(path,json.dumps({'digest':digest,'sessionRoot':str(root),'planSHA256':approval_sha,'reservedAt':dt.datetime.now(dt.timezone.utc).isoformat(),'dispatchCountMaximum':1}).encode(),exclusive=True)
  return n
 raise RuntimeError('B_TRANSITION_BUDGET_EXHAUSTED')


def b2_supplement(cfg):
 a=work_auth(cfg['authorization']);ref=cfg['supplement'];v=private_read(ref['path'],ref['sha256'])
 require(v.get('schema')=='opsloop-b2-supplement/v1' and v.get('parentAuthorization')==cfg['authorization'] and v.get('allowedRounds')==cfg.get('allowedRounds')==['B2'],'B2_SUPPLEMENT_SCOPE_CHANGED')
 require(v.get('workDeadline')==a['workDeadline'] and dt.datetime.fromisoformat(v['receivedAt']).timestamp()<=time.time(),'B2_SUPPLEMENT_TIME_CHANGED')
 require(v.get('candidateDigest')==a['candidateDigest'] and v.get('additionalTransitions')==1 and v.get('priorTransitions')==2 and v.get('modelCallsConsumed')==1 and v.get('additionalModelCalls')==1 and v.get('cumulativeModelMaximum')==2 and v.get('runtimeEngineeringSeconds')==900 and v.get('AAdditionalInjections')==0,'B2_SUPPLEMENT_LIMIT_CHANGED')
 return v

def completed_b1(i,c):
 receipt=verify_completion(c['completion']['path'],c['completion']['sha256'])
 runid=c['previousRunID'];require(receipt['binding']['runID']==runid,'B2_PREDECESSOR_CHANGED')
 runtime=private_read(c['completedRuntime']['path'],c['completedRuntime']['sha256'])
 model=private_read(c['completedModel']['path'],c['completedModel']['sha256'])
 require(runtime.get('binding')==receipt['binding'] and runtime.get('faultIntent') and runtime.get('state')=='ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED' and runtime.get('stopReceipt',{}).get('state',{}).get('phase')=='STOP_CONFIRMED','B1_RUNTIME_UNCONFIRMED')
 require(model.get('state')=='VERIFIED_PENDING_REVIEW' and model.get('incident',{}).get('run_id')==runid and model['incident'].get('epoch')==receipt['binding']['epoch'] and model.get('broker',{}).get('dispatched_requests')==1 and model.get('comparison',{}).get('same_input_confirmed') is True,'B1_MODEL_BUDGET_UNCONFIRMED')
 checked={runid}
 for ref in c['preFaultAttempts']:
  v=private_read(ref['path'],ref['sha256']);require(not v.get('faultIntent') and v.get('stopReceipt',{}).get('state',{}).get('phase')=='STOP_CONFIRMED','B_PRIOR_FAULT_OR_STOP_UNCONFIRMED');checked.add(v['binding']['runID'])
 # Failed trials remain consumed trials. Only their verified safe lifecycle can
 # become a new explicitly approved execution's immediate predecessor.
 failed=c.get('failedCompletedAttempts',[])
 if failed:
  # Historical custody is bounded by the exact, immutable prior-run ledger below,
  # not by the number of earlier implementation revisions. This issues no budget.
  require(1<=len(failed)<len(c['priorRunIDs']) and 'predecessorCompletion' in c,'B2_REVALIDATION_SCOPE_CHANGED')
  from tools.exhibition_replay import verified_predecessor
  failed_ids=[]
  for item in failed:
   latest=verified_predecessor(item['completion'],receipt['binding'])
   prior=private_read(item['runtimeReceipt']['path'],item['runtimeReceipt']['sha256'])
   retired=private_read(item['retirement']['path'],item['retirement']['sha256'])
   expected={k:latest[k] for k in ('runID','epoch','policyUID','deploymentUID')}
   require(prior.get('binding')==expected and prior.get('faultIntent') and prior.get('state')!='ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED','B2_FAILED_TRIAL_CUSTODY_CHANGED')
   stop=prior.get('stopReceipt',{})
   require(stop.get('runID')==expected['runID'] and stop.get('state',{}).get('phase')=='STOP_CONFIRMED','B2_FAILED_TRIAL_STOP_UNCONFIRMED')
   require(retired.get('binding')==expected and retired.get('state')=='RETIRED','B2_FAILED_TRIAL_AUTHORITY_UNCONFIRMED')
   require(expected['runID'] not in checked,'B2_FAILED_TRIAL_DUPLICATE')
   checked.add(expected['runID']);failed_ids.append(expected['runID'])
  require(failed[-1]['completion']==c['predecessorCompletion'],'B2_PREDECESSOR_REFERENCE_CHANGED')
  require(c['priorRunIDs'][-len(failed_ids):]==failed_ids,'B2_FAILED_TRIAL_NOT_LATEST')
 db=sqlite3.connect('file:'+str(Path(i['controlState'])/'ledger.sqlite3')+'?mode=ro',uri=True)
 try:
  runs=[json.loads(x[0]) for x in db.execute('SELECT value FROM runs ORDER BY rowid')]
  require([r['runId'] for r in runs]==c['priorRunIDs'] and all(r.get('lifecycleComplete') is True for r in runs),'B2_PRIOR_LEDGER_CHANGED')
  require({r['runId'] for r in runs if r.get('track')=='engineering'}==checked,'B2_ATTEMPT_CUSTODY_MISSING')
 finally:db.close()
 db=sqlite3.connect('file:'+str(Path(i['engineering']['state'])/'ledger.sqlite3')+'?mode=ro',uri=True)
 try:
  jobs=[(st,json.loads(raw)) for st,raw in db.execute('SELECT state,detail FROM jobs')];newjobs=[(st,v) for st,v in jobs if v.get('incident',{}).get('run_id') in checked]
  require(len(newjobs)==1 and newjobs[0][0]=='VERIFIED_PENDING_REVIEW' and newjobs[0][1]==model,'B2_MODEL_LEDGER_CHANGED')
 finally:db.close()
 db=sqlite3.connect('file:'+str(Path(i['issuanceState'])/'issuance.sqlite3')+'?mode=ro',uri=True)
 try:require(db.execute('SELECT run,state FROM permits ORDER BY rowid').fetchall()==[(r,'ISSUED') for r in c['priorRunIDs']],'B2_ISSUANCE_CHANGED')
 finally:db.close()
 from tools.exhibition_reconcile import model_jobs_stopped
 from tools.exhibition_execution import host_idle
 model_jobs_stopped(i['engineering']['state'],receipt['binding']);host_idle(i)
