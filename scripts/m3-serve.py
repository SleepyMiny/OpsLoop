#!/usr/bin/env python3
"""Loopback exhibition entrypoint. Replay is default; Live performs GETs only."""
import argparse
import functools
import http.server
import json
from pathlib import Path
import signal
import subprocess
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from tools.engineering_dashboard_bridge import projection, join
PROM_PATH = '/api/v1/namespaces/monitoring/services/http:opsloop-mon-kube-prometheu-prometheus:9090/proxy'


class Handler(http.server.SimpleHTTPRequestHandler):
    live = False
    bff_ports = {'a': 8090, 'b': 8091}
    engineering = None

    def parse_request(self):
        if not super().parse_request():
            return False
        allowed = {f'127.0.0.1:{self.server.server_port}', f'localhost:{self.server.server_port}'}
        host = self.headers.get('Host', '')
        origin = self.headers.get('Origin')
        if host not in allowed or origin is not None and origin != 'http://' + host:
            self.send_error(403, 'Loopback same-origin access only')
            return False
        return True

    def end_headers(self):
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Referrer-Policy', 'no-referrer')
        self.send_header('Cache-Control', 'no-store')
        super().end_headers()

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path.startswith('/api/'):
            for scope, port in self.bff_ports.items():
                if path == f'/api/{scope}/api/v1/snapshot' and self.live:
                    try:
                        # Never forward browser cookies, authorization or an arbitrary URL.
                        with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/v1/snapshot', timeout=5) as r:
                            body = r.read(4 * 1024 * 1024)
                        if self.engineering is not None:
                            body = json.dumps(join(json.loads(body), self.engineering)).encode()
                        self.send_response(200)
                        self.send_header('Content-Type', 'application/json')
                        self.end_headers()
                        self.wfile.write(body)
                    except (OSError, urllib.error.URLError, ValueError, TypeError, KeyError):
                        self.send_error(503, 'Live observation unavailable')
                    return
            self.send_error(503 if not self.live else 404, 'Replay has no observation API' if not self.live else 'Unknown API')
            return
        resolved = Path(self.translate_path(path)).resolve()
        if not resolved.is_relative_to(Path(self.directory).resolve()) or resolved.is_dir() and path != '/':
            self.send_error(404)
            return
        super().do_GET()

    def list_directory(self, path):
        self.send_error(404)

    def send_head(self):
        path = Path(self.translate_path(self.path))
        if path.is_dir():
            path = path / 'index.html'
        if not path.resolve().is_relative_to(Path(self.directory).resolve()):
            self.send_error(404)
            return None
        return super().send_head()

    def reject_write(self):
        self.send_response(405)
        self.send_header('Allow', 'GET, HEAD')
        self.end_headers()

    do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = reject_write


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--mode', choices=['replay', 'live'], default='replay')
    p.add_argument('--port', type=int, default=4173)
    p.add_argument('--dist', type=Path, default=ROOT / 'frontend/dashboard/dist')
    p.add_argument('--bff-bin', type=Path, default=ROOT / 'build/dashboard-bff')
    p.add_argument('--kubeconfig', type=Path)
    p.add_argument('--kubectl', default='kubectl')
    p.add_argument('--engineering-artifacts', type=Path, help='Pinned local artifact manifest; history joins only on exact UID/run/epoch binding')
    p.add_argument('--prometheus', action='store_true', help='Start GET-only Kubernetes service proxy for Prometheus instant queries')
    args = p.parse_args()
    if not (args.dist / 'index.html').is_file():
        p.error('Dashboard build missing: npm --prefix frontend/dashboard run build')
    if args.mode == 'live' and (not args.kubeconfig or not args.kubeconfig.is_file() or not args.bff_bin.is_file()):
        p.error('Live requires a server-only --kubeconfig and built --bff-bin')
    if args.mode != 'live' and args.prometheus:
        p.error('--prometheus requires --mode live')
    if args.engineering_artifacts:
        if args.mode != 'live':
            p.error('--engineering-artifacts requires --mode live')
        try:
            manifest = json.loads(args.engineering_artifacts.read_bytes())
            for entry in manifest['files'].values():
                entry_path = Path(entry['path'])
                if not entry_path.is_absolute():
                    entry['path'] = str(args.engineering_artifacts.resolve().parent / entry_path)
            Handler.engineering = projection(manifest)
        except (OSError, ValueError, KeyError, TypeError):
            p.error('Engineering evidence validation failed; no unverified projection will be served')
    children = []
    Handler.live = args.mode == 'live'
    server = http.server.ThreadingHTTPServer(('127.0.0.1', args.port), functools.partial(Handler, directory=str(args.dist.resolve())))
    try:
        if args.mode == 'live':
            prom_url = ''
            if args.prometheus:
                children.append(subprocess.Popen([args.kubectl, '--kubeconfig', str(args.kubeconfig), 'proxy',
                    '--address=127.0.0.1', '--port=19090', '--accept-hosts=^127\\.0\\.0\\.1$',
                    f'--accept-paths=^{PROM_PATH}/api/v1/query$', '--reject-methods=^(POST|PUT|PATCH|DELETE|CONNECT|TRACE|OPTIONS)$']))
                prom_url = 'http://127.0.0.1:19090' + PROM_PATH
            for scope, policy in [('a', 'demo-api'), ('b', 'engineering-demo')]:
                children.append(subprocess.Popen([str(args.bff_bin.resolve()), '--listen', f'127.0.0.1:{Handler.bff_ports[scope]}',
                    '--kubeconfig', str(args.kubeconfig), '--namespace', f'opsloop-demo-{scope}', '--policy', policy,
                    '--prometheus-url', prom_url]))
        def stop(*_):
            threading.Thread(target=server.shutdown, daemon=True).start()
        signal.signal(signal.SIGINT, stop)
        signal.signal(signal.SIGTERM, stop)
        print(json.dumps({'url': f'http://127.0.0.1:{args.port}', 'mode': args.mode, 'clusterWrites': 0, 'children': [c.pid for c in children]}), flush=True)
        server.serve_forever(poll_interval=.25)
    finally:
        server.server_close()
        for c in children:
            if c.poll() is None:
                c.terminate()
        for c in children:
            try:
                c.wait(timeout=5)
            except subprocess.TimeoutExpired:
                c.kill()
                c.wait()


if __name__ == '__main__':
    main()
