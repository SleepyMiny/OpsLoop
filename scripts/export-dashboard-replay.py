#!/usr/bin/env python3
"""Offline, allowlisted evidence derivative. Never invokes kubectl or mutates evidence.

Only verified ObserveOnly/no-attempt experiments are supported. Output is public
presentation input, NOT an importable CRD, executable experiment or raw bundle.
"""
import argparse, copy, datetime, decimal, hashlib, json, pathlib, re

SCHEMA = 'opsloop-evidence-replay/v1'

def verified(root):
    root = root.resolve()
    lines = (root / 'SHA256SUMS').read_text().splitlines()
    if not lines: raise ValueError('Empty evidence checksum manifest')
    for line in lines:
        digest, relative = line.split('  ', 1)
        p = (root / relative).resolve()
        if not p.is_relative_to(root) or p.is_symlink() or hashlib.sha256(p.read_bytes()).hexdigest() != digest:
            raise ValueError('Evidence integrity failure: ' + relative)
    return root

def epoch(value):
    return int(datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp() * 1000)

def choose(obj, keys):
    return {key: copy.deepcopy(obj[key]) for key in keys if key in obj}

SPEC = ['targetRef', 'enabled', 'mode', 'candidateSelector', 'requiredPlatforms', 'detection', 'filter', 'score', 'budget', 'verification']

def policy(p):
    if p['spec']['mode'] != 'ObserveOnly':
        raise ValueError('Only ObserveOnly evidence is supported')
    return {'apiVersion': 'opsloop.io/v1alpha1', 'kind': 'RecoveryPolicy',
            'metadata': choose(p['metadata'], ['name', 'namespace', 'uid', 'generation', 'creationTimestamp']),
            'spec': choose(p['spec'], SPEC),
            'status': choose(p.get('status', {}), ['observedGeneration', 'incidentSequence', 'circuits', 'recentFailures'])}

def incident(i):
    if i['spec']['policy']['mode'] != 'ObserveOnly' or i.get('status', {}).get('runtime', {}).get('attempts'):
        raise ValueError('Evidence cannot imply automatic recovery')
    spec = choose(i['spec'], ['sequence', 'policyGeneration', 'policyHash', 'detectedAt', 'deadline'])
    spec['policyRef'] = choose(i['spec']['policyRef'], ['name', 'uid'])
    spec['workload'] = choose(i['spec']['workload'], ['name', 'uid'])
    spec['policy'] = choose(i['spec']['policy'], SPEC)
    spec['trigger'] = choose(i['spec']['trigger'], ['class', 'reason', 'signature', 'ruleVersion', 'nodeName', 'nodeUID', 'podName', 'podUID', 'observedAt'])
    spec['baseline'] = choose(i['spec']['baseline'], ['templateHash', 'specHash', 'imageDigest', 'replicas', 'hostname'])
    rt = choose(i['status']['runtime'], ['phase', 'reason', 'completedAt', 'lastTransitionTime'])
    rt['attempts'] = []
    if 'decision' in i['status']['runtime']:
        d = i['status']['runtime']['decision']
        rt['decision'] = choose(d, ['incidentID', 'actionID', 'policyGeneration', 'policyHash', 'workloadUID', 'baselineTemplateHash', 'imageIndexDigest', 'queryVersion', 'snapshotObservedAt', 'snapshotExpiresAt', 'selectedNodeUID', 'selectedNodeName', 'selectedHostname', 'actionType', 'decisionReason', 'decidedAt'])
        rt['decision']['candidates'] = [dict(choose(c, ['nodeName', 'nodeUID', 'hostname', 'platform', 'eligible', 'rejectReasons']),
            measurements=choose(c.get('measurements', {}), ['cpuUtilizationPermille', 'cpuSampleCount', 'cpuSampleTime', 'memAvailableBytes', 'memTotalBytes', 'memSampleTime', 'allocatableCPUMilli', 'allocatableMemoryBytes', 'requestedCPUMilli', 'requestedMemoryBytes', 'newPodCPUMilli', 'newPodMemoryBytes', 'reserveCPUMilli', 'reserveMemoryBytes']),
            **({'score': choose(c['score'], ['cpuHeadroomPermille', 'memoryHeadroomPermille', 'requestedHeadroomPermille', 'recentFailures', 'penaltyMilli', 'scoreMilli'])} if 'score' in c else {})) for c in d['candidates']]
    return {'apiVersion': 'opsloop.io/v1alpha1', 'kind': 'RecoveryIncident',
            'metadata': choose(i['metadata'], ['name', 'namespace', 'uid', 'generation', 'creationTimestamp']),
            'spec': spec, 'status': {'runtime': rt}}

def workload(d):
    pod = d['spec']['template']['spec']
    if pod.get('initContainers'):
        raise ValueError('Replay exporter does not model init containers')
    cpu = decimal.Decimal(0); mem = decimal.Decimal(0)
    for c in pod['containers']:
        q = c['resources']['requests']; raw = q['cpu']
        cpu += decimal.Decimal(raw[:-1]) if raw.endswith('m') else decimal.Decimal(raw) * 1000
        raw = q['memory']; match = re.fullmatch(r'([0-9.]+)(Ki|Mi|Gi)?', raw)
        if not match: raise ValueError('Unsupported replay request quantity')
        mem += decimal.Decimal(match[1]) * {'Ki': 2**10, 'Mi': 2**20, 'Gi': 2**30, None: 1}[match[2]]
    image = pod['containers'][0]['image']
    return {'uid': d['metadata']['uid'], 'namespace': d['metadata']['namespace'], 'name': d['metadata']['name'],
            'replicas': d['spec']['replicas'], 'nodeSelectorHostname': pod.get('nodeSelector', {}).get('kubernetes.io/hostname'),
            'imageIndexDigest': image.split('@', 1)[1] if '@' in image else '', 'platforms': [],
            'requests': {'cpuMillicores': int(cpu.to_integral_value(rounding=decimal.ROUND_CEILING)), 'memoryBytes': int(mem.to_integral_value(rounding=decimal.ROUND_CEILING))},
            'unsupportedConstraints': []}

