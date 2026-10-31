"""Controller terminal -> Python stop/capture/seal -> blocked-model intent.

Only disposable envtest TLS credentials. Kubelet, probe/log, Guard IPC, pinned
build provenance and outer approval are explicit test fixtures. Runtime terminal,
CAS transport, identity/custody, capture, sealing, adapter budget/binding and final
Controller stop/fence use production paths. No real model is called.
"""
import copy,datetime as dt,json,sys,threading,time,urllib.request
from contextlib import ExitStack
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from tools import exhibition_demo_b_trial as core
from tools.exhibition_terminal import TerminalObserved
from tools.exhibition_handoff import collect,write_new
from tools.exhibition_control import ControlError
from tools.engineering_evidence import seal
from tools.exhibition_engineering import EngineeringAdapter
from tests.test_exhibition_engineering import EngineeringAdapterTests
config,kubectl,input_file=sys.argv[1:]
assert '127.0.0.1' in Path(config).read_text()
v=json.loads(Path(input_file).read_text());s=v['scope'];f=v['terminalFixture'];root=Path(v['sharedRoot'])/'run'
import yaml
origin=yaml.safe_load(Path(config).read_text())['clusters'][0]['cluster']['server'].rstrip('/')
core.NS=s['namespace'];core.APP=s['deployment'];core.POLICY_NAME=s['policy'];core.CORE='/api/v1/namespaces/'+s['namespace'];core.POLICY='/apis/opsloop.io/v1alpha1/namespaces/'+s['namespace']+'/recoverypolicies/'+s['policy'];core.DEPLOY='/apis/apps/v1/namespaces/'+s['namespace']+'/deployments/'+s['deployment']
p={'runID':s['runID'],'epoch':s['epoch'],'clusterUID':s['clusterUID'],'apiOrigin':origin,'executionScope':s,'enforcePolicySpec':v['policySpec'],'sourceDeploymentSpec':v['sourceDeploymentSpec'],'initialIncidentSequence':v['payload']['baseIncidentSequence'],'expiresAt':v['grant']['notAfter'],'exhibitionPermit':{'local':'fixture'},'initialPolicyGeneration':s['policyGeneration']-2}
c=core.Client(SimpleNamespace(CLIENT=kubectl,CONFIG=config),'local',{'requests':[]},lambda:None)
old=c.get(core.POLICY);wire={'annotation':old['metadata']['annotations'][core.GRANT],'binding':v['binding']};grant=json.loads(wire['annotation']);calls=[];stops=[]
fixed=core.instant(v['observedAt']);started=core.instant(f['startedAt'])
def barrier(path):
 with urllib.request.urlopen(v['barrier']+path,timeout=10) as response:response.read()
def clock(p):
 tick=time.clock_gettime_ns(time.CLOCK_BOOTTIME)
 return {'apiServerDate':v['observedAt'],'beforeTick':tick,'afterTick':tick}
original=c.heartbeat_patch
def send(p,payload,directory,before_send):
 calls.append(copy.deepcopy(payload))
 result=original(p,payload,directory,before_send)
 # Successful response exists before Controller consumes the one Incident.
 barrier('/advance')
 return result
c.heartbeat_patch=send
with ExitStack() as stack:
 for name,value in [('ROOT',root),('now',lambda:fixed),('clock',clock),('exhibition_permit',lambda *a,**kw:(v['envelope'],v['payload']))]:stack.enter_context(patch.object(core,name,value))
 stack.enter_context(patch.object(core,'native',side_effect=lambda p,key,payload:{'annotation':json.dumps(payload),'binding':v['binding']}))
 try:core.renew_heartbeat(c,p,s['policyGeneration'],v['envelope'],wire,grant,lambda receipt:None,revalidate=lambda:barrier('/authority'))
 except TerminalObserved as exc:terminal=exc.proof
 else:raise AssertionError('terminal became authority success')
 assert len(calls)==1,'terminal sent extra heartbeat'
 assert terminal['incident']['status']['runtime']['reason']=='RUNTIME_BUDGET_EXHAUSTED'

