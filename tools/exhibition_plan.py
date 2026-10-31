"""Generate a separate correction revision only from pinned private raw GET captures."""
from __future__ import annotations
import argparse
import copy
import datetime as dt
import hashlib
import json
from pathlib import Path
from tools.exhibition_hashes import (RAW, VERSION, NORMALIZATION, load_capture, private_read,
                                    raw_pin, describe, digest, require, identity, project)
from tools.exhibition_preflight import contract_pins, preflight, saved_reader, object_path


def generate(previous, capture, *, previous_ref, capture_ref, candidate_ref, repository, prepared_at, valid_until, correction_basis, basis_ref, verification_ref=None, candidate_tree=None):
    required={'a-deployment','b-deployment','a-policy','b-policy','a-incident','b-incident','controller-deployment'}
    require(set(capture)==required,'PLAN_REQUIRED_RAW_MISSING')
    for source in capture.values():
        require(source.representation==RAW and source.path==object_path(identity(source.obj)),'PLAN_RAW_PROVENANCE_REQUIRED')
    require(correction_basis.get('decision') == 'NO_GO_STOPPED_BEFORE_MUTATION' and correction_basis.get('approvalSHA256') == previous_ref['sha256'], 'PLAN_CORRECTION_BASIS_REQUIRED')
    findings={f['target']:f for f in correction_basis['findings']}
    require(set(findings)=={'a','b'},'PLAN_CORRECTION_BASIS_INCOMPLETE')
    for k in ('a','b'):
        require(raw_pin(capture[k+'-deployment'],'/spec')['sha256']==findings[k]['deployment']['actualRawSpecSHA256']
                and raw_pin(capture[k+'-incident'],'/')['sha256']==findings[k]['incident']['actualFullObjectSHA256'], 'PLAN_RAW_CHANGED_SINCE_NO_GO')
    plan=copy.deepcopy(previous)
    plan['revision']='hash-contract-r1';plan['previousRevision']=previous_ref
    plan['correctionBasis']=basis_ref
    if verification_ref is not None:
        plan['previousVerification']=previous['verification'];plan['verification']=verification_ref
    plan['reviewScope']='Prior independent review applies to unchanged base candidate. Hash-contract correction is covered by focused local connection/mutation tests, not a repeated full independent review.'
    plan['previousCandidate']=previous['candidate'];plan['candidate']=candidate_ref
    require(isinstance(candidate_tree,str) and len(candidate_tree)==40 and all(c in '0123456789abcdef' for c in candidate_tree),'PLAN_CANDIDATE_TREE_REQUIRED')
    plan['previousCandidateTree']=previous.get('candidateTree');plan['candidateTree']=candidate_tree
    plan['preparedAt']=prepared_at;plan['approvalValidUntil']=valid_until
    require(dt.datetime.fromisoformat(prepared_at)<dt.datetime.fromisoformat(valid_until),'PLAN_WINDOW_INVALID')
    plan['status']='AWAITING_NEW_APPROVAL_NOT_EXECUTABLE'
    plan['correctionReason']='Previous revision treated sanitized views as raw spec/full-object pins; no target substitution or execution budget change.'
    pins={key:raw_pin(obs,'/' if key.endswith('incident') else '/spec') for key,obs in capture.items()}
    plan['objectHashContract']={'schema':VERSION,'normalization':NORMALIZATION,'repository':str(repository),'capture':capture_ref,'objects':pins,
        'preflightEntrypoint':'python3 -m tools.exhibition_preflight','originalMeaning':'Decoded original GET object. No field removal; /spec hashes all env/image/security fields. Full Incident includes metadata and status.',
        'observationMeaning':'New GET capture, not reconstructed original at prior approval time.'}
    plan['displayHashes']={key:describe(project(o.obj),'/',representation='display-projection') for key,o in capture.items()}
    for k,t in plan['fixedTargets'].items():
        dep=capture[k+'-deployment'].obj;pol=capture[k+'-policy'].obj
        # The correction never adopts a new UID, generation, image, sequence or Policy spec.
        require(dep['metadata']['uid']==t['deploymentUID'] and dep['metadata']['generation']==t['observedDeploymentGeneration'],'PLAN_TARGET_DRIFT')
        require(pol['metadata']['uid']==t['policyUID'] and pol['metadata']['generation']==t['observedPolicyGeneration'] and pol.get('status',{}).get('incidentSequence',0)==t['observedIncidentSequence'],'PLAN_POLICY_DRIFT')
        require(dep['spec']['template']['spec']['containers'][0]['image']==t['image'],'PLAN_IMAGE_DRIFT')
        require(pins[k+'-policy']['sha256']==t['policySpecSHA256'],'PLAN_POLICY_SPEC_DRIFT')
        for suffix,name in [('deployment','observedDeploymentSpec'),('policy','policySpec')]:
            t[name+'Hash']=pins[k+'-'+suffix];t[name+'SHA256']=pins[k+'-'+suffix]['sha256']
        baseline=copy.deepcopy(dep);ops=[]
        if k=='b':
            env=baseline['spec']['template']['spec']['containers'][0]['env']
            indices=[i for i,e in enumerate(env) if e.get('name')=='DEMO_B_REPLAY']
            require(indices==[5] and env[5].get('value')=='empty-order-after-30s','PLAN_BASELINE_TRANSFORM_CHANGED')
            env[5]['value']='';ops=[{'op':'replace','path':'/spec/template/spec/containers/0/env/5/value','value':''}]
        t['baselineSpecHash']=describe(baseline,'/spec');t['readyBaselineSpecSHA256']=t['baselineSpecHash']['sha256']
        t['baselineDerivation']={'source':pins[k+'-deployment'],'patch':ops,'patchSHA256':digest(ops),'retainsAllOtherSpecFields':True}
        inc=capture[k+'-incident'].obj;e=next(x for x in plan['migration']['legacyObjectsAllowed'] if x['namespace']==t['namespace'])
        require(all(inc['metadata'].get(f)==e[f] for f in ('name','uid','resourceVersion')) and inc['status']['runtime']['phase']==e['phase'],'PLAN_INCIDENT_DRIFT')
        e['hash']=pins[k+'-incident'];e['sha256']=e['hash']['sha256']
    c=plan['deployment']['controller'];obj=capture['controller-deployment'].obj
    require(obj['metadata']['uid']==c['uid'] and pins['controller-deployment']['sha256']==c['expectedSpecSHA256'],'PLAN_CONTROLLER_DRIFT')
    c['expectedSpecHash']=pins['controller-deployment']
    contract_pins(plan);preflight(plan,saved_reader(capture))
    # Budgets, targets, deployment image, rollback intent and exclusions are immutable.
    for key in ('authority','trials','cleanupAndRollback','excludedEvenOnProposedApproval','timeWindow'):
        require(plan[key]==previous[key],'PLAN_EXECUTION_SCOPE_CHANGED')
    require(plan['deployment']['imagePublication']==previous['deployment']['imagePublication'],'PLAN_IMAGE_CHANGED')
    return plan



