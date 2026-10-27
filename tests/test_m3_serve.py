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