def export(image_root, node_root):
    output = {'schemaVersion': SCHEMA, 'experiments': []}
    for ident, title, directory, initial_file in [
        ('image-pull', 'IMAGE_PULL · 실환경 장애 분류', verified(image_root), 'initial-policy-disabled-verified.json'),
        ('node-not-ready', 'NODE_NOT_READY · 후보 평가와 운영자 복원', verified(node_root), 'policy-preflight.json')]:
        sources = {}
        checksums = {line.split('  ', 1)[1]: line.split('  ', 1)[0] for line in (directory / 'SHA256SUMS').read_text().splitlines()}
        def load(name):
            if name not in checksums: raise ValueError('Consumed evidence is not checksummed: ' + name)
            p = directory / name; sources[name] = hashlib.sha256(p.read_bytes()).hexdigest()
            return json.loads(p.read_text())
        initial = load(initial_file)
        if initial.get('kind') != 'RecoveryPolicy': raise ValueError('Expected preserved Policy object')
        evidence = load('evidence-before-delete.json'); d = evidence['deployment']; final = evidence['recoverypolicies.opsloop.io']; i = evidence['recoveryincidents.opsloop.io']
        phase = i['status']['runtime']['phase']
        if phase != ('MANUAL_REQUIRED' if ident == 'image-pull' else 'OBSERVED'): raise ValueError('Unexpected terminal phase')
        frames = []
        for label, at, p, record in [('등록 상태 · Disabled (생성 시각 기준)', epoch(initial['metadata']['creationTimestamp']), initial, None),
                                     ('종료 기록 · ' + phase, epoch(evidence['at']), final, incident(i))]:
            frames.append({'label': label, 'snapshot': {'source': 'replay', 'observedAt': at,
                'cluster': {'name': '보존된 실험 기록', 'k3sVersion': '기록에 없음', 'nodes': []},
                'workload': workload(d), 'http': {'endpoint': '', 'probes': []}, 'policy': policy(p),
                'activeIncident': None, 'recentIncidents': [record] if record else []}})
        milestones = [{'label': 'Controller 장애 감지', 'at': epoch(i['spec']['detectedAt'])}]
        if ident == 'node-not-ready':
            timeline = load('experiment-timeline.json')
            milestones += [{'label': 'Azure agent stop 요청', 'at': epoch(timeline['fault_request_utc'])},
                           {'label': '운영자 start-only 복원 요청 (자동 복구 아님)', 'elapsedSeconds': timeline['restore_dispatch_elapsed']},
                           {'label': 'Node Ready + agent active 확인', 'elapsedSeconds': timeline['node_ready_agent_active_elapsed']}]
        output['experiments'].append({'id': ident, 'title': title, 'verification': 'LIVE VERIFIED / ObserveOnly',
            'limitations': ['과거 실증 기록이며 현재 클러스터 상태가 아닙니다.', '전체 과거 Node snapshot은 미포함입니다. 저장된 Controller Decision을 재평가하지 않습니다.',
                            '자동 재배치/Enforce/Engineering·PR 자동화는 미실증입니다.', '첫 frame의 시각은 Policy 생성 시각이며 상태 조회 시각은 별도 보존되지 않았습니다.'],
            'milestones': milestones, 'frames': frames,
            'provenance': [{'file': name, 'sha256': digest} for name, digest in sorted(sources.items())]})
    # Stable pseudonyms remove original object identities without changing enum,
    # measurements, score, phase or time. Neither raw files nor private paths ship.
    aliases = {}
    def sanitize(v):
        if isinstance(v, dict): return {k: sanitize(x) for k, x in v.items()}
        if isinstance(v, list): return [sanitize(x) for x in v]
        if isinstance(v, str) and re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', v):
            return aliases.setdefault(v, f'evidence-object-{len(aliases)+1}')
        return v
    return sanitize(output)

if __name__ == '__main__':
    ap = argparse.ArgumentParser(description=__doc__); ap.add_argument('--imagepull', type=pathlib.Path, required=True); ap.add_argument('--nodenotready', type=pathlib.Path, required=True); ap.add_argument('--output', type=pathlib.Path, required=True); args = ap.parse_args()
    for root in (args.imagepull, args.nodenotready):
        if args.output.resolve().is_relative_to(root.resolve()): raise SystemExit('Output cannot overwrite original evidence')
    data = json.dumps(export(args.imagepull, args.nodenotready), ensure_ascii=False, indent=2) + '\n'
    if args.output.exists() and args.output.read_text() != data: raise SystemExit('Refusing to overwrite differing replay input')
    args.output.parent.mkdir(parents=True, exist_ok=True); args.output.write_text(data)
    print('Verified, allowlisted replay derivative:', args.output)
