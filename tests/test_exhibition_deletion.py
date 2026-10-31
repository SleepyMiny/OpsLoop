"""Synthetic deletion completion only: no Kubernetes, registry, or model calls."""
import copy
import datetime as dt
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import Mock, patch
from tools.exhibition_control import ControlError, canonical
from tools.exhibition_deletion import wait_deleted
from tools.exhibition_lifecycle import Lifecycle
from tools.exhibition_runtime import Transport, RuntimeRejected
from tests import test_exhibition_lifecycle as lifecycle
from tests import test_exhibition_migration as migration

class Clock:
    def __init__(self): self.now=1000.
    def __call__(self): return self.now
    def sleep(self, seconds): self.now+=seconds

class WaitTests(unittest.TestCase):
    def setUp(self):
        tmp=tempfile.TemporaryDirectory();self.addCleanup(tmp.cleanup);self.root=Path(tmp.name)
        self.identity={'kind':'RecoveryIncident','name':'incident','uid':'uid','resourceVersion':'1'}
        self.path='/apis/opsloop.io/v1alpha1/namespaces/demo/recoveryincidents/incident'
        self.marker=self.root/'dispatch.json';self.marker.write_text(canonical({'state':'DELETE_DISPATCHED','identity':self.identity}));self.marker.chmod(0o600)
        self.original=self.marker.read_bytes();self.clock=Clock();self.api=Mock()
        self.object={'metadata':{'name':'incident','namespace':'demo','uid':'uid','resourceVersion':'2','deletionTimestamp':'pending'}}
    def wait(self, **kw):
        return wait_deleted(self.api,self.path,self.identity,self.marker,dt.datetime.fromtimestamp(1010,dt.timezone.utc).isoformat(),
                            clock=self.clock,monotonic=self.clock,sleep=self.clock.sleep,window_seconds=1,**kw)
    def tearDown(self):
        self.assertEqual(self.marker.read_bytes(),self.original)
        self.api.delete.assert_not_called()
        for call in self.api.request.call_args_list:self.assertEqual(call.args,(self.path,));self.assertEqual(call.kwargs,{'absent':True})
    def test_immediate_absence(self):
        self.api.request.return_value=None;proof=self.wait()
        self.assertEqual(json.loads(Path(proof['path']).read_text())['state'],'DELETED')
        self.assertEqual(self.api.request.call_count,1)
    def test_delayed_same_uid_then_absent(self):
        self.api.request.side_effect=[self.object,None];self.wait();self.assertEqual(self.api.request.call_count,2)
    def test_deadline_stops_and_preserves_unconfirmed(self):
        self.api.request.return_value=self.object
        with self.assertRaisesRegex(ControlError,'DELETE_WAIT_DEADLINE'):self.wait()
        self.assertFalse(self.marker.with_suffix('.confirmed.json').exists())
        receipts=list(self.marker.with_suffix('.wait').glob('*.json'))
        self.assertEqual(json.loads(receipts[0].read_text())['state'],'UNCONFIRMED')
        self.assertEqual(self.api.request.call_count,2)
    def test_other_uid_rejected(self):
        self.object['metadata']['uid']='replacement';self.api.request.return_value=self.object
        with self.assertRaisesRegex(ControlError,'OBJECT_REPLACED'):self.wait()
        self.assertFalse(self.marker.with_suffix('.confirmed.json').exists())
    def test_permissions_and_connection_errors_are_not_absence(self):
        for error in (RuntimeRejected('API_OBSERVATION_UNCONFIRMED'),ConnectionError('synthetic')):
            self.api.request.side_effect=error
            with self.assertRaises(type(error)):self.wait()
            self.assertFalse(self.marker.with_suffix('.confirmed.json').exists())
    def test_process_interruption_then_restart_keeps_failure(self):
        self.api.request.side_effect=KeyboardInterrupt()
        with self.assertRaises(KeyboardInterrupt):self.wait()
        old={p:p.read_bytes() for p in self.marker.with_suffix('.wait').glob('*.json')}
        self.api.request.side_effect=[self.object,None];self.wait()
        self.assertTrue(all(p.read_bytes()==v for p,v in old.items()))
    def test_absence_after_deadline_is_not_accepted(self):
        def late(*a,**kw): self.clock.now=1011;return None
        self.api.request.side_effect=late
        with self.assertRaisesRegex(ControlError,'DELETE_WAIT_DEADLINE'):self.wait()
        self.assertFalse(self.marker.with_suffix('.confirmed.json').exists())

