"""Exhibition server observation and filesystem boundaries, using loopback only."""
import importlib.util
import functools
import http.client
import http.server
from pathlib import Path
import tempfile
import threading
import unittest

spec = importlib.util.spec_from_file_location('m3serve', Path(__file__).resolve().parents[1] / 'scripts/m3-serve.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ServerBoundary(unittest.TestCase):
    def test_replay_has_no_api_no_writes_and_no_file_escape(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / 'site').mkdir()
            (root / 'private').write_text('LOCAL_PRIVATE_SENTINEL')
            (root / 'site/index.html').write_text('sealed replay')
            (root / 'site/escape').symlink_to(root / 'private')
            module.Handler.live = False
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(module.Handler, directory=str(root / 'site')))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                def request(method, path, headers=None):
                    conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=2)
                    conn.request(method, path, headers=headers or {})
                    response = conn.getresponse()
                    status, body = response.status, response.read()
                    conn.close()
                    self.assertNotIn(b'LOCAL_PRIVATE_SENTINEL', body)
                    return status
                self.assertEqual(request('GET', '/'), 200)
                self.assertEqual(request('GET', '/api/a/api/v1/snapshot'), 503)
                self.assertEqual(request('GET', '/escape'), 404)
                self.assertEqual(request('GET', '/../private'), 404)
                for method in ['POST', 'PATCH', 'PUT', 'DELETE']:
                    self.assertEqual(request(method, '/api/a/api/v1/snapshot'), 405)
                self.assertEqual(request('GET', '/', {'Host': 'attacker.invalid'}), 403)
                self.assertEqual(request('GET', '/', {'Origin': 'http://attacker.invalid'}), 403)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()


if __name__ == '__main__':
    unittest.main()

class ControlHTTPBoundary(unittest.TestCase):
    def test_control_requires_same_origin_token_and_exact_fields(self):
        class FakeCoordinator:
            calls = 0
            def start(self, plan, request):
                self.calls += 1
                return {'runId': 'synthetic-run', 'source': 'synthetic'}
            def reconcile(self, run_id):
                self.calls += 1
                return {'runId': run_id, 'state': 'RECONCILING'}
            def state(self):
                return {'active': None, 'runs': [], 'plans': []}
        import json
        with tempfile.TemporaryDirectory() as d:
            coordinator = FakeCoordinator()
            class Handler(module.Handler):
                live = True
                csrf_token = 'local-test-token'
            Handler.coordinator = coordinator
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Handler, directory=d))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                origin = f'http://127.0.0.1:{server.server_port}'
                def post(data, headers, action="start"):
                    conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=2)
                    conn.request('POST', '/api/exhibition/' + action, json.dumps(data), headers)
                    response = conn.getresponse()
                    code = response.status
                    response.read()
                    conn.close()
                    return code
                headers = {'Content-Type': 'application/json', 'Origin': origin, 'X-OpsLoop-CSRF': 'local-test-token'}
                body = {'planId': 'approved-plan', 'requestId': 'unique-click'}
                self.assertEqual(post(body, {'Content-Type': 'application/json'}), 403)
                self.assertEqual(post(body, {**headers, 'Origin': 'http://attacker.invalid'}), 403)
                self.assertEqual(post(body, {**headers, 'X-OpsLoop-CSRF': 'wrong'}), 403)
                self.assertEqual(post({**body, 'command': 'arbitrary'}, headers), 400)
                self.assertEqual(coordinator.calls, 0)
                self.assertEqual(post(body, headers), 200)
                self.assertEqual(coordinator.calls, 1)
                self.assertEqual(post({'runId': 'synthetic-run', 'stopConfirmed': 'true'}, headers, 'reconcile'), 400)
                self.assertEqual(post({'runId': 'synthetic-run'}, {'Content-Type': 'application/json'}, 'reconcile'), 403)
                self.assertEqual(coordinator.calls, 1)
                self.assertEqual(post({'runId': 'synthetic-run'}, headers, 'reconcile'), 200)
                self.assertEqual(coordinator.calls, 2)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()

