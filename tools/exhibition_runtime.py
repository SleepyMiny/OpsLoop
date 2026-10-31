"""Finite Demo A adapter. No network or process runs until prepare/run is called.

Only a server-pinned execution plan is accepted. This is not a general command
runner: the sole mutation paths are the approved Policy CAS/heartbeat, existing
Guard/Finish and one in-Pod readiness command. Original journals are never reset.
"""
from __future__ import annotations

import base64
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

GRANT = 'opsloop.io/execution-grant'
BINDING = 'opsloop.io/execution-grant-binding'
SAFE = re.compile(r'^[A-Za-z0-9_.:-]{1,253}$')
SHA = re.compile(r'^[0-9a-f]{64}$')


class RuntimeRejected(Exception):
    """Fixed public error codes only; never include transport bodies/credentials."""


def require(condition, code):
    if not condition:
        raise RuntimeRejected(code)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()


def stamp(value):
    try:
        result = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
        require(result.tzinfo is not None, 'TIME_INVALID')
        return result.timestamp()
    except (ValueError, TypeError, AttributeError):
        raise RuntimeRejected('TIME_INVALID') from None


def pinned(ref, limit=1024 * 1024):
    require(isinstance(ref, dict) and set(ref) == {'path', 'sha256'}, 'PIN_INVALID')
    p = Path(ref['path'])
    require(p.is_absolute() and not p.is_symlink() and SHA.fullmatch(ref['sha256']), 'PIN_INVALID')
    try:
        with p.open('rb') as f:
            content = f.read(limit + 1)
    except OSError:
        raise RuntimeRejected('PIN_UNAVAILABLE') from None
    require(len(content) <= limit and hashlib.sha256(content).hexdigest() == ref['sha256'], 'PIN_CHANGED')
    return content


def process_json(argv, timeout, limit, cancel=None, *, diagnostics=None):
    """Bounded subprocess; cancellation kills only this new process group.

    An interrupted external mutation remains uncertain and must be reconciled by
    Guard/Finish. It is never retried. No further work is launched after cancel.
    """
    require(cancel is None or not cancel.is_set(), 'CANCELLED')
    process = None
    try:
        if diagnostics is not None:
            from tools.exhibition_lifecycle import private_dir, write
            diagnostics=private_dir(str(diagnostics))
        with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
            started=time.time()
            try:
                process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=output, stderr=errors,
                                           start_new_session=True, env={'PATH': '/usr/bin:/bin', 'HOME': '/nonexistent', 'LANG': 'C'})
                deadline = time.monotonic() + timeout
                while process.poll() is None:
                    require(cancel is None or not cancel.is_set(), 'CANCELLED_DISPATCH_UNCONFIRMED')
                    require(time.monotonic() < deadline, 'COMMAND_DEADLINE_UNCONFIRMED')
                    require(max(os.fstat(output.fileno()).st_size,os.fstat(errors.fileno()).st_size) <= limit, 'COMMAND_OUTPUT_LIMIT')
                    if cancel is None: time.sleep(.025)
                    else: cancel.wait(.025)
                require(cancel is None or not cancel.is_set(), 'CANCELLED_DISPATCH_UNCONFIRMED')
                require(process.returncode == 0, 'COMMAND_REJECTED_OR_UNCONFIRMED')
                output.seek(0); raw = output.read(limit + 1)
                require(len(raw) <= limit, 'COMMAND_OUTPUT_LIMIT')
                result = json.loads(raw)
                require(isinstance(result, dict), 'COMMAND_RECEIPT_INVALID')
                return result
            finally:
                if diagnostics is not None:
                    # Only the fixed Guard caller enables this private sink.
                    # Never persist argv/environment or browser-facing raw text.
                    if process is not None and process.poll() is None:
                        try: os.killpg(process.pid,signal.SIGKILL)
                        except ProcessLookupError: pass
                        process.wait(timeout=1)
                    record=private_dir(str(diagnostics/uuid.uuid4().hex))
                    refs={}
                    for name,stream in (('stdout',output),('stderr',errors)):
                        stream.seek(0);raw=stream.read(limit);write(record/name,raw,exclusive=True)
                        refs[name]={'sha256':hashlib.sha256(raw).hexdigest(),'truncated':os.fstat(stream.fileno()).st_size>limit}
                    write(record/'receipt.json',canonical({'startedAt':started,'endedAt':time.time(),
                        'exitCode':None if process is None else process.returncode,'inputSHA256':hashlib.sha256(canonical(argv)).hexdigest(),**refs}),exclusive=True)
    except (OSError, ValueError, subprocess.SubprocessError):
        raise RuntimeRejected('COMMAND_UNCONFIRMED') from None
    finally:
        if process is not None and process.poll() is None:
            try: os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            try: process.wait(timeout=1)
            except subprocess.TimeoutExpired: raise RuntimeRejected('COMMAND_TERMINATION_UNCONFIRMED') from None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeRejected('REDIRECT_REJECTED')


def static_kubeconfig(raw):
    """Parse the existing pinned static JSON/YAML config; reject aliases/duplicates.

    This does not execute plugins or export credentials. Transport still enforces
    exactly one static credential form and TLS-only, explicit-origin access.
    """
    import yaml
    from tools.exhibition_hashes import parse
    try:
        result=parse(raw)
        require(isinstance(result,dict),'STATIC_KUBECONFIG_REQUIRED')
        return result
    except ValueError:pass
    class Strict(yaml.SafeLoader):
        pass
    def mapping(loader,node,deep=False):
        result={}
        for k,v in node.value:
            key=loader.construct_object(k,deep=deep)
            require(isinstance(key,str) and key not in result and key!='<<','STATIC_KUBECONFIG_REQUIRED')
            result[key]=loader.construct_object(v,deep=deep)
        return result
    Strict.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,mapping)
    try:
        require(not any(isinstance(e,yaml.events.AliasEvent) for e in yaml.parse(raw)),'STATIC_KUBECONFIG_REQUIRED')
        result=yaml.load(raw,Loader=Strict)
        require(isinstance(result,dict),'STATIC_KUBECONFIG_REQUIRED')
        return result
    except yaml.YAMLError:raise RuntimeRejected('STATIC_KUBECONFIG_REQUIRED') from None


