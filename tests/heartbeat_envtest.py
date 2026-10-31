"""Disposable loopback envtest: real Python TLS heartbeat PATCH, no real grant/key."""
import copy,datetime as dt,io,json,subprocess,sys,tempfile,time,urllib.error
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
from tools import exhibition_demo_b_trial as core
from tools import exhibition_heartbeat as transport

config=Path(sys.argv[1]);raw=config.read_text();assert '127.0.0.1' in raw and '100.73.' not in raw
kubectl=sys.argv[2]
import yaml
origin=yaml.safe_load(raw)['clusters'][0]['cluster']['server'].rstrip('/')
def cli(args,body=None):
 r=subprocess.run([kubectl,'--kubeconfig',str(config),'--context','local',*args],input=None if body is None else json.dumps(body).encode(),capture_output=True,timeout=10)
 assert r.returncode==0,r.stderr.decode();return json.loads(r.stdout)
cli(['create','-f','-','-o','json'],{'apiVersion':'v1','kind':'Namespace','metadata':{'name':'opsloop-demo-b'}})
spec={'enabled':True,'mode':'Enforce','targetRef':{'name':'local-app'},'candidateSelector':{},'verification':{'serviceName':'local-app'}}
obj=cli(['create','-f','-','-o','json'],{'apiVersion':'opsloop.io/v1alpha1','kind':'RecoveryPolicy','metadata':{'name':'engineering-demo','namespace':'opsloop-demo-b'},'spec':spec})
path=core.POLICY

def case(label,conflicts=0,mutation=None,loss=False,error_code=None):
 now=dt.datetime.now(dt.timezone.utc);tick=time.clock_gettime_ns(time.CLOCK_BOOTTIME)
 scope={'policyUID':obj['metadata']['uid'],'runID':'local-'+label,'epoch':'local-'+label}
 grant={'version':'local-only','scope':scope,'instance':'synthetic-instance','notBefore':(now-dt.timedelta(seconds=2)).isoformat(),'notAfter':(now+dt.timedelta(seconds=120)).isoformat(),'heartbeatAt':(now-dt.timedelta(seconds=1)).isoformat(),'heartbeatSequence':1}
 envelope={'synthetic':'never-an-operational-permit'};wire={'annotation':json.dumps(grant),'binding':'synthetic-only'}
 current=cli(['patch','recoverypolicy','engineering-demo','-n','opsloop-demo-b','--type=merge','-p',json.dumps({'spec':{'enabled':True,'mode':'Enforce'},'metadata':{'annotations':{core.GRANT:wire['annotation'],core.EXHIBITION_PERMIT:core.canonical(envelope).decode()}}}),'-o','json'])
 cli(['patch','recoverypolicy','engineering-demo','-n','opsloop-demo-b','--subresource=status','--type=merge','-p',json.dumps({'status':{'incidentSequence':0,'conditions':[{'type':'Monitoring','status':'True','reason':'Active','message':'local','lastTransitionTime':'2026-01-01T00:00:00Z'}]}}),'-o','json'])
 p={'runID':scope['runID'],'epoch':scope['epoch'],'apiOrigin':origin,'executionScope':scope,'enforcePolicySpec':current['spec'],'initialIncidentSequence':0,'expiresAt':grant['notAfter']}
 fixed={k:grant[k] for k in ('version','scope','instance','notBefore','notAfter')};attempts=[];guard_stopped=False
 original_transport=transport.Transport
 class FaultTransport(original_transport):
  def __init__(self,settings):
   super().__init__(settings);real=self.opener.open
   def open_(request,timeout):
    if error_code:
     body=json.dumps({'kind':'Status','status':'Failure','code':error_code,'reason':{401:'Unauthorized',403:'Forbidden',422:'Invalid'}[error_code]}).encode()
     raise urllib.error.HTTPError(request.full_url,error_code,'synthetic response fault',{},io.BytesIO(body))
    response=real(request,timeout=timeout)
    if loss:
     response.close();raise TimeoutError('synthetic lost response AFTER real apply')
    return response
   self.opener.open=open_
 with tempfile.TemporaryDirectory() as root,patch.object(core,'ROOT',Path(root)),patch.object(core,'clock',return_value={'apiServerDate':now.isoformat(),'beforeTick':tick,'afterTick':tick}),patch.object(core,'native',side_effect=lambda p,key,payload:{'annotation':json.dumps(payload),'binding':'synthetic-only'}),patch.object(core,'exhibition_permit',return_value=(envelope,{'grant':fixed})),patch.object(transport,'Transport',FaultTransport):
  record={'requests':[]};c=core.Client(SimpleNamespace(CLIENT=kubectl,CONFIG=str(config)),'local',record,lambda:None)
  original=c.heartbeat_patch
  def guard(receipt):
   if guard_stopped or p.get('_testGuardStopped'):raise core.Stop('local Guard stopped')
  def send(p,payload,directory,before_send):
   def barrier():
    before_send();attempts.append(copy.deepcopy(payload))
    if len(attempts)<=conflicts:
     cli(['patch','recoverypolicy','engineering-demo','-n','opsloop-demo-b','--subresource=status','--type=merge','-p',json.dumps({'status':{'conditions':[{'type':'Monitoring','status':'True','reason':'Active','message':'local','lastTransitionTime':'2026-01-01T00:00:0'+str(len(attempts))+'Z'}]}}),'-o','json'])
   result=original(p,payload,directory,barrier)
   if result.get('httpStatus')==409 and mutation:
    mutation(c,p)
   return result
  c.heartbeat_patch=send
  def revalidate():
   if p.get('_testInstanceChanged'):raise core.Stop('local Controller instance changed')
  try:
   result=core.renew_heartbeat(c,p,current['metadata']['generation'],envelope,wire,grant,guard,revalidate=revalidate);success=True
  except core.Stop:success=False
  observed=c.get(path)
  if label in ('normal','benign'):
   assert success and len(attempts)==(1 if label=='normal' else 2),(label,success,len(attempts))
   assert observed['metadata']['annotations'][core.GRANT]==result[0]['annotation']
   if label=='benign':
    assert attempts[0][1]['value']!=attempts[1][1]['value'] and attempts[0][-1]==attempts[1][-1]
  else:
   assert not success and len(attempts)==(2 if label=='second-conflict' else 1),(label,success,len(attempts))
   if loss:assert observed['metadata']['annotations'][core.GRANT]!=wire['annotation']
  print('PASS local actual transport:',label,'PATCH attempts',len(attempts),'success',success)

