"""Common real/saved preflight: accepts GET only; never dispatches mutations."""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path
import subprocess
from tools.exhibition_hashes import (NORMALIZATION, VERSION, RAW, describe, validate, verify,
                                    load_capture, private_read, require, identity, digest)

RESOURCES = {('apps/v1','Deployment'):('/apis/apps/v1','deployments'),
             ('opsloop.io/v1alpha1','RecoveryPolicy'):('/apis/opsloop.io/v1alpha1','recoverypolicies'),
             ('opsloop.io/v1alpha1','RecoveryIncident'):('/apis/opsloop.io/v1alpha1','recoveryincidents')}


def object_path(obj):
    prefix, resource = RESOURCES[(obj['apiVersion'], obj['kind'])]
    return prefix+'/namespaces/'+obj['namespace']+'/'+resource+'/'+obj['name']


def contract_pins(plan):
    contract = plan.get('objectHashContract')
    require(isinstance(contract,dict) and contract.get('schema') == VERSION and contract.get('normalization') == NORMALIZATION, 'PREFLIGHT_HASH_CONTRACT_REQUIRED')
    pins = contract.get('objects')
    expected_keys = {'a-deployment','b-deployment','a-policy','b-policy','a-incident','b-incident','controller-deployment'}
    require(isinstance(pins,dict) and set(pins) == expected_keys, 'PREFLIGHT_RAW_INVENTORY_REQUIRED')
    for key,pin in pins.items(): validate(pin, fields='/' if key.endswith('incident') else '/spec')
    for k,t in plan['fixedTargets'].items():
        for suffix,kind,name,uid,alias in [('deployment','Deployment',t['deployment'],t['deploymentUID'],'observedDeploymentSpec'),('policy','RecoveryPolicy',t['policy'],t['policyUID'],'policySpec')]:
            pin=pins[k+'-'+suffix]
            require(pin['object'] == {'apiVersion':'apps/v1' if kind=='Deployment' else 'opsloop.io/v1alpha1','kind':kind,'namespace':t['namespace'],'name':name,'uid':uid}, 'PREFLIGHT_TARGET_CHANGED')
            require(t.get(alias+'Hash') == pin and t.get(alias+'SHA256') == pin['sha256'], 'PREFLIGHT_HASH_ALIAS_CHANGED')
        baseline=validate(t.get('baselineSpecHash'),fields='/spec')
        require(baseline['object']==pins[k+'-deployment']['object'] and t.get('readyBaselineSpecSHA256')==baseline['sha256'], 'PREFLIGHT_BASELINE_DOMAIN_CHANGED')
        incs=[x for x in plan['migration']['legacyObjectsAllowed'] if x['namespace']==t['namespace']]
        require(len(incs)==1,'PREFLIGHT_INCIDENT_INVENTORY_CHANGED');inc=incs[0];pin=pins[k+'-incident']
        require(inc.get('hash')==pin and inc.get('sha256')==pin['sha256'] and pin['object']=={'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryIncident','namespace':t['namespace'],'name':inc['name'],'uid':inc['uid']}, 'PREFLIGHT_CUSTODY_DELETE_MISMATCH')
    c=plan['deployment']['controller'];pin=pins['controller-deployment']
    require(c.get('expectedSpecHash')==pin and c['expectedSpecSHA256']==pin['sha256'] and pin['object']=={'apiVersion':'apps/v1','kind':'Deployment','namespace':c['namespace'],'name':c['name'],'uid':c['uid']}, 'PREFLIGHT_CONTROLLER_CHANGED')
    return pins


def verify_sources(plan):
    manifest=private_read(plan['candidate']['path'],plan['candidate']['sha256'])
    root=Path(plan['objectHashContract']['repository'])
    for name,sha in manifest['files'].items():
        path=Path(name)
        require(not path.is_absolute() and '..' not in path.parts,'PREFLIGHT_SOURCE_PATH')
        require(hashlib.sha256((root/path).read_bytes()).hexdigest()==sha,'PREFLIGHT_SOURCE_CHANGED')
    verification=private_read(plan['verification']['path'],plan['verification']['sha256'])
    require(verification['candidateManifest']==plan['candidate'],'PREFLIGHT_VERIFICATION_CANDIDATE_CHANGED')
    image_ref=verification['image']
    raw=Path(image_ref['path']).read_bytes()
    require(hashlib.sha256(raw).hexdigest()==image_ref['sha256'],'PREFLIGHT_IMAGE_RECEIPT_CHANGED')
    image=json.loads(raw)
    require(image['imageIndex']['digest']==plan['deployment']['imagePublication']['digest'],'PREFLIGHT_IMAGE_CHANGED')
    for name,sha in image['sourceFiles'].items():
        require(hashlib.sha256((root/name).read_bytes()).hexdigest()==sha,'PREFLIGHT_CONTROLLER_SOURCE_CHANGED')


