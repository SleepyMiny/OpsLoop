import base64,copy,json,time,unittest
from tools.exhibition_pull_recovery import claims,scope,replacement,secret_auth,Stop
REPO='opsloop/demo-a/controller';KEY='registry.example/'+REPO
def token(sub='same',actions=None,repo=REPO,expiry=None):
 c={'iss':'same-issuer','sub':sub,'exp':expiry or time.time()+7200,'access':[{'type':'repository','name':repo,'actions':actions or ['pull']}]}
 return 'eyJhbGciOiJub25lIn0.'+base64.urlsafe_b64encode(json.dumps(c).encode()).decode().rstrip('=')+'.synthetic'
class PullTests(unittest.TestCase):
 def setUp(self):
  self.username='00000000-0000-0000-0000-000000000000'
  self.cfg={'auths':{KEY:{'auth':base64.b64encode((self.username+':'+token(expiry=1)).encode()).decode()},'other':{'auth':'preserved'}}}
  self.secret={'metadata':{'uid':'exact-uid','resourceVersion':'7','labels':{'keep':'yes'}},'type':'kubernetes.io/dockerconfigjson','data':{'.dockerconfigjson':base64.b64encode(json.dumps(self.cfg).encode()).decode(),'other':'unchanged'}}
 def test_uid_rv_cas_changes_only_credential_field(self):
  original=copy.deepcopy(self.secret);out,patch=replacement(self.secret,KEY,self.username,token())
  self.assertEqual(self.secret,original);self.assertEqual(out['metadata'],original['metadata']);self.assertEqual(out['data']['other'],'unchanged')
  cfg,_,_=secret_auth(out,KEY);self.assertEqual(cfg['auths']['other'],self.cfg['auths']['other'])
  self.assertEqual([(x['op'],x['path']) for x in patch],[('test','/metadata/uid'),('test','/metadata/resourceVersion'),('test','/type'),('test','/data/.dockerconfigjson'),('replace','/data/.dockerconfigjson')])
 def test_identity_or_repository_or_push_scope_changes_rejected(self):
  for value in (token(sub='different'),token(repo='other'),token(actions=['pull','push'])):
   with self.subTest(value_kind='synthetic'),self.assertRaises(Stop):replacement(self.secret,KEY,self.username,value)
 def test_insufficient_lifetime_rejected(self):
  with self.assertRaisesRegex(Stop,'LIFETIME'):replacement(self.secret,KEY,self.username,token(expiry=time.time()+20))
 def test_unsupported_credential_type_rejected(self):
  self.secret['type']='Opaque'
  with self.assertRaises(Stop):secret_auth(self.secret,KEY)