case('normal');case('benign',conflicts=1);case('second-conflict',conflicts=2)
def withdraw(c,p):cli(['patch','recoverypolicy','engineering-demo','-n','opsloop-demo-b','--type=merge','-p',json.dumps({'metadata':{'annotations':{core.EXHIBITION_PERMIT:None}}}),'-o','json'])
case('withdrawn-after-conflict',conflicts=1,mutation=withdraw)
def mutate_policy(body):
 return lambda c,p:cli(['patch','recoverypolicy','engineering-demo','-n','opsloop-demo-b','--type=merge','-p',json.dumps(body),'-o','json'])
case('generation-stop-change',conflicts=1,mutation=mutate_policy({'spec':{'enabled':False}}))
case('grant-change',conflicts=1,mutation=mutate_policy({'metadata':{'annotations':{core.GRANT:'changed'}}}))
case('guard-stopped-in-conflict',conflicts=1,mutation=lambda c,p:p.update(_testGuardStopped=True))
case('instance-changed-in-conflict',conflicts=1,mutation=lambda c,p:p.update(_testInstanceChanged=True))
case('deadline-exhausted-in-conflict',conflicts=1,mutation=lambda c,p:p.update(expiresAt=(dt.datetime.now(dt.timezone.utc)-dt.timedelta(seconds=1)).isoformat()))
case('lost-response-after-apply',loss=True)
for status in (401,403,422):case('synthetic-http-'+str(status),error_code=status)
print('All local connection cases use generated envtest credentials; no historical cause assertion.')
