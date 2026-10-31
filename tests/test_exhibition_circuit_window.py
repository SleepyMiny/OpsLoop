import copy,datetime as dt,tempfile,unittest
from pathlib import Path
from tools.exhibition_inputs import new
from tools.exhibition_b_work import work_auth,reserve_transition
from tools.exhibition_resume import rounds_for,session_deadline
from tools.exhibition_hashes import ControlError
class CircuitWindow(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name);self.now=dt.datetime.now(dt.timezone.utc)
  self.a={'schema':'opsloop-b2-circuit-work-approval/v1','receivedAt':self.now.isoformat(),'workDeadline':(self.now+dt.timedelta(hours=6)).isoformat(),'allowedRounds':['B2'],'candidateTransitionsMaximum':1,'candidatePublicationsMaximum':1,'modelCallsConsumed':1,'modelCallsPerRound':1,'modelCallsTotalMaximum':2,'runtimeEngineeringSecondsPerRound':900,'AAdditionalInjections':0,'finalReserveSeconds':1200,'candidateDigest':'sha256:'+'a'*64,'restoreDigest':'sha256:82e91893d67ff64a868485d971134aeda8aed8fa1979e609926ac45e75e7eaed','controllerRepository':'opsloopp1b43a5cde9729.azurecr.io/opsloop/demo-a/controller','circuitSignature':'bb2a8692d8404ac8ac67ce0f5baec2cfadd0f65106542bf0c5b17823267fd337','independentReview':new(self.root,'review.json',{'verdict':'APPROVE','confirmedBlockers':0}),'transitionLedger':str(self.root/'transitions')};self.n=0
 def seal(self):
  self.n+=1;return {'authorization':new(self.root,'auth-'+str(self.n)+'.json',self.a),'allowedRounds':['B2'],'transitionLedger':self.a['transitionLedger']}
 def test_new_window_only_b2_without_reviving_old_window(self):
  cfg=self.seal();self.assertEqual(work_auth(cfg['authorization']),self.a);self.assertEqual(rounds_for(cfg,self.a),[(4,'B2')]);self.assertEqual(session_deadline(self.now,self.a,{'priorSession':'must-not-read-old-window'}).isoformat(),self.a['workDeadline'])
 def test_limits_and_exact_signature_and_repository(self):
  original=copy.deepcopy(self.a)
  for key,bad in [('allowedRounds',['B1','B2']),('candidateTransitionsMaximum',2),('candidatePublicationsMaximum',2),('modelCallsConsumed',0),('modelCallsPerRound',2),('modelCallsTotalMaximum',3),('AAdditionalInjections',1),('runtimeEngineeringSecondsPerRound',901),('circuitSignature','b'*64),('controllerRepository','other'),('restoreDigest','other'),('workDeadline',(self.now+dt.timedelta(hours=7)).isoformat())]:
   self.a={**copy.deepcopy(original),key:bad};cfg=self.seal()
   with self.subTest(key=key),self.assertRaises(ControlError):work_auth(cfg['authorization'])
 def test_single_transition_and_no_ledger_substitution(self):
  cfg=self.seal();self.assertEqual(reserve_transition(cfg,self.root/'run','a'*64,self.a['candidateDigest']),1)
  with self.assertRaises(FileExistsError):reserve_transition(cfg,self.root/'again','b'*64,self.a['candidateDigest'])
  cfg['transitionLedger']=str(self.root/'other')
  with self.assertRaises(ControlError):reserve_transition(cfg,self.root/'again','b'*64,self.a['candidateDigest'])
 def test_review_required(self):
  self.a['independentReview']=new(self.root,'reject.json',{'verdict':'REQUEST_CHANGES','confirmedBlockers':1});cfg=self.seal()
  with self.assertRaises(ControlError):work_auth(cfg['authorization'])