def preflight(plan, get):
    """Same entrypoint for local replay and future real GET transport."""
    pins=contract_pins(plan);objects={}
    # Hash every target before even considering any executor stage.
    for key,pin in pins.items():
        obj=get(object_path(pin['object']))
        if key=='a-incident' and 'continuation' in plan:
            from tools.exhibition_deletion import archived_incident
            obj=archived_incident(plan,'a',obj)
        verify(obj,pin,fields=pin['fields']);objects[key]=obj
    for k,t in plan['fixedTargets'].items():
        dep=objects[k+'-deployment'];pol=objects[k+'-policy'];inc=objects[k+'-incident']
        require(dep['metadata']['generation']==t['observedDeploymentGeneration'],'PREFLIGHT_DEPLOYMENT_GENERATION_CHANGED')
        require(pol['metadata']['generation']==t['observedPolicyGeneration'] and pol.get('status',{}).get('incidentSequence',0)==t['observedIncidentSequence'],'PREFLIGHT_POLICY_SEQUENCE_CHANGED')
        require(not pol['spec']['enabled'] and not pol.get('status',{}).get('activeIncident') and any(c.get('type')=='Quiesced' and c.get('status')=='True' and c.get('reason')=='FenceClosed' and c.get('observedGeneration')==pol['metadata']['generation'] for c in pol.get('status',{}).get('conditions',[])),'PREFLIGHT_POLICY_STOP_UNCONFIRMED')
        # Independently reconstruct the sole permitted baseline transform from raw input.
        import copy
        baseline=copy.deepcopy(dep);expected_ops=[]
        if k=='b':
            env=baseline['spec']['template']['spec']['containers'][0]['env']
            require(len(env)>5 and env[5].get('name')=='DEMO_B_REPLAY' and env[5].get('value')=='empty-order-after-30s','PREFLIGHT_BASELINE_TRANSFORM_CHANGED')
            if t.get('baselineDerivation',{}).get('responseContract')=='kubernetes-env-empty-omitted/v1':
                del env[5]['value']
                expected_ops=[{'op':'remove','path':'/spec/template/spec/containers/0/env/5/value'}]
            else:
                env[5]['value']=''
                expected_ops=[{'op':'replace','path':'/spec/template/spec/containers/0/env/5/value','value':''}]
        derivation=t.get('baselineDerivation',{})
        require(derivation.get('source')==pins[k+'-deployment'] and derivation.get('patch')==expected_ops and derivation.get('patchSHA256')==digest(expected_ops),'PREFLIGHT_BASELINE_DERIVATION_CHANGED')
        verify(baseline,t['baselineSpecHash'],fields='/spec')
        desc=next(x for x in plan['migration']['legacyObjectsAllowed'] if x['namespace']==t['namespace'])
        require(inc['metadata']['resourceVersion']==desc['resourceVersion'] and inc.get('status',{}).get('runtime',{}).get('phase')==desc['phase'] and desc['phase'] in ('RECOVERED','FAILED'),'PREFLIGHT_INCIDENT_TERMINATION_CHANGED')
        require(inc['spec']['policyRef']['uid']==t['policyUID'] and inc['spec']['workload']['uid']==t['deploymentUID'],'PREFLIGHT_INCIDENT_OWNER_CHANGED')
        require(inc['metadata'].get('annotations',{}).get('opsloop.io/execution-grant-binding')==desc['grantBinding'],'PREFLIGHT_GRANT_CHANGED')
    return {'state':'OBJECT_HASH_PREFLIGHT_PASS','objectsVerified':len(objects),'mutationRequests':0,
            'historicalCustodyChecks':['a-incident'] if 'continuation' in plan else [],
            'scope':'object integrity gate only; full execution preflight still needs quota/RBAC/nodes/registry under fresh approval'}


def saved_reader(capture):
    values={object_path(identity(o.obj)):o.obj for o in capture.values()}
    return lambda path: values[path]


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--plan',required=True);p.add_argument('--sha256',required=True)
    p.add_argument('--full',action='store_true')
    g=p.add_mutually_exclusive_group(required=True);g.add_argument('--capture');g.add_argument('--live-read-until');g.add_argument('--responses')
    p.add_argument('--responses-sha256')
    p.add_argument('--capture-sha256');a=p.parse_args()
    plan=private_read(a.plan,a.sha256)
    if not a.full: verify_sources(plan)
    if a.responses:
        require(a.full,'FULL_PREFLIGHT_REQUIRED')
        from tools.exhibition_readiness import saved_responses
        reader=saved_responses(a.responses,a.responses_sha256)
    elif a.capture:
        reader=saved_reader(load_capture(a.capture,a.capture_sha256))
    else:
        end=dt.datetime.fromisoformat(a.live_read_until.replace('Z','+00:00'))
        now=dt.datetime.now(dt.timezone.utc);require(now<end<=now+dt.timedelta(minutes=15),'PREFLIGHT_READ_WINDOW_INVALID')
        # Read-only preparation has its own explicitly bounded approval window.
        # A local draft has no execution deadline and cannot authorize mutations.
        if not a.full:
            require(now<dt.datetime.fromisoformat(plan['approvalValidUntil']),'PREFLIGHT_REVISION_EXPIRED')
        def reader(path):
            from tools.exhibition_readiness import MissingObservation
            if not path.startswith(('/api/v1/','/apis/')): raise MissingObservation()
            require('/secrets' not in path and '/exec' not in path,'PREFLIGHT_PATH_FORBIDDEN')
            require(dt.datetime.now(dt.timezone.utc)<end,'PREFLIGHT_READ_WINDOW_EXPIRED')
            argv=['/home/sleepyminy/.local/bin/kubectl-opsloop','--kubeconfig','/home/sleepyminy/.kube/opsloop.yaml','--context','default','--request-timeout=5s','get','--raw',path]
            result=subprocess.run(argv,capture_output=True,timeout=8)
            require(result.returncode==0,'PREFLIGHT_GET_FAILED')
            from tools.exhibition_hashes import parse
            return result.stdout.decode('utf-8') if '/log?' in path else parse(result.stdout)
    if a.full:
        from tools.exhibition_readiness import full_preflight
        result=full_preflight(plan,reader,mode='live' if a.live_read_until else 'saved')
    else: result=preflight(plan,reader)
    print(json.dumps(result))
    if result.get('state')=='NO_GO': raise SystemExit(2)

if __name__=='__main__':main()