class IntegratedHTTPFlow(unittest.TestCase):
    def test_synthetic_adapter_real_http_ledger_and_reconnect(self):
        import datetime as dt
        import hashlib
        import json
        from tools.exhibition_control import Coordinator
        class Adapter:
            calls = 0
            def prepare(self, plan):
                pass
            def reconcile(self, plan, emit):
                return {'binding': plan['binding'], 'stopConfirmed': True, 'modelStopped': True}
            def run(self, plan, emit, cancel):
                self.calls += 1
                emit('observer', 'BASELINE_CONFIRMED', {'status': 'LOCAL_SYNTHETIC_TEST_ONLY'})
                cancel.wait(2)
                emit('guard', 'STOP_CONFIRMED', {'stopConfirmed': True})
                return {'state': 'STOPPED', 'stopConfirmed': True}
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            plan = {'schema': 'opsloop-exhibition-plan/v1', 'planId': 'test-plan', 'authorizationId': 'test-authority',
                'track': 'runtime', 'source': 'measured', 'binding': {'runID': 'test-run', 'epoch': 'test-epoch', 'policyUID': 'test-policy', 'deploymentUID': 'test-workload'},
                'expiresAt': (dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=1)).isoformat(), 'totalSeconds': 30, 'approvalReference': 'LOCAL_SYNTHETIC_TEST_ONLY'}
            file = root / 'plan.json'; file.write_text(json.dumps(plan))
            config = {'schema': 'opsloop-exhibition-control/v1', 'plans': [{'path': str(file), 'sha256': hashlib.sha256(file.read_bytes()).hexdigest()}]}
            adapter = Adapter()
            class SyntheticLifecycle:
                def verify_run(self, run): return {}
                def config(self, plan): return {}
                def reconcile(self, plan, receipt, emit):
                    return {'binding': plan['binding'], 'stopConfirmed': True, 'baselineRestored': True, 'modelStopped': True, 'evidenceArchived': True, 'objectsCleaned': True, 'lifecycleReceiptSHA256': 'a' * 64}
            coordinator = Coordinator(root / 'state', config, {'runtime': adapter}, lifecycle=SyntheticLifecycle())
            class Handler(module.Handler):
                live = True
                csrf_token = 'local-flow-token'
            Handler.coordinator = coordinator
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Handler, directory=d))
            thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
            origin = f'http://127.0.0.1:{server.server_port}'
            def request(path, body=None):
                conn = http.client.HTTPConnection('127.0.0.1', server.server_port, timeout=2)
                conn.request('POST' if body is not None else 'GET', '/api/exhibition/' + path,
                    None if body is None else json.dumps(body), {'Origin': origin, 'Content-Type': 'application/json', 'X-OpsLoop-CSRF': 'local-flow-token'})
                response = conn.getresponse(); status = response.status; data = json.loads(response.read()); conn.close()
                return status, data
            try:
                self.assertTrue(request('capabilities')[1]['control'])
                self.assertFalse(request('state')[1]['plans'][0]['ready'])
                self.assertEqual(request('prepare', {'planId': 'test-plan'})[0], 200)
                first = request('start', {'planId': 'test-plan', 'requestId': 'click-1'})
                duplicate = request('start', {'planId': 'test-plan', 'requestId': 'click-1'})
                self.assertEqual(first[1]['runId'], duplicate[1]['runId'])
                self.assertEqual(request('state')[1]['active']['runId'], 'test-run')
                self.assertEqual(request('stop', {'runId': 'test-run'})[0], 200)
                coordinator.worker.join(2)
                self.assertEqual(adapter.calls, 1)
                self.assertEqual(request('state')[1]['runs'][0]['state'], 'STOPPED')
                all_events = request('events?runId=test-run&after=0')[1]['events']
                self.assertEqual(request('events?runId=test-run&after=1')[1]['events'], all_events[1:])
                self.assertEqual(request('artifacts?runId=test-run')[1], {'artifacts': []})
                self.assertEqual(request('prepare', {'planId': 'test-plan'})[0], 409)
                self.assertIsNone(request('state')[1]['active'])
            finally:
                coordinator.close(); server.shutdown(); server.server_close(); thread.join()

class DisconnectedPreview(unittest.TestCase):
    def test_preview_never_contacts_bff_or_exposes_control(self):
        from unittest.mock import patch
        import json
        class Handler(module.Handler):
            live=True
            observation_enabled=False
            coordinator=None
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),functools.partial(Handler,directory='/tmp'))
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            with patch.object(module.urllib.request,'urlopen',side_effect=AssertionError('preview contacted BFF')):
                for path in ['/api/exhibition/capabilities','/api/a/api/v1/snapshot','/api/b/api/v1/snapshot']:
                    conn=http.client.HTTPConnection('127.0.0.1',server.server_port,timeout=2);conn.request('GET',path)
                    response=conn.getresponse();body=response.read();conn.close()
                    if path.endswith('capabilities'):
                        caps=json.loads(body);self.assertFalse(caps['control']);self.assertFalse(caps['observationConfigured']);self.assertTrue(caps['live'])
                    else:self.assertNotEqual(response.status,200)
        finally:server.shutdown();server.server_close();thread.join()

class ObservationExpiryHTTP(unittest.TestCase):
    def test_expired_approval_returns_last_snapshot_without_upstream_get(self):
        from unittest.mock import patch
        import json,time
        class Handler(module.Handler):
            live=True
            observation_enabled=True
            observe_until=time.time()-1
            last_snapshots={'a':{'observedAt':123,'source':'live'}}
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),functools.partial(Handler,directory='/tmp'))
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            with patch.object(module.urllib.request,'urlopen',side_effect=AssertionError('expired observation sent upstream GET')):
                conn=http.client.HTTPConnection('127.0.0.1',server.server_port,timeout=2);conn.request('GET','/api/a/api/v1/snapshot')
                response=conn.getresponse();body=json.loads(response.read());conn.close()
                self.assertEqual(response.status,410);self.assertEqual(body['snapshot']['observedAt'],123)
        finally:server.shutdown();server.server_close();thread.join()
