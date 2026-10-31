import copy,tempfile,unittest
from pathlib import Path
from tools.exhibition_execution import Reader
class RolloutObservationTests(unittest.TestCase):
 def test_pending_logs_are_not_requested_but_running_logs_are(self):
  for running in (False,True):
   with self.subTest(running=running),tempfile.TemporaryDirectory() as d:
    r=object.__new__(Reader);r.root=Path(d);r.plan={'deployment':{'controller':{'uid':'dep','namespace':'system'}}}
    r.allowed={'/pods':{'key':'controller-pods'},'/rs':{'key':'controller-replicasets'}}
    pod={'metadata':{'uid':'pod','name':'controller','ownerReferences':[{'uid':'rs','controller':True}]},'status':{'containerStatuses':[{'name':'manager','state':{'running':{'startedAt':'now'}} if running else {'waiting':{'reason':'ErrImagePull'}}}]}}
    data={'/pods':{'items':[pod]},'/rs':{'items':[{'metadata':{'uid':'rs','ownerReferences':[{'uid':'dep','controller':True}]}}]}}
    calls=[]
    def get(path,**kw):
     calls.append(path)
     if path not in data:
      self.assertTrue(running);return 'actual-startup-log'
     return copy.deepcopy(data[path])
    r.get=get;out=r.snapshot()
    self.assertEqual(any('/log?' in p for p in calls),running)
    self.assertEqual(out['controller-pods']['items'][0],pod)
    self.assertEqual('controller-log-pod' in out,running)