class Transport:
    """One static JSON/YAML kubeconfig, TLS verification, no ambient proxy/exec auth."""
    def __init__(self, settings):
        self.settings = settings
        raw = pinned(settings['kubeconfig'])
        try:
            config = static_kubeconfig(raw)
            context = next(x['context'] for x in config['contexts'] if x['name'] == config['current-context'])
            cluster = next(x['cluster'] for x in config['clusters'] if x['name'] == context['cluster'])
            user = next(x['user'] for x in config['users'] if x['name'] == context['user'])
        except (ValueError, KeyError, TypeError, StopIteration):
            raise RuntimeRejected('STATIC_KUBECONFIG_REQUIRED') from None
        require(set(cluster) <= {'server', 'certificate-authority-data'} and set(cluster) == {'server', 'certificate-authority-data'}, 'TLS_CONFIG_REJECTED')
        self.origin = cluster['server'].rstrip('/')
        url = urllib.parse.urlsplit(self.origin)
        require(url.scheme == 'https' and url.hostname and not url.username and not url.password and not url.query and not url.fragment and not url.path, 'API_ORIGIN_REJECTED')
        require(self.origin == settings['apiOrigin'], 'API_ORIGIN_CHANGED')
        require(set(user) in ({'token'}, {'client-certificate-data', 'client-key-data'}), 'STATIC_AUTH_REQUIRED')
        self.temp = tempfile.TemporaryDirectory(prefix='opsloop-runtime-')
        try:
            ca = base64.b64decode(cluster['certificate-authority-data'], validate=True).decode()
            ctx = ssl.create_default_context(cadata=ca)
            ctx.minimum_version = ssl.TLSVersion.TLSv1_2
            self.token = user.get('token')
            if self.token:
                require(re.fullmatch(r'[A-Za-z0-9._~-]+', self.token), 'TOKEN_FORMAT_REJECTED')
            else:
                paths = []
                for key in ('client-certificate-data', 'client-key-data'):
                    p = Path(self.temp.name) / key
                    with os.fdopen(os.open(p, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as f:
                        f.write(base64.b64decode(user[key], validate=True))
                    paths.append(str(p))
                ctx.load_cert_chain(*paths)
            self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(), urllib.request.HTTPSHandler(context=ctx))
            # Private snapshot prevents kubectl reading a changed auth/plugin document.
            self.kubeconfig = Path(self.temp.name) / 'kubeconfig.json'
            with os.fdopen(os.open(self.kubeconfig, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as f:
                f.write(raw)
        except Exception:
            self.temp.cleanup()
            raise RuntimeRejected('TLS_IDENTITY_REJECTED') from None

    def close(self):
        self.temp.cleanup()

    def request(self, path, patch=None, absent=False):
        require(path.startswith('/') and not path.startswith('//'), 'API_PATH_REJECTED')
        headers = {'Accept': 'application/json'}
        if self.token:
            headers['Authorization'] = 'Bearer ' + self.token
        if patch is not None:
            headers['Content-Type'] = 'application/json-patch+json'
        req = urllib.request.Request(self.origin + path, data=canonical(patch) if patch is not None else None, headers=headers,
                                     method='PATCH' if patch is not None else 'GET')
        try:
            with self.opener.open(req, timeout=2) as response:
                raw = response.read(1024 * 1024 + 1)
            require(len(raw) <= 1024 * 1024, 'API_RESPONSE_LIMIT')
            result = json.loads(raw)
            require(isinstance(result, dict), 'API_RESPONSE_OBJECT_REQUIRED')
            return result
        except urllib.error.HTTPError as error:
            if absent and patch is None and error.code == 404: return None
            raise RuntimeRejected('API_OBSERVATION_UNCONFIRMED' if patch is None else 'API_WRITE_UNCONFIRMED') from None
        except (OSError, ValueError, urllib.error.URLError):
            raise RuntimeRejected('API_OBSERVATION_UNCONFIRMED' if patch is None else 'API_WRITE_UNCONFIRMED') from None

    def probe(self):
        started = time.monotonic()
        url = self.settings['serviceURL']
        headers = {'Connection': 'close'}
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        # ClusterIP services are reached through the already pinned API origin.
        # TLS credentials must never follow a redirect or reach another URL.
        if url.startswith(self.origin + '/'):
            expected = self.origin + '/api/v1/namespaces/' + self.settings['namespace'] + '/services/http:' + self.settings['service'] + ':8080/proxy/'
            require(url == expected, 'SERVICE_PROXY_SCOPE_CHANGED')
            opener = self.opener
            if self.token: headers['Authorization'] = 'Bearer ' + self.token
        request = urllib.request.Request(url, headers=headers)
        status, body = None, {}
        try:
            try:
                response = opener.open(request, timeout=2)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                status = response.status
                raw = response.read(65537)
            if len(raw) <= 65536:
                body = json.loads(raw)
        except (OSError, ValueError, urllib.error.URLError, RuntimeRejected):
            pass
        # Never return body/error text. Pod self-report is joined against API identities.
        return {'httpStatus': status, 'latencyMs': round((time.monotonic() - started) * 1000),
                **{k: v for k, v in body.items() if k in {'pod', 'podUID', 'node', 'namespace', 'arch'} and isinstance(v, str) and SAFE.fullmatch(v)}}

    def fault(self, cancel):
        require(not cancel.is_set(), 'CANCELLED')
        r = self.settings
        pinned(r['kubectl'], 256 * 1024 * 1024)
        argv = [r['kubectl']['path'], '--kubeconfig', str(self.kubeconfig), '--request-timeout=5s',
                '-n', r['namespace'], 'exec', r['sourcePod']['name'], '-c', r['container'], '--',
                '/app', 'inject-readiness-fault', '--pod-uid', r['sourcePod']['uid'], '--seconds', str(r['faultSeconds'])]
        require(not cancel.is_set(), 'CANCELLED')
        receipt = process_json(argv, 7, 4096, cancel)
        require(receipt.get('podUID') == r['sourcePod']['uid'] and receipt.get('status') == 'fault-active' and receipt.get('namespace') == r['namespace'], 'FAULT_RECEIPT_MISMATCH')
        return {'podUID': r['sourcePod']['uid'], 'durationSeconds': r['faultSeconds']}


def public_candidate(c):
    out = {}
    for key in ('nodeName', 'nodeUID'):
        if isinstance(c.get(key), str) and SAFE.fullmatch(c[key]): out[key] = c[key]
    if type(c.get('eligible')) is bool: out['eligible'] = c['eligible']
    allowed_reasons = {'NODE_NOT_READY', 'UNSCHEDULABLE', 'UNTOLERATED_TAINT', 'MEMORY_PRESSURE', 'DISK_PRESSURE', 'PID_PRESSURE', 'CPU_PRESSURE', 'ARCH_MISMATCH', 'INSUFFICIENT_REQUESTED_CPU', 'INSUFFICIENT_REQUESTED_MEMORY', 'INSUFFICIENT_AVAILABLE_MEMORY', 'METRICS_MISSING', 'METRICS_STALE', 'COOLDOWN', 'UNSUPPORTED_CONSTRAINT'}
    out['rejectReasons'] = [v for v in c.get('rejectReasons', [])[:15] if v in allowed_reasons]
    for group in ('measurements', 'resourceCondition'):
        values = c.get(group, {})
        if isinstance(values, dict):
            # Public evidence is numeric only; arbitrary text in CR fields is never a log.
            out[group] = {k: v for k, v in values.items() if k in {
                'cpuUtilizationPermille', 'memAvailableBytes', 'memTotalBytes', 'requestedCPUMilli',
                'requestedMemoryBytes', 'allocatableCPUMilli', 'allocatableMemoryBytes',
                'measuredCPU', 'appliedCPU', 'measuredMem', 'appliedMem',
                'measuredCPUPermille', 'appliedCPUPermille', 'measuredMemoryBytes', 'appliedMemoryBytes'}
                and type(v) in (int, float) and 0 <= v <= 2**63 - 1}
    condition = c.get('resourceCondition')
    if isinstance(condition, dict):
        applied = public_candidate({'measurements': condition.get('appliedMeasurements', {})}).get('measurements', {})
        projected = {'appliedMeasurements': applied}
        for key in ('runID', 'epoch', 'policyUID'):
            if isinstance(condition.get(key), str) and SAFE.fullmatch(condition[key]): projected[key] = condition[key]
        if isinstance(condition.get('configSHA256'), str) and SHA.fullmatch(condition['configSHA256']): projected['configSHA256'] = condition['configSHA256']
        out['resourceCondition'] = projected
    return out


class RuntimeAdapter:
    def __init__(self, transport_factory=Transport, clock=time.time, monotonic=time.monotonic, sleep=time.sleep):
        self.transport_factory, self.clock, self.monotonic, self.sleep = transport_factory, clock, monotonic, sleep

    def authority_now(self, plan):
        r = plan['runtime']; now = self.clock(); grant = r['executionGrant']
        require(stamp(grant['notBefore']) <= now < min(stamp(grant['notAfter']), stamp(plan['expiresAt'])), 'EXECUTION_EXPIRED')
        if 'resourceConditions' not in r: return
        raw = pinned(r['resourceConditions'], 16384)
        def unique(pairs):
            out = {}
            for key, value in pairs:
                require(key not in out, 'RESOURCE_CONDITION_DUPLICATE_KEY'); out[key] = value
            return out
        c = json.loads(raw, object_pairs_hook=unique)
        require(set(c) == {'schema', 'runID', 'epoch', 'policyUID', 'deploymentUID', 'notBefore', 'expiresAt', 'nodes'} and c['schema'] == 'opsloop-resource-conditions/v1', 'RESOURCE_CONDITION_SCHEMA')
        require(all(c[k] == v for k, v in plan['binding'].items()), 'RESOURCE_CONDITION_BINDING')
        begin, end = stamp(c['notBefore']), stamp(c['expiresAt'])
        require(stamp(grant['notBefore']) <= begin <= now < end <= stamp(grant['notAfter']) and 0 < end - begin <= 600, 'RESOURCE_CONDITION_EXPIRED')
        require(isinstance(c['nodes'], list) and 1 <= len(c['nodes']) <= 16, 'RESOURCE_CONDITION_NODES')
        names, uids = set(), set()
        for n in c['nodes']:
            require(set(n) <= {'nodeName', 'nodeUID', 'cpuUtilizationFloorPermille', 'memoryAvailableCeilingBytes'} and {'nodeName', 'nodeUID'} < set(n), 'RESOURCE_CONDITION_FIELDS')
            require({'name': n['nodeName'], 'uid': n['nodeUID']} in r['nodes'] and n['nodeName'] not in names and n['nodeUID'] not in uids, 'RESOURCE_CONDITION_NODE_CHANGED')
            names.add(n['nodeName']); uids.add(n['nodeUID'])
            for key, maximum in [('cpuUtilizationFloorPermille', 1000), ('memoryAvailableCeilingBytes', 2**63-1)]:
                if key in n: require(type(n[key]) is int and 0 <= n[key] <= maximum, 'RESOURCE_CONDITION_VALUE')

    def settings(self, plan):
        require(plan.get('track') == 'runtime' and plan.get('source') == 'measured', 'RUNTIME_PLAN_REQUIRED')
        r = plan['runtime']
        require(stamp(plan['expiresAt']) > self.clock(), 'PLAN_EXPIRED')
        for key in ('namespace', 'policy', 'workload', 'service', 'serviceUID', 'clusterUID', 'namespaceUID', 'container'):
            require(isinstance(r.get(key), str) and SAFE.fullmatch(r[key]), 'RUNTIME_BINDING_INVALID')
        for item in [r['sourcePod'], *r['nodes']]:
            require(set(item) == {'name', 'uid'} and all(isinstance(v, str) and SAFE.fullmatch(v) for v in item.values()), 'RESOURCE_BINDING_INVALID')
        require(1 <= len(r['nodes']) <= 16 and len({n['uid'] for n in r['nodes']}) == len(r['nodes']), 'NODE_BINDING_INVALID')
        require(type(r['faultSeconds']) is int and 30 <= r['faultSeconds'] <= 600, 'FAULT_DURATION_INVALID')
        require(type(r['baselineSamples']) is int and 3 <= r['baselineSamples'] <= 10 and type(r['recoverySamples']) is int and 3 <= r['recoverySamples'] <= 60 and 2 <= r['stableSeconds'] <= 60, 'PROBE_CONTRACT_INVALID')
        url = urllib.parse.urlsplit(r['serviceURL'])
        require(url.scheme in ('http', 'https') and url.hostname and not url.username and not url.password and not url.query and not url.fragment, 'SERVICE_URL_INVALID')
        guard = json.loads(pinned(r['guardConfig']))
        for key in ('runID', 'epoch', 'policyUID', 'deploymentUID'):
            require(guard[key] == plan['binding'][key], 'GUARD_BINDING_MISMATCH')
        require(guard['namespace'] == r['namespace'] and guard['policy'] == r['policy'] and guard['workload'] == r['workload'] and guard['apiOrigin'] == r['apiOrigin'], 'GUARD_TARGET_MISMATCH')
        require(guard['mode'] == 'hybrid-live' and guard['evidenceKind'] == 'observed', 'GUARD_MODE_INVALID')
        require(guard['kubeconfigFile'] == r['kubeconfig']['path'], 'GUARD_CREDENTIAL_BINDING')
        require(stamp(guard['hardDeadline']) <= stamp(plan['expiresAt']) and self.clock() + plan['totalSeconds'] <= stamp(guard['hardDeadline']), 'GUARD_DEADLINE_INVALID')
        require(1000 <= guard['heartbeatMs'] <= 10000, 'GUARD_HEARTBEAT_INVALID')
        g = r['executionGrant']
        require(set(g) == {'version', 'scope', 'instance', 'notBefore', 'notAfter'} and g['version'] == 'opsloop-execution-grant/v1', 'GRANT_INVALID')
        for key, value in plan['binding'].items():
            require(g['scope'][key] == value, 'GRANT_BINDING_MISMATCH')
        require(g['scope']['namespace'] == r['namespace'] and g['scope']['policy'] == r['policy'] and g['scope']['deployment'] == r['workload'] and g['scope']['policyGeneration'] == guard['policyGeneration'], 'GRANT_TARGET_MISMATCH')
        require(stamp(g['notBefore']) <= self.clock() < stamp(g['notAfter']) <= stamp(guard['hardDeadline']) and stamp(g['notAfter']) - stamp(g['notBefore']) <= 600, 'GRANT_TIME_INVALID')
        require(self.clock() + plan['totalSeconds'] <= stamp(g['notAfter']), 'GRANT_HORIZON_INVALID')
        before = r['policyBefore']
        if 'exhibitionPermit' in plan:
            from tools.exhibition_permit import bound_permit
            _, permit = bound_permit(plan)
            if 'resourceConditions' in r:
                require(permit.get('resourceConditions') == json.loads(pinned(r['resourceConditions'], 16384)), 'PERMIT_RESOURCE_CONDITIONS_CHANGED')
            else:
                require('resourceConditions' not in permit, 'PERMIT_RESOURCE_CONDITIONS_MISSING')
            require(permit['grant'] == g and type(before.get('incidentSequence')) is int and before['incidentSequence'] == permit['baseIncidentSequence'], 'PERMIT_LIFETIME_BASE_CHANGED')
            require(before['spec'].get('enabled') is False, 'REPEAT_POLICY_NOT_STOPPED')
            expected = copy.deepcopy(before['spec']); expected.update(mode='Enforce', enabled=True)
            require(expected == guard['policySpec'] and before['generation'] + 2 == guard['policyGeneration'], 'POLICY_TRANSITION_INVALID')
        else:
            expected = copy.deepcopy(before['spec']); expected['mode'] = 'Enforce'
            require(before['spec'].get('mode') == 'ObserveOnly' and before['spec'].get('enabled') is True and expected == guard['policySpec'] and before['generation'] + 1 == guard['policyGeneration'], 'POLICY_TRANSITION_INVALID')
        require(SHA.fullmatch(r['guardBinding']) is not None and SHA.fullmatch(r['grantBinding']) is not None and SHA.fullmatch(r['deploymentSpecSHA256']) is not None and SHA.fullmatch(r['sourcePodSpecSHA256']) is not None, 'GUARD_CONFIRM_INVALID')
        pinned(r['driver'], 256 * 1024 * 1024)
        pinned(r['kubectl'], 256 * 1024 * 1024)
        pinned(r['kubeconfig'])
        self.authority_now(plan)
        return r, guard

    @staticmethod
    def paths(r):
        core = '/api/v1/namespaces/' + r['namespace']
        return {'cluster': '/api/v1/namespaces/kube-system', 'namespace': core,
                'policy': '/apis/opsloop.io/v1alpha1/namespaces/' + r['namespace'] + '/recoverypolicies/' + r['policy'],
                'deployment': '/apis/apps/v1/namespaces/' + r['namespace'] + '/deployments/' + r['workload'],
                'service': core + '/services/' + r['service'], 'pod': core + '/pods/' + r['sourcePod']['name'],
                'endpoints': '/apis/discovery.k8s.io/v1/namespaces/' + r['namespace'] + '/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3D' + r['service'] + '&limit=100',
                'incident': '/apis/opsloop.io/v1alpha1/namespaces/' + r['namespace'] + '/recoveryincidents/' + r['executionGrant']['scope']['incident']}

    @staticmethod
    def identity(obj, uid):
        require(obj.get('metadata', {}).get('uid') == uid and not obj['metadata'].get('deletionTimestamp'), 'IDENTITY_UNCONFIRMED')

    def baseline(self, plan, api, emit=lambda *args: None, cancel=None):
        r = plan['runtime']; paths = self.paths(r)
        for key, uid in [('cluster', r['clusterUID']), ('namespace', r['namespaceUID']), ('service', r['serviceUID']), ('deployment', plan['binding']['deploymentUID']), ('pod', r['sourcePod']['uid'])]:
            self.identity(api.request(paths[key]), uid)
        deployment = api.request(paths['deployment'])
        require(hashlib.sha256(canonical(deployment['spec'])).hexdigest() == r['deploymentSpecSHA256'] and deployment.get('status', {}).get('readyReplicas') == 1 and deployment['status'].get('observedGeneration') == deployment['metadata']['generation'], 'DEPLOYMENT_BASELINE_CHANGED')
        pod = api.request(paths['pod'])
        require(hashlib.sha256(canonical(pod['spec'])).hexdigest() == r['sourcePodSpecSHA256'], 'SOURCE_POD_SPEC_CHANGED')
        containers = pod.get('status', {}).get('containerStatuses', [])
        require(len(containers) == 1 and containers[0].get('name') == r['container'] and containers[0].get('imageID') == r['sourceImageID'] and containers[0].get('restartCount') == 0 and containers[0].get('ready') is True, 'SOURCE_CONTAINER_CHANGED')
        require(any(c.get('type') == 'Ready' and c.get('status') == 'True' for c in pod.get('status', {}).get('conditions', [])), 'SOURCE_POD_NOT_READY')
        policy = api.request(paths['policy']); self.identity(policy, plan['binding']['policyUID'])
        require(policy['metadata']['generation'] == r['policyBefore']['generation'] and policy['spec'] == r['policyBefore']['spec'] and not policy.get('status', {}).get('activeIncident') and policy.get('status', {}).get('incidentSequence', 0) == r['policyBefore'].get('incidentSequence', 0) and not policy['metadata'].get('annotations', {}).get(GRANT), 'BASELINE_POLICY_CHANGED')
        if 'exhibitionPermit' in plan:
            require(any(c.get('type') == 'Quiesced' and c.get('status') == 'True' and c.get('reason') == 'FenceClosed' and c.get('observedGeneration') == policy['metadata']['generation'] for c in policy.get('status', {}).get('conditions', [])), 'REPEAT_FENCE_UNCONFIRMED')
        for n in r['nodes']:
            node = api.request('/api/v1/nodes/' + n['name']); self.identity(node, n['uid'])
            conditions = {c['type']: c['status'] for c in node.get('status', {}).get('conditions', [])}
            require(conditions.get('Ready') == 'True' and all(conditions.get(k) == 'False' for k in ('MemoryPressure', 'DiskPressure', 'PIDPressure')), 'NODE_HEALTH_UNCONFIRMED')
        for _ in range(r['baselineSamples']):
            require(cancel is None or not cancel.is_set(), 'CANCELLED')
            sample = api.probe(); emit('observer', 'SERVICE_PROBE', sample)
            require(sample.get('httpStatus') == 200 and sample.get('podUID') == r['sourcePod']['uid'] and sample.get('namespace') == r['namespace'], 'BASELINE_NOT_HEALTHY')
        return policy

    def prepare(self, plan):
        self.settings(plan)
        api = self.transport_factory(plan['runtime'])
        try:
            self.baseline(plan, api)
        finally:
            api.close()

    def cli(self, r, action, sequence=None, cancel=None):
        pinned(r['driver'], 256 * 1024 * 1024); pinned(r['guardConfig'])
        argv = [r['driver']['path'], 'finish-workload' if action == 'stop' else 'guard-workload']
        if action != 'stop': argv.append(action)
        argv += ['-config', r['guardConfig']['path'], '-execute', '-confirm', r['guardBinding']]
        if sequence is not None: argv += ['-sequence', str(sequence)]
        return process_json(argv, 20, 1024 * 1024, cancel, diagnostics=Path(r['guardConfig']['path']).parent/'guard-command-diagnostics')

    def stop_and_wait(self, plan, guard, watcher):
        """Finish dispatches once. Pending acknowledgement is observed via status.

        Existing Guard watcher owns fresh API proof and finite stop completion.
        No repeated Finish, heartbeat, Policy write, or new execution authority.
        """
        r=plan['runtime']
        try: receipt=self.cli(r,'stop')
        except RuntimeRejected: receipt=self.cli(r,'status')
        end=self.monotonic()+min(30,max(0,stamp(guard['authorityExpires'])-self.clock()))
        while True:
            state=receipt.get('state',{})
            require(receipt.get('runID')==plan['binding']['runID'] and state.get('binding')==r['guardBinding'],'STOP_IDENTITY_UNCONFIRMED')
            if state.get('phase')=='STOP_CONFIRMED': return receipt
            require(state.get('phase') in ('STOP_INTENT','STOP_DISPATCHING','STOP_RECONCILING') and watcher is not None and watcher.poll() is None,'STOP_UNCONFIRMED')
            require(self.monotonic()<end,'STOP_ACKNOWLEDGEMENT_DEADLINE')
            self.sleep(min(.25,max(0,end-self.monotonic())))
            require(self.monotonic()<end,'STOP_ACKNOWLEDGEMENT_DEADLINE')
            receipt=self.cli(r,'status')

    def watcher(self, r):
        return subprocess.Popen([r['driver']['path'], 'guard-workload', 'watch', '-config', r['guardConfig']['path'], '-execute', '-confirm', r['guardBinding']],
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                start_new_session=True, env={'PATH': '/usr/bin:/bin', 'HOME': '/nonexistent', 'LANG': 'C'})

    def check_guard(self, plan, phase):
        status = self.cli(plan['runtime'], 'status')
        require(status.get('runID') == plan['binding']['runID'] and status.get('state', {}).get('binding') == plan['runtime']['guardBinding'] and status['state'].get('phase') == phase, 'GUARD_STATE_UNCONFIRMED')
        return status

    @staticmethod
    def preconditions(obj):
        return [{'op': 'test', 'path': '/metadata/uid', 'value': obj['metadata']['uid']},
                {'op': 'test', 'path': '/metadata/resourceVersion', 'value': obj['metadata']['resourceVersion']},
                {'op': 'test', 'path': '/metadata/generation', 'value': obj['metadata']['generation']},
                {'op': 'test', 'path': '/spec', 'value': obj['spec']}]

    def grant_patch(self, plan, api, cancel, grant, activate=False):
        r = plan['runtime']; path = self.paths(r)['policy']; p = api.request(path)
        self.identity(p, plan['binding']['policyUID'])
        pre = copy.deepcopy(r['policyBefore'])
        if 'exhibitionPermit' in plan:
            pre['spec'].update(enabled=True, mode='ObserveOnly'); pre['generation'] += 1
        expected = copy.deepcopy(pre['spec'])
        if not activate: expected['mode'] = 'Enforce'
        require(p['spec'] == expected and p['metadata']['generation'] == pre['generation'] + (not activate), 'ACTIVE_POLICY_CHANGED')
        self.check_guard(plan, 'ARMED')
        annotations = p['metadata'].get('annotations', {})
        if activate: require(GRANT not in annotations, 'GRANT_ALREADY_PRESENT')
        else:
            previous = json.loads(annotations[GRANT])
            require({k: previous[k] for k in r['executionGrant']} == r['executionGrant'] and previous['heartbeatSequence'] == grant['heartbeatSequence'] - 1, 'GRANT_CHANGED')
        patch = self.preconditions(p)
        if activate: patch.append({'op': 'replace', 'path': '/spec/mode', 'value': 'Enforce'})
        if p['metadata'].get('annotations') is None:
            patch.append({'op': 'add', 'path': '/metadata/annotations', 'value': {GRANT: canonical(grant).decode()}})
        else:
            patch.append({'op': 'add', 'path': '/metadata/annotations/' + GRANT.replace('/', '~1'), 'value': canonical(grant).decode()})
        if 'exhibitionPermit' in plan:
            from tools.exhibition_permit import bound_permit, ANNOTATION
            envelope, permit = bound_permit(plan)
            require(permit['grant'] == r['executionGrant'], 'PERMIT_GRANT_CHANGED')
            if activate:
                # The signature travels with this exact grant, never private key material.
                if p['metadata'].get('annotations') is None:
                    patch[-1]['value'][ANNOTATION] = canonical(envelope).decode()
                else:
                    patch.append({'op':'add','path':'/metadata/annotations/' + ANNOTATION.replace('/','~1'),'value':canonical(envelope).decode()})
            else:
                require(json.loads(annotations.get(ANNOTATION, '{}')) == envelope, 'PERMIT_ANNOTATION_CHANGED')
        self.authority_now(plan)
        require(not cancel.is_set() and self.clock() < stamp(grant['notAfter']), 'CANCELLED_OR_EXPIRED')
        actual = api.request(path, patch)
        self.identity(actual, plan['binding']['policyUID'])
        require(actual['metadata']['generation'] == pre['generation'] + 1 and actual['metadata']['annotations'][GRANT] == canonical(grant).decode(), 'GRANT_RESPONSE_UNCONFIRMED')

    def observe_only(self, plan, api, cancel):
        """One pre-Enforce transition on the fixed Policy; no status/history reset."""
        if 'exhibitionPermit' not in plan: return
        from tools.exhibition_permit import bound_permit
        bound_permit(plan)
        r=plan['runtime']; p=api.request(self.paths(r)['policy'])
        self.identity(p, plan['binding']['policyUID'])
        before=r['policyBefore']
        require(p['spec']==before['spec'] and p['metadata']['generation']==before['generation'] and p.get('status',{}).get('incidentSequence',0)==before['incidentSequence'], 'REPEAT_POLICY_CHANGED')
        require(not cancel.is_set(), 'CANCELLED')
        expected=copy.deepcopy(before['spec']);expected.update(enabled=True,mode='ObserveOnly')
        patch=self.preconditions(p)+[{'op':'replace','path':'/spec','value':expected}]
        actual=api.request(self.paths(r)['policy'],patch)
        require(actual['metadata']['uid']==plan['binding']['policyUID'] and actual['spec']==expected and actual['metadata']['generation']==before['generation']+1,'OBSERVE_ONLY_TRANSITION_UNCONFIRMED')

    def observe(self, plan, api, emit, observed):
        r = plan['runtime']; paths = self.paths(r)
        policy = api.request(paths['policy']); self.identity(policy, plan['binding']['policyUID'])
        dep = api.request(paths['deployment']); self.identity(dep, plan['binding']['deploymentUID'])
        sample = api.probe(); emit('observer', 'SERVICE_PROBE', sample)
        incident = None
        active = policy.get('status', {}).get('activeIncident')
        if active:
            require(active.get('name') == r['executionGrant']['scope']['incident'] and (not observed.get('incidentUID') or active.get('incidentUID') == observed['incidentUID']), 'ACTIVE_INCIDENT_CHANGED')
        if (active and active.get('incidentUID')) or observed.get('incidentUID'):
            incident = api.request(paths['incident'])
            require(not observed.get('incidentUID') or incident['metadata']['uid'] == observed['incidentUID'], 'INCIDENT_UID_CHANGED')
            require(not active or incident['metadata']['uid'] == active.get('incidentUID'), 'ACTIVE_INCIDENT_UID_CHANGED')
            observed['incidentUID'] = incident['metadata']['uid']
            spec = incident['spec']
            require(spec['policyRef']['uid'] == plan['binding']['policyUID'] and spec['workload']['uid'] == plan['binding']['deploymentUID'] and spec['policyGeneration'] == r['executionGrant']['scope']['policyGeneration'] and spec['policyHash'] == r['executionGrant']['scope']['policyHash'], 'INCIDENT_BINDING_CHANGED')
            require(incident['metadata'].get('annotations', {}).get(BINDING) == r['grantBinding'], 'INCIDENT_GRANT_CHANGED')
            if 'lifecycle' in plan:
                from tools.exhibition_reconcile import config, new_file
                _,root=config(plan); path=root/'incident-identity.json'
                identity={'schema':'opsloop-exhibition-incident-identity/v1','binding':plan['binding'],'namespace':r['namespace'],'name':r['executionGrant']['scope']['incident'],'uid':incident['metadata']['uid'],'grantBinding':r['grantBinding']}
                if path.exists():require(json.loads(path.read_bytes())==identity,'INCIDENT_CUSTODY_CHANGED')
                else:new_file(path,identity)
            runtime = incident.get('status', {}).get('runtime', {})
            d = runtime.get('decision') or {}
            emit('controller', 'CONTROLLER_ACTION', {k: v for k, v in {'incidentUID': incident['metadata']['uid'], 'phase': runtime.get('phase'), 'actionID': d.get('actionID'), 'selectedNode': d.get('selectedNodeName')}.items() if isinstance(v, str) and SAFE.fullmatch(v)})
            candidates = []
            for c in d.get('candidates', [])[:16]:
                candidates.append(public_candidate(c))
            emit('controller', 'CANDIDATES', {'candidates': candidates})
        endpoints = api.request(paths['endpoints'])
        require(not endpoints.get('metadata', {}).get('continue') and len(endpoints.get('items', [])) <= 100, 'ENDPOINT_LIST_INCOMPLETE')
        pod_uids = []
        for item in endpoints.get('items', []):
            owners = item.get('metadata', {}).get('ownerReferences', [])
            require(any(o.get('kind') == 'Service' and o.get('uid') == r['serviceUID'] for o in owners), 'ENDPOINT_SERVICE_UID_CHANGED')
            for address in item.get('endpoints', []):
                conditions = address.get('conditions', {})
                ref = address.get('targetRef', {})
                if conditions.get('ready') is True and conditions.get('terminating') is not True and ref.get('kind') == 'Pod' and ref.get('namespace') == r['namespace'] and SAFE.fullmatch(ref.get('uid', '')):
                    pod_uids.append(ref['uid'])
        emit('observer', 'ENDPOINT', {'podUIDs': sorted(set(pod_uids))})
        recovered = incident and incident.get('status', {}).get('runtime', {}).get('phase') == 'RECOVERED'
        healthy = bool(recovered and sample.get('httpStatus') == 200 and sample.get('namespace') == r['namespace'] and sample.get('podUID') != r['sourcePod']['uid'] and sample.get('podUID') in pod_uids)
        if not healthy: return False
        require(isinstance(sample.get('pod'), str) and SAFE.fullmatch(sample['pod']), 'RESPONSE_POD_MISSING')
        pod = api.request('/api/v1/namespaces/' + r['namespace'] + '/pods/' + sample['pod'])
        self.identity(pod, sample['podUID'])
        owners = [o for o in pod['metadata'].get('ownerReferences', []) if o.get('controller') is True]
        require(len(owners) == 1 and owners[0].get('kind') == 'ReplicaSet' and SAFE.fullmatch(owners[0].get('name', '')), 'RECOVERY_POD_OWNER_UNCONFIRMED')
        rs = api.request('/apis/apps/v1/namespaces/' + r['namespace'] + '/replicasets/' + owners[0]['name'])
        self.identity(rs, owners[0]['uid'])
        rs_owners = [o for o in rs['metadata'].get('ownerReferences', []) if o.get('controller') is True]
        require(len(rs_owners) == 1 and rs_owners[0].get('kind') == 'Deployment' and rs_owners[0].get('uid') == plan['binding']['deploymentUID'], 'RECOVERY_DEPLOYMENT_OWNER_UNCONFIRMED')
        decision = incident['status']['runtime'].get('decision') or {}
        require(pod.get('spec', {}).get('nodeName') == decision.get('selectedNodeName') and any(c.get('type') == 'Ready' and c.get('status') == 'True' for c in pod.get('status', {}).get('conditions', [])), 'RECOVERY_PLACEMENT_UNCONFIRMED')
        emit('scheduler', 'POD_PLACED', {'podUID': sample['podUID'], 'node': pod['spec']['nodeName'], 'deploymentUID': plan['binding']['deploymentUID']})
        return True


    def settle_fault(self, plan, api, confirmed_at, emit=lambda *args: None):
        """Policy stop does not clear readiness. Prove source absence or TTL end.

        Reads are bounded by the approved plan's absolute horizon. A cancelled
        or lost exec receipt cannot use a guessed TTL to declare the fault over.
        """
        r = plan['runtime']; paths = self.paths(r)
        guard = json.loads(pinned(r['guardConfig']))
        # TTL limits the fault, not a later read proving that it has ended.
        # Preserve the TTL+5 propagation wait, and allow a bounded fresh identity
        # proof (up to seven 2-second GETs) when recovery finishes after that.
        # Both remain inside the original plan/Guard read authority.
        wall_end = min(stamp(plan['expiresAt']), stamp(guard['authorityExpires']))
        now = self.monotonic()
        authority_end = now + max(0, wall_end - self.clock())
        wait_end = min(authority_end, confirmed_at + r['faultSeconds'] + 5) if confirmed_at is not None else authority_end
        # Do not begin a late multi-GET proof without its bounded read budget.
        if now >= wait_end and authority_end - now < 15: return False
        deadline = min(authority_end, max(wait_end, now + 15))
        def bounded_read(operation):
            require(self.monotonic() < deadline and self.clock() < wall_end, 'CLEANUP_READ_HORIZON_EXPIRED')
            value = operation()
            require(self.monotonic() < deadline and self.clock() < wall_end, 'CLEANUP_READ_HORIZON_EXPIRED')
            return value
        pending = False
        while self.monotonic() < deadline and self.clock() < wall_end:
            source = bounded_read(lambda: api.request(paths['pod'], absent=True))
            if source is None or source.get('metadata', {}).get('uid') != r['sourcePod']['uid']:
                sample = bounded_read(api.probe)
                require(sample.get('httpStatus') == 200 and sample.get('namespace') == r['namespace'] and sample.get('podUID') != r['sourcePod']['uid'] and isinstance(sample.get('pod'), str) and SAFE.fullmatch(sample['pod']), 'POST_STOP_SERVICE_UNCONFIRMED')
                pod = bounded_read(lambda: api.request('/api/v1/namespaces/' + r['namespace'] + '/pods/' + sample['pod']))
                self.identity(pod, sample['podUID'])
                require(any(c.get('type') == 'Ready' and c.get('status') == 'True' for c in pod.get('status', {}).get('conditions', [])), 'POST_STOP_POD_NOT_READY')
                owner = [o for o in pod['metadata'].get('ownerReferences', []) if o.get('controller') is True]
                require(len(owner) == 1 and owner[0].get('kind') == 'ReplicaSet' and SAFE.fullmatch(owner[0].get('name', '')), 'POST_STOP_POD_OWNER_UNCONFIRMED')
                rs = bounded_read(lambda: api.request('/apis/apps/v1/namespaces/' + r['namespace'] + '/replicasets/' + owner[0]['name']))
                self.identity(rs, owner[0]['uid'])
                owners = [o for o in rs['metadata'].get('ownerReferences', []) if o.get('controller') is True]
                require(len(owners) == 1 and owners[0].get('kind') == 'Deployment' and owners[0].get('uid') == plan['binding']['deploymentUID'], 'POST_STOP_DEPLOYMENT_OWNER_UNCONFIRMED')
                return True
            if confirmed_at is None: return False
            if self.monotonic() >= confirmed_at + r['faultSeconds'] and not source['metadata'].get('deletionTimestamp') and any(c.get('type') == 'Ready' and c.get('status') == 'True' for c in source.get('status', {}).get('conditions', [])):
                return True
            if self.monotonic() >= wait_end: return False
            if not pending:
                emit('coordinator', 'CLEANUP_PENDING', {'reasonCode': 'READINESS_FAULT_NOT_YET_SETTLED', 'podUID': r['sourcePod']['uid']})
                pending = True
            # Cancellation has already stopped mutation; it must not abort the
            # bounded cleanup reads needed to safely release the single slot.
            self.sleep(min(.1, max(0, deadline - self.monotonic())))
        return False

    def reconcile(self, plan, emit):
        from tools.exhibition_reconcile import reconcile_runtime
        return reconcile_runtime(plan, self, emit)

    def run(self, plan, emit, cancel_event):
        r, guard = self.settings(plan)
        from tools.exhibition_reconcile import record_start, record_outcome
        record_start(plan)
        api = self.transport_factory(r)
        armed, watcher, success, failure = False, None, False, None
        stop_failure = None
        fault_dispatched = False
        fault_confirmed_at = None
        start = self.monotonic()
        try:
            self.baseline(plan, api, emit, cancel_event)
            require(not cancel_event.is_set(), 'CANCELLED')
            # Set before dispatch: a lost arm response still requires Finish reconciliation.
            armed = True
            self.observe_only(plan, api, cancel_event)
            self.cli(r, 'arm', cancel=cancel_event)
            require(not cancel_event.is_set(), 'CANCELLED')
            watcher = self.watcher(r)
            for _ in range(20):
                require(watcher.poll() is None and not cancel_event.is_set(), 'GUARD_WATCH_UNAVAILABLE')
                state = self.cli(r, 'status')
                if state.get('state', {}).get('phase') == 'ARMED': break
                cancel_event.wait(.05)
            self.check_guard(plan, 'ARMED')
            grant = copy.deepcopy(r['executionGrant'])
            grant.update(heartbeatSequence=1, heartbeatAt=dt.datetime.fromtimestamp(self.clock(), dt.timezone.utc).isoformat())
            self.grant_patch(plan, api, cancel_event, grant, activate=True)
            emit('coordinator', 'ENFORCE_ACTIVATED', {'policyUID': plan['binding']['policyUID']})
            source = api.request(self.paths(r)['pod']); self.identity(source, r['sourcePod']['uid'])
            require(hashlib.sha256(canonical(source['spec'])).hexdigest() == r['sourcePodSpecSHA256'], 'SOURCE_POD_SPEC_CHANGED')
            containers = source.get('status', {}).get('containerStatuses', [])
            require(len(containers) == 1 and containers[0].get('imageID') == r['sourceImageID'] and containers[0].get('restartCount') == 0, 'SOURCE_CONTAINER_CHANGED')
            self.check_guard(plan, 'ARMED')
            emit('coordinator', 'FAULT_DISPATCHED', {'podUID': r['sourcePod']['uid']})
            self.authority_now(plan)
            require(not cancel_event.is_set(), 'CANCELLED')
            if 'lifecycle' in plan:
                from tools.exhibition_reconcile import config, new_file
                _,root=config(plan)
                new_file(root/'fault-intent.json',{'binding':plan['binding'],'sourcePodUID':r['sourcePod']['uid'],'durationSeconds':r['faultSeconds']})
            fault_dispatched = True
            receipt = api.fault(cancel_event)
            if 'lifecycle' in plan:
                new_file(root/'fault-confirmed.json',{'binding':plan['binding'],'sourcePodUID':r['sourcePod']['uid'],'durationSeconds':r['faultSeconds'], 'hostBoot':Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'confirmedBootSeconds':time.clock_gettime(time.CLOCK_BOOTTIME)})
            fault_confirmed_at = self.monotonic()
            emit('workload', 'READINESS_FAULT', receipt)
            count, stable_at = 0, None
            observed = {}
            sequence, next_beat = 1, self.monotonic()
            while self.monotonic() - start < plan['totalSeconds']:
                require(not cancel_event.is_set() and watcher.poll() is None, 'CANCELLED_OR_GUARD_EXITED')
                if self.monotonic() >= next_beat:
                    sequence += 1
                    self.cli(r, 'heartbeat', sequence, cancel_event)
                    grant['heartbeatSequence'] = sequence
                    grant['heartbeatAt'] = dt.datetime.fromtimestamp(self.clock(), dt.timezone.utc).isoformat()
                    self.grant_patch(plan, api, cancel_event, grant)
                    next_beat = self.monotonic() + min(2, guard['heartbeatMs'] / 3000)
                healthy = self.observe(plan, api, emit, observed)
                count = count + 1 if healthy else 0
                stable_at = stable_at or self.monotonic() if healthy else None
                if count >= r['recoverySamples'] and stable_at is not None and self.monotonic() - stable_at >= r['stableSeconds']:
                    emit('observer', 'SERVICE_RECOVERED', {'stableSamples': count, 'stableElapsedMS': round((self.monotonic() - stable_at) * 1000)})
                    success = True; break
                cancel_event.wait(.5)
            if not success: failure = 'CANCELLED' if cancel_event.is_set() else 'RECOVERY_NOT_CONFIRMED'
        except Exception as exc:
            failure = str(exc) if isinstance(exc, RuntimeRejected) else 'RUNTIME_UNCONFIRMED'
        finally:
            execution_failure = failure
            stop_confirmed = not armed
            if armed:
                try:
                    receipt = self.stop_and_wait(plan, guard, watcher)
                    require(receipt.get('runID') == plan['binding']['runID'] and receipt.get('state', {}).get('binding') == r['guardBinding'] and receipt['state'].get('phase') == 'STOP_CONFIRMED', 'STOP_UNCONFIRMED')
                    self.check_guard(plan, 'STOP_CONFIRMED')
                    stop_confirmed = not fault_dispatched or self.settle_fault(plan, api, fault_confirmed_at, emit)
                    require(stop_confirmed, 'READINESS_FAULT_TERMINATION_UNCONFIRMED')
                    if fault_dispatched: emit('workload', 'FAULT_SETTLED', {'podUID': r['sourcePod']['uid']})
                    emit('guard', 'STOP_CONFIRMED', {'policyUID': plan['binding']['policyUID']})
                except Exception as exc:
                    stop_failure = str(exc) if isinstance(exc, RuntimeRejected) else type(exc).__name__
                    failure = 'STOP_UNCONFIRMED'
            # A live watchdog is not killed when acknowledgement is missing. Its
            # original owner/heartbeat/deadline responsibility remains in force.
            if watcher is not None and stop_confirmed:
                try: watcher.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    stop_confirmed = False
            api.close()
        if not stop_confirmed: state = 'UNCONFIRMED_TERMINATION'
        elif cancel_event.is_set(): state = 'STOPPED'
        else: state = 'COMPLETED' if success else 'FAILED'
        outcome = {'state': state, 'stopConfirmed': stop_confirmed, 'reason': failure, 'executionReason': execution_failure, 'stopReason': stop_failure, 'serviceRecovered': success, 'readinessFaultMayRemain': fault_dispatched and not stop_confirmed,
                'faultMaximumSeconds': r['faultSeconds'] if fault_dispatched else 0}
        record_outcome(plan, outcome)
        return outcome