def migration_hash_inputs(plan, key):
    """Exact generator output consumed by migration/cleanup config assembly."""
    contract_pins(plan)
    t = plan['fixedTargets'][key]
    return {'baselineSpecHash': copy.deepcopy(t['baselineSpecHash']),
            'baselineSpecSHA256': t['readyBaselineSpecSHA256'],
            'policySpecHash': copy.deepcopy(t['policySpecHash']),
            'policySpecSHA256': t['policySpecSHA256'],
            'objects': [{k: copy.deepcopy(v) for k,v in item.items()
                         if k in ('kind','name','uid','resourceVersion','sha256','hash','grantBinding')}
                        for item in plan['migration']['legacyObjectsAllowed'] if item['namespace']==t['namespace']]}


def readiness_revision(previous, contract, get, *, previous_ref):
    """Local draft: same full preflight as execution, without minting authority.

    No deadline is invented during development. A later approval must seal the
    fresh observation window and execution start separately.
    """
    from tools.exhibition_readiness import readiness_contract, full_preflight
    plan=copy.deepcopy(previous)
    plan['revision']='readiness-r3-local'
    plan['previousRevision']=copy.deepcopy(previous_ref)
    plan['status']='LOCAL_DRAFT_NOT_EXECUTABLE'
    plan['readinessContract']=copy.deepcopy(contract)
    plan['previousApprovalValidUntil']=plan.pop('approvalValidUntil',None)
    plan['timeStructure']={'localDevelopment':'No execution authority',
        'readOnlyPreparation':'Separate bounded GET approval required; never inherits prior expired approval',
        'execution':'Fresh explicit approval after readiness; start within 30 minutes, total <=90 minutes',
        'freshnessCheck':'Exact approved identities, specs, instance, quota and fence only; no new code or safety conditions',
        'grantSeconds':previous['timeWindow']['maxRuntimeGrantSeconds'],
        'cleanupSecondsPerRun':previous['timeWindow']['maxCleanupSecondsPerRun'],
        'completionDeadline':'UNSET_REQUIRES_FEASIBLE_START_AND_EXPLICIT_APPROVAL'}
    readiness_contract(plan)
    report=full_preflight(plan,get,mode='saved')
    for key in ('fixedTargets','migration','deployment','authority','trials','cleanupAndRollback','timeWindow','objectHashContract'):
        require(plan[key]==previous[key],'READINESS_EXECUTION_SCOPE_CHANGED')
    return plan,report


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for flag in ('previous','previous-sha256','capture','capture-sha256','candidate','candidate-sha256','repository','valid-until','output','basis','basis-sha256','verification','verification-sha256'):p.add_argument('--'+flag,required=True)
    a=p.parse_args();previous=private_read(a.previous,a.previous_sha256);capture=load_capture(a.capture,a.capture_sha256)
    plan=generate(previous,capture,previous_ref={'path':a.previous,'sha256':a.previous_sha256},capture_ref={'path':a.capture,'sha256':a.capture_sha256},candidate_ref={'path':a.candidate,'sha256':a.candidate_sha256},repository=a.repository,prepared_at=dt.datetime.now(dt.timezone.utc).isoformat(),valid_until=a.valid_until,correction_basis=private_read(a.basis,a.basis_sha256),basis_ref={'path':a.basis,'sha256':a.basis_sha256},verification_ref={'path':a.verification,'sha256':a.verification_sha256},candidate_tree=private_read(a.candidate,a.candidate_sha256)['candidateTree'])
    from tools.exhibition_lifecycle import write
    output=Path(a.output);write(output,(json.dumps(plan,indent=2,ensure_ascii=False)+'\n').encode(),exclusive=True)
    print(json.dumps({'revision':plan['revision'],'sha256':hashlib.sha256(output.read_bytes()).hexdigest(),'mutationRequests':0}))

if __name__=='__main__':main()