# Existing adapter fixture supplies only local executables/harness/state paths.
fixture=EngineeringAdapterTests();fixture.setUp()
try:
 plan=copy.deepcopy(fixture.public_plan);plan['maxModelCalls']=1
 plan['binding']={k:s[k] for k in ('runID','epoch','policyUID','deploymentUID')}
 plan['expiresAt']=p['expiresAt'];plan['runtimeCapture']={'fixture':'local-only'}
 image=v['payload']['approvedReproduction']['workloadImage'].split('@')[1]
 provenance={'schema_version':'opsloop-image-source-provenance/v1','evidence_kind':'observed-build','repository':plan['engineering']['repository'],'base_commit':plan['engineering']['base_commit'],'image_digest':image,'platform_image_digests':[image],'source_files':{path:'2'*64 for path in [*seal.ALLOWED_PATHS,'apps/engineering-demo/main.go']}}
 (root/'verification').mkdir();write_new(root/'verification/image-provenance.json',provenance)
 cfg={'root':str(root),'plan':str(root/'fixture-plan.json'),'planSHA256':'a'*64,'provenance':str(root/'verification/image-provenance.json'),'provenanceSHA256':seal.digest((root/'verification/image-provenance.json').read_bytes())}
 setup={'authorityController':{'instance':v['grant']['instance']}}
 def observation(pod,phase,when):return {'phase':phase,'pod_uid':pod['metadata']['uid'],'pod_name':pod['metadata']['name'],'namespace':s['namespace'],'method':'POST','path':'/quote','request_sha256':seal.FIXTURE_SHA256,'response_sha256':'f'*64,'transport':'kubernetes-pod-proxy','evidence_kind':'observed','http_status':500,'error_code':seal.ERROR,'observed_at':when}
 before={'deployment':f['beforeDeployment'],'pod':f['beforePod'],'probe':observation(f['beforePod'],'before_runtime',f['beforeTime']),'log':{'pod_uid':f['beforePod']['metadata']['uid'],'text':seal.ERROR}}
 stop_receipt={'runID':s['runID'],'state':{'phase':'STOP_CONFIRMED','binding':'local-guard-binding'}}
 def native(p,key,args,**kw):
  if key=='guard':
   if args[0]=='finish-workload':stops.append(1);barrier('/stop');return {'state':'STOP_RECONCILING'}
   assert args[:2]==['guard-workload','status'];return stop_receipt
  assert key=='quoteProbe'
  pod=c.get(core.CORE+'/pods/'+args[0]);return observation(pod,'after_runtime',fixed.isoformat())
 def actual_capture(client,runtime,setup,record,save):
  core.POLICY='/apis/opsloop.io/v1alpha1/namespaces/'+s['namespace']+'/recoverypolicies/'+s['policy']
  raw=root/'incident-raw';raw.mkdir(mode=0o700)
  for name,value in [('deployment-before.json',before['deployment']),('pod-before.json',before['pod'])]:write_new(raw/name,value)
  record['guardBinding']='local-guard-binding'
  stop=lambda:core.finish_guard(p,[],SimpleNamespace(poll=lambda:None),record,save)
  try:core.capture_terminal(c,p,setup,record,save,terminal,raw,before,stop=stop,started=started,read_deadline=fixed+dt.timedelta(seconds=45))
  except Exception:
   import traceback;traceback.print_exc();raise
  record['state']='ACTUAL_RUNTIME_EXHAUSTED_STRONG_STOP_CONFIRMED';save()
 def preflight(plan):
  # Outer signed-approval provisioning was separately exercised; local fixture
  # cannot issue operational authority. Retain exclusive handoff ownership.
  if (root/'handoff-start.json').exists():raise ControlError('CAPTURE_ALREADY_CONSUMED')
  if plan['maxModelCalls']!=1:raise ControlError('CAPTURE_AUTHORITY_MISMATCH')
  return cfg,root,p,setup
 actual_build=seal.build_bundle
 def build(*a,**kw):return actual_build(*a,now=fixed,**kw)
 original_request=c.request
 def synthetic_log(ns,pod,container):return seal.ERROR
 model=[];events=[]
 def blocked_model(args,**kw):
  assert args.bound_bundle['incident']['run_id']==s['runID']
  assert args.bound_bundle['runtime']['outcome']=='EXHAUSTED'
  model.append(args.bound_bundle['incident']['run_id']);return {'state':'ENGINEERING_FAILED'}
 def diagnosed_collect(*args):
  try:return collect(*args)
  except ControlError as exc:
   if str(exc)!='CAPTURE_ALREADY_CONSUMED':
    import traceback;traceback.print_exc()
   raise
  except Exception:
   import traceback;traceback.print_exc();raise
 with ExitStack() as st:
  st.enter_context(patch('tools.exhibition_handoff.collect',side_effect=diagnosed_collect))
  for name,value in [('ROOT',root),('now',lambda:fixed),('stamp',lambda:fixed.isoformat()),('native',native),('exhibition_permit',lambda *a,**kw:(v['envelope'],v['payload'])),('bind_controller',lambda *a,**kw:barrier('/authority')),('load',lambda *a:(p,SimpleNamespace(CLIENT=kubectl,CONFIG=config),'local')),('trial',actual_capture)]:st.enter_context(patch.object(core,name,value))
  st.enter_context(patch.object(c,'log',synthetic_log))
  st.enter_context(patch('tools.exhibition_handoff.preflight',side_effect=preflight))
  st.enter_context(patch('tools.exhibition_handoff.importlib.util.spec_from_file_location',return_value=SimpleNamespace(loader=SimpleNamespace(exec_module=lambda module:None))))
  st.enter_context(patch('tools.exhibition_handoff.importlib.util.module_from_spec',return_value=core))
  st.enter_context(patch('tools.exhibition_handoff.time.time',return_value=started.timestamp()))
  st.enter_context(patch('tools.exhibition_handoff.build_bundle',side_effect=build))
  st.enter_context(patch('tools.engineering_evidence.seal.verify_source',return_value=provenance['source_files']))
  st.enter_context(patch('tools.exhibition_engineering.run_engineering',side_effect=blocked_model))
  outcome=EngineeringAdapter().run(plan,lambda *event:events.append(event),threading.Event())
  assert len(model)==1,(outcome,events,(root/'runtime-error.json').read_text() if (root/'runtime-error.json').exists() else '')
  assert outcome=={'state':'FAILED','stopConfirmed':True},outcome # Model deliberately blocked; never claims real success.
  second=EngineeringAdapter().run(plan,lambda *event:None,threading.Event())
  assert len(model)==1 and second['state']=='UNCONFIRMED_TERMINATION'
 assert len(stops)==1 and len(calls)==1
 seal.verify_sealed(root/'sealed',seal.digest((root/'sealed/manifest.json').read_bytes()))
 assert any(event[1]=='EVIDENCE_SEALED' for event in events)
 barrier('/cleanup')
 assert c.get(core.POLICY)['spec']['enabled'] is False
 assert c.get('/apis/opsloop.io/v1alpha1/namespaces/'+s['namespace']+'/recoveryincidents/'+s['incident'],absent=True) is None
 print(json.dumps({'stage':'Controller action-terminal-stop-capture-seal-adapter-cleanup','heartbeatPATCH':len(calls),'afterTerminalPATCH':0,'stopRequests':len(stops),'modelIntent':len(model),'realModelRequests':0,'secondEntry':'rejected','scope':'envtest API; synthetic kubelet/probes/Guard IPC/approval/build provenance; model transport blocked'}))
finally:
 # Only this disposable envtest seal, after integrity verification. Production
 # archives are never chmod/deleted by this test. Allow Go TempDir cleanup.
 if (root/'sealed').is_dir():(root/'sealed').chmod(0o700)
 fixture.doCleanups()
