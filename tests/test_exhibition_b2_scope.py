import copy,datetime as dt,json,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
from tools.exhibition_inputs import new
from tools.exhibition_b_work import b2_supplement,reserve_transition
from tools.exhibition_resume import rounds_for
from tools.exhibition_hashes import ControlError
class B2Scope(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name);now=dt.datetime.now(dt.timezone.utc);self.auth={'schema':'opsloop-b-work-approval/v1','receivedAt':now.isoformat(),'workDeadline':(now+dt.timedelta(hours=6)).isoformat(),'allowedRounds':['B1','B2'],'candidateTransitionsMaximum':2,'modelCallsPerRound':1,'modelCallsTotalMaximum':2,'runtimeEngineeringSecondsPerRound':900,'AAdditionalInjections':0,'finalReserveSeconds':1200,'candidateDigest':'sha256:fixed'};ref=new(self.root,'auth.json',self.auth)
  self.v={'schema':'opsloop-b2-supplement/v1','receivedAt':now.isoformat(),'parentAuthorization':ref,'workDeadline':self.auth['workDeadline'],'allowedRounds':['B2'],'additionalTransitions':1,'priorTransitions':2,'candidateDigest':'sha256:fixed','modelCallsConsumed':1,'additionalModelCalls':1,'cumulativeModelMaximum':2,'runtimeEngineeringSeconds':900,'AAdditionalInjections':0}
  self.cfg={'authorization':ref,'allowedRounds':['B2'],'transitionLedger':str(self.root/'transitions')};self.number=0
 def tearDown(self):self.tmp.cleanup()
 def seal(self):self.number+=1;self.cfg['supplement']=new(self.root,f'supp-{self.number}.json',self.v)
 def test_only_b2(self):
  self.seal();self.assertEqual(rounds_for(self.cfg,self.auth),[(4,'B2')])
 def test_budget_cannot_reset(self):
  for key,value in [('additionalTransitions',2),('modelCallsConsumed',0),('additionalModelCalls',2),('runtimeEngineeringSeconds',901),('AAdditionalInjections',1)]:
   old=self.v[key];self.v[key]=value;self.seal()
   with self.assertRaises(ControlError):b2_supplement(self.cfg)
   self.v[key]=old
 def test_no_time_extension(self):
  self.v['workDeadline']=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(hours=7)).isoformat();self.seal()
  with self.assertRaises(ControlError):b2_supplement(self.cfg)
 def test_third_transition_once(self):
  self.seal();ledger=self.root/'transitions';ledger.mkdir(mode=0o700)
  for n in (1,2):
   d=self.root/f'prior{n}';d.mkdir();new(d,'result.json',{'rollbackRollout':'CONFIRMED'});new(d,'controller-restore-readback.json',{'state':'ORIGINAL_SPEC_READY'});new(ledger,f'transition-{n}.json',{'digest':'sha256:fixed','sessionRoot':str(d)})
  self.assertEqual(reserve_transition(self.cfg,self.root/'new','a'*64,'sha256:fixed'),3)
  with self.assertRaises(FileExistsError):reserve_transition(self.cfg,self.root/'again','b'*64,'sha256:fixed')
 def test_missing_prior_not_adopted(self):
  self.seal()
  with self.assertRaises(FileNotFoundError):reserve_transition(self.cfg,self.root/'new','a'*64,'sha256:fixed')
 def test_other_digest(self):
  self.seal()
  with self.assertRaises(ControlError):reserve_transition(self.cfg,self.root/'new','a'*64,'sha256:other')