class TransportTests(unittest.TestCase):
    def test_only_404_is_absence(self):
        api=object.__new__(Transport);api.origin='https://synthetic.invalid';api.token=None;api.opener=Mock()
        for code in (401,403,500):
            error=urllib.error.HTTPError(api.origin,code,'synthetic',{},io.BytesIO());self.addCleanup(error.close)
            api.opener.open.side_effect=error
            with self.assertRaises(RuntimeRejected):api.request('/exact',absent=True)
        api.opener.open.side_effect=urllib.error.URLError('synthetic')
        with self.assertRaises(RuntimeRejected):api.request('/exact',absent=True)
        error=urllib.error.HTTPError(api.origin,404,'synthetic',{},io.BytesIO());self.addCleanup(error.close)
        api.opener.open.side_effect=error
        self.assertIsNone(api.request('/exact',absent=True))
        api.opener.open.side_effect=None
        for raw in (b'null',b'[]',b'invalid'):
            api.opener.open.return_value=io.BytesIO(raw)
            with self.assertRaises(RuntimeRejected):api.request('/exact',absent=True)

class ConnectedTests(unittest.TestCase):
    def case(self, kind):
        fixture=(migration.MigrationTests if kind=='migration' else lifecycle.LifecycleTests)()
        fixture.setUp();self.addCleanup(fixture.doCleanups)
        run=fixture.execute if kind=='migration' else fixture.run_cleanup
        return fixture,run
    def test_timeout_blocks_completion_and_restart_cannot_extend(self):
        for kind in ('migration','lifecycle'):
            with self.subTest(kind=kind):
                f,run=self.case(kind);api=f.api
                def delete(path,uid,rv):
                    api.deleted.append(('incident-38',uid,rv))
                    api.objects['incident-38']['metadata'].update(resourceVersion='4',deletionTimestamp='pending')
                api.delete=delete;clock=Clock();clock.now=dt.datetime.now(dt.timezone.utc).timestamp()
                def bounded(*args,**kw):
                    kw['window_seconds']=1
                    return wait_deleted(*args,**kw,clock=clock,monotonic=clock,sleep=clock.sleep)
                with patch('tools.exhibition_'+kind+'.wait_deleted',bounded):
                    with self.assertRaisesRegex(ControlError,'DELETE_WAIT_DEADLINE'):run()
                self.assertEqual(len(api.deleted),1)
                self.assertFalse(list(f.root.rglob('completion.json')))
                self.assertFalse(api.patch_calls)
                failed={p:p.read_bytes() for p in f.root.rglob('*.json') if 'UNCONFIRMED' in p.read_text()}
                self.assertTrue(failed)
                api.objects.clear()
                with patch('tools.exhibition_'+kind+'.wait_deleted',bounded):
                    with self.assertRaisesRegex(ControlError,'DELETE_WAIT_DEADLINE'):run()
                self.assertFalse(list(f.root.rglob('completion.json')))
                self.assertEqual(len(api.deleted),1)
                self.assertTrue(all(p.read_bytes()==raw for p,raw in failed.items()))
    def test_delayed_delete_and_restart_use_both_real_cleanup_paths(self):
        for kind in ('migration','lifecycle'):
            for interrupted in (False,True):
                with self.subTest(kind=kind,interrupted=interrupted):
                    f,run=self.case(kind);api=f.api;request=api.request;pending=[False];calls=[0]
                    def delete(path,uid,rv):
                        self.assertEqual((uid,rv),('incident-uid','3'));api.deleted.append(('incident-38',uid,rv));pending[0]=True
                        api.objects['incident-38']['metadata'].update(resourceVersion='4',deletionTimestamp='pending')
                        if interrupted:raise KeyboardInterrupt()
                    def read(path,patch=None,absent=False):
                        if pending[0] and path.endswith('/incident-38'):
                            calls[0]+=1
                            if calls[0]>=3:api.objects.pop('incident-38',None)
                        return request(path,patch=patch,absent=absent)
                    api.delete=delete;api.request=read
                    if interrupted:
                        with self.assertRaises(KeyboardInterrupt):run()
                        # A new coordinator object/process must recover from disk, not memory.
                        if kind=='lifecycle':f.lifecycle=Lifecycle(f.root/'state',lambda _:api)
                    markers={p:p.read_bytes() for p in f.root.rglob('*.json') if 'DELETE_DISPATCHED' in p.read_text()}
                    # Avoid wall-clock sleeps only; real caller and confirmation logic execute.
                    with patch('tools.exhibition_deletion.time.sleep',lambda _:None):result=run()
                    self.assertTrue(result);self.assertEqual(len(api.deleted),1)
                    self.assertTrue(all(p.read_bytes()==raw for p,raw in markers.items()))
                    self.assertTrue(list(f.root.rglob('*.confirmed.json')))

if __name__=='__main__':unittest.main()
