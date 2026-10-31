#!/usr/bin/env python3
"""Loopback exhibition entrypoint. Explicit observation and opt-in, pinned experiment control."""
import argparse
import functools
import datetime as dt
import time
import http.server
import json
from pathlib import Path
import signal
import secrets
import subprocess
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from tools.engineering_dashboard_bridge import projection, join
from tools.exhibition_control import Coordinator, ControlError, pinned
PROM_PATH = '/api/v1/namespaces/monitoring/services/http:opsloop-mon-kube-prometheu-prometheus:9090/proxy'


class Handler(http.server.SimpleHTTPRequestHandler):
    live = False
    observation_enabled = True
    bff_ports = {'a': 8090, 'b': 8091}
    engineering = None
    coordinator = None
    ledger_view = None
    csrf_token = None
    public_artifacts = None
    observe_until = None
    last_snapshots = {}

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
        if path == '/api/exhibition/capabilities':
            return self.json_response(200, {'live': self.live, 'control': self.coordinator is not None,
                'mode': 'live' if self.live else 'replay', 'observationConfigured': self.live and self.observation_enabled, 'lastObservationAvailable': bool(self.last_snapshots), 'csrfToken': self.csrf_token})
        if path == '/api/exhibition/state':
            return self.json_response(200, self.coordinator.state() if self.coordinator else self.ledger_view.state() if self.ledger_view else {'active': None, 'runs': [], 'plans': []})
        if path == '/api/exhibition/artifacts':
            try:
                query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
                run_id = query.get('runId', [''])[0]
                if not self.public_artifacts or not (self.coordinator or self.ledger_view) or not any(r['runId'] == run_id for r in (self.coordinator or self.ledger_view).state()['runs']):
                    return self.json_response(200, {'artifacts': []})
                from tools.exhibition_disclosure import load_public_artifacts
                return self.json_response(200, load_public_artifacts(*self.public_artifacts, run_id))
            except Exception:
                return self.json_response(409, {'error': 'DISCLOSURE_NOT_CONFIRMED'})
        if path == '/api/exhibition/events' and (self.coordinator or self.ledger_view):
            try:
                query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
                return self.json_response(200, (self.coordinator or self.ledger_view).events(query.get('runId', [''])[0], int(query.get('after', ['0'])[0])))
            except (ValueError, ControlError):
                return self.json_response(400, {'error': 'INVALID_CURSOR'})
        if path.startswith('/api/'):
            for scope, port in self.bff_ports.items():
                if path == f'/api/{scope}/api/v1/snapshot' and self.live:
                    if not self.observation_enabled:
                        return self.json_response(410, {'error':'OBSERVATION_CLOSED','snapshot':self.last_snapshots.get(scope)})
                    if self.observe_until is not None and time.time() >= self.observe_until:
                        return self.json_response(410, {'error':'OBSERVATION_APPROVAL_EXPIRED','snapshot':self.last_snapshots.get(scope)})
                    try:
                        # Never forward browser cookies, authorization or an arbitrary URL.
                        with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/v1/snapshot', timeout=5) as r:
                            body = r.read(4 * 1024 * 1024)
                        if self.engineering is not None:
                            body = json.dumps(join(json.loads(body), self.engineering)).encode()
                        self.last_snapshots[scope] = json.loads(body)
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

    def json_response(self, status, value):
        body = json.dumps(value, allow_nan=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        path = urllib.parse.urlsplit(self.path).path
        if not self.coordinator or path not in ('/api/exhibition/prepare', '/api/exhibition/start', '/api/exhibition/stop', '/api/exhibition/reconcile'):
            return self.reject_write()
        # Require exact browser origin AND a server session token. Host checking
        # alone does not stop cross-origin simple requests or DNS rebinding.
        if (self.headers.get('Origin') != 'http://' + self.headers.get('Host', '') or
            self.headers.get('Content-Type') != 'application/json' or
            not secrets.compare_digest(self.headers.get('X-OpsLoop-CSRF', ''), self.csrf_token or '')):
            return self.json_response(403, {'error': 'SAME_ORIGIN_TOKEN_REQUIRED'})
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 < length <= 4096 or self.headers.get('Transfer-Encoding'):
                raise ValueError()
            self.connection.settimeout(3)
            data = json.loads(self.rfile.read(length))
            fields = {'/api/exhibition/prepare': {'planId'}, '/api/exhibition/start': {'planId', 'requestId'}, '/api/exhibition/stop': {'runId'}, '/api/exhibition/reconcile': {'runId'}}[path]
            if not isinstance(data, dict) or set(data) != fields or not all(isinstance(v, str) for v in data.values()):
                raise ValueError()
        except (ValueError, OSError):
            return self.json_response(400, {'error': 'INVALID_REQUEST'})
        try:
            if path.endswith('/prepare'):
                result = self.coordinator.prepare(data['planId'])
            elif path.endswith('/start'):
                result = self.coordinator.start(data['planId'], data['requestId'])
            elif path.endswith('/reconcile'):
                result = self.coordinator.reconcile(data['runId'])
            else:
                result = self.coordinator.stop(data['runId'])
            return self.json_response(200, result)
        except ControlError as exc:
            return self.json_response(409, {'error': str(exc)})
        except Exception:
            # Raw adapter exceptions may contain private URLs, local paths or credentials.
            return self.json_response(409, {'error': 'EXECUTION_BOUNDARY_REJECTED'})

    do_PUT = do_PATCH = do_DELETE = do_OPTIONS = reject_write


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--mode', choices=['replay', 'live', 'disconnected'], default='replay')
    p.add_argument('--port', type=int, default=4173)
    p.add_argument('--dist', type=Path, default=ROOT / 'frontend/dashboard/dist')
    p.add_argument('--bff-bin', type=Path, default=ROOT / 'build/dashboard-bff')
    p.add_argument('--kubeconfig', type=Path)
    p.add_argument('--kubectl', default='kubectl')
    p.add_argument('--engineering-artifacts', type=Path, help='Pinned local artifact manifest; history joins only on exact UID/run/epoch binding')
    p.add_argument('--public-artifacts', type=Path, help='Separately reviewed exact public diff/checks catalog')
    p.add_argument('--public-artifacts-sha256')
    p.add_argument('--control-config', type=Path, help='Private pinned approved execution plan catalog; never browser supplied')
    p.add_argument('--control-config-sha256')
    p.add_argument('--control-state', type=Path, help='Private durable execution ledger, preserved across restarts')
    p.add_argument('--closed-snapshots', type=Path, help='Pinned final observations served as closed/stale only; no cluster access')
    p.add_argument('--closed-snapshots-sha256')
    p.add_argument('--ledger-view', type=Path, help='GET-only view of the host-owned execution ledger; no execution controls')
    p.add_argument('--observe-session', type=Path, help='Pinned active execution session bounding observation to its 90-minute window')
    p.add_argument('--observe-session-sha256')
    p.add_argument('--observe-until', help='Absolute UTC GET approval deadline; retains last snapshot after closing observation')
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
    if args.closed_snapshots:
        if args.mode != 'disconnected' or not args.closed_snapshots_sha256:p.error('Closed observations require disconnected mode and a hash')
        Handler.last_snapshots=pinned(args.closed_snapshots,args.closed_snapshots_sha256)
        if set(Handler.last_snapshots)!={'a','b'}:p.error('Closed observation scope mismatch')
    if args.ledger_view:
        if args.control_config: p.error('Ledger observation and Coordinator ownership are mutually exclusive')
        from tools.exhibition_view import LedgerView
        Handler.ledger_view=LedgerView(args.ledger_view.resolve())
    if args.control_config:
        if args.mode != 'live' or not args.control_state or not args.control_config_sha256:
            p.error('Experiment control requires live mode, explicit durable state and pinned approved catalog')
        from tools.exhibition_runtime import RuntimeAdapter
        from tools.exhibition_engineering import EngineeringAdapter
        try:
            Handler.coordinator = Coordinator(args.control_state, pinned(args.control_config, args.control_config_sha256),
                {'runtime': RuntimeAdapter(), 'engineering': EngineeringAdapter()})
            Handler.csrf_token = secrets.token_urlsafe(32)
        except (ValueError, OSError, KeyError):
            p.error('Control configuration rejected; no experiment started')
    elif args.control_state or args.control_config_sha256:
        p.error('Control state/hash requires explicit control catalog')
    if args.public_artifacts:
        if not (Handler.coordinator or Handler.ledger_view) or not args.public_artifacts_sha256:
            p.error('Public artifact disclosure requires a control ledger and exact reviewed catalog hash')
        try:
            pinned(args.public_artifacts, args.public_artifacts_sha256)
        except (ValueError, OSError):
            p.error('Public disclosure catalog rejected')
        Handler.public_artifacts = (args.public_artifacts, args.public_artifacts_sha256)
    elif args.public_artifacts_sha256:
        p.error('Public artifact hash requires a catalog')
    if args.observe_until:
        try:
            until=dt.datetime.fromisoformat(args.observe_until.replace('Z','+00:00'))
            maximum=time.time()+1800
            if args.observe_session:
                session=pinned(args.observe_session,args.observe_session_sha256)
                start=dt.datetime.fromisoformat(session['startedAt']).timestamp();end=dt.datetime.fromisoformat(session['deadline']).timestamp()
                if session.get('delegatedWork'):
                    from tools.exhibition_b_work import work_auth
                    work_auth(session['delegatedWork'],deadline=end)
                    if not start<=time.time()<end:raise ValueError()
                elif not start<=time.time()<end<=start+5400 or end>dt.datetime.fromisoformat(session['completionDeadline']).timestamp():raise ValueError()
                maximum=end
            if until.tzinfo is None or not time.time() < until.timestamp() <= maximum: raise ValueError()
            Handler.observe_until=until.timestamp()
        except ValueError: p.error('Observation deadline must be a future timezone-aware time within 30 minutes')
    children = []
    Handler.live = args.mode != 'replay'
    Handler.observation_enabled = args.mode == 'live'
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
                    '--prometheus-url', prom_url, '--pod-namespaces', 'opsloop-demo-a,opsloop-demo-b,opsloop-system', *(['--observe-until', args.observe_until] if args.observe_until else [])]))
        if Handler.observe_until:
            def expire_observation():
                for child in children:
                    if child.poll() is None: child.terminate()
            expiry_timer=threading.Timer(max(0,Handler.observe_until-time.time()), expire_observation)
            expiry_timer.daemon=True; expiry_timer.start()
        def stop(*_):
            threading.Thread(target=server.shutdown, daemon=True).start()
        signal.signal(signal.SIGINT, stop)
        signal.signal(signal.SIGTERM, stop)
        print(json.dumps({'url': f'http://127.0.0.1:{args.port}', 'mode': args.mode, 'controlEnabled': Handler.coordinator is not None, 'children': [c.pid for c in children]}), flush=True)
        server.serve_forever(poll_interval=.25)
    finally:
        if Handler.coordinator:
            Handler.coordinator.close()
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
