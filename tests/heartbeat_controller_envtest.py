"""Real local TLS heartbeat with deterministic Go Controller barrier.

Disposable envtest credentials only. Native grant encoder, Guard and server clock
are deterministic stand-ins; transport, progress reads and Controller are real.
The same run-private directory is preserved across every source transition.
"""
import copy,json,sys,time,urllib.request
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from tools import exhibition_demo_b_trial as core
config,kubectl,input_file=sys.argv[1:]
assert '127.0.0.1' in Path(config).read_text()
v=json.loads(Path(input_file).read_text());s=v['scope'];grant=v['grant'];root=Path(v['sharedRoot'])/'run';root.mkdir(mode=0o700,exist_ok=True)
import yaml
origin=yaml.safe_load(Path(config).read_text())['clusters'][0]['cluster']['server'].rstrip('/')
core.NS=s['namespace'];core.APP=s['deployment'];core.POLICY_NAME=s['policy'];core.CORE='/api/v1/namespaces/'+s['namespace'];core.POLICY='/apis/opsloop.io/v1alpha1/namespaces/'+s['namespace']+'/recoverypolicies/'+s['policy'];core.DEPLOY='/apis/apps/v1/namespaces/'+s['namespace']+'/deployments/'+s['deployment']
p={'runID':s['runID'],'epoch':s['epoch'],'apiOrigin':origin,'executionScope':s,'enforcePolicySpec':v['policySpec'],'sourceDeploymentSpec':v['sourceDeploymentSpec'],'initialIncidentSequence':v['payload']['baseIncidentSequence'],'expiresAt':grant['notAfter'],'exhibitionPermit':{'local':'fixture'},'initialPolicyGeneration':s['policyGeneration']-2}
c=core.Client(SimpleNamespace(CLIENT=kubectl,CONFIG=config),'local',{'requests':[]},lambda:None)
old=c.get(core.POLICY);wire={'annotation':old['metadata']['annotations'][core.GRANT],'binding':v['binding']};grant=json.loads(wire['annotation']);calls=[]
def barrier(path):
 with urllib.request.urlopen(v['barrier']+path,timeout=10) as response:response.read()
def revalidate():
 try:barrier('/authority')
 except Exception as exc:raise core.Stop('local Controller authority rejected') from exc
original=c.heartbeat_patch
def send(p,payload,directory,before_send):
 def before():
  before_send()
  if not calls:barrier('/advance')
  calls.append(copy.deepcopy(payload))
 return original(p,payload,directory,before)
c.heartbeat_patch=send
# Tick starts when production renew obtains its original clock after initial proof reads.
def clock(p):
 tick=time.clock_gettime_ns(time.CLOCK_BOOTTIME)
 return {'apiServerDate':v['observedAt'],'beforeTick':tick,'afterTick':tick}
with patch.object(core,'ROOT',root),patch.object(core,'clock',side_effect=clock),patch.object(core,'native',side_effect=lambda p,key,payload:{'annotation':json.dumps(payload),'binding':v['binding']}),patch.object(core,'exhibition_permit',return_value=(v['envelope'],v['payload'])):
 error=None
 try:result=core.renew_heartbeat(c,p,s['policyGeneration'],v['envelope'],wire,grant,lambda receipt:None,revalidate=revalidate);success=True
 except core.Stop as exc:success=False;error=str(exc)
 if success!=v['expectedSuccess']:raise AssertionError((v['label'],success,len(calls),error))
 assert 1<=len(calls)<=2,(v['label'],calls)
 if not success:assert len(calls)==1,(v['label'],'unexpected second PATCH')
 if success:
  if v['label']=='same-incident-progress':
   from tools.engineering_evidence.seal import policy_snapshots_equal
   inc=c.get('/apis/opsloop.io/v1alpha1/namespaces/'+s['namespace']+'/recoveryincidents/'+s['incident'])
   live=c.get(core.POLICY)
   assert inc['spec']['policy']!=live['spec'], 'fixture did not cross Go duration serialization'
   assert policy_snapshots_equal(inc['spec']['policy'],live['spec'])
   assert len(inc['status']['runtime']['attempts'])==1
  assert core.instant(result[1]['heartbeatAt'])==core.instant(v['observedAt']) and result[1]['notAfter']==grant['notAfter']
  if v['label'] in ('identity-bound-not-ready','readiness-false-to-true'):assert len(calls)==2,(v['label'],'Controller transition did not collide')
  if len(calls)==2:assert calls[0][1]['value']!=calls[1][1]['value'] and calls[0][-1]==calls[1][-1]
 print(json.dumps({'stage':v['label'],'success':success,'PATCHes':len(calls),'error':error,'scope':'local Controller and real TLS; synthetic kubelet/Guard/clock'}))
