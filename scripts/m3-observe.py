#!/usr/bin/env python3
"""Bounded current-cluster GET receipt. No raw configuration or Secret data is saved."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import subprocess


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--kubectl', default='kubectl')
    p.add_argument('--kubeconfig', required=True)
    p.add_argument('--output', type=Path, required=True)
    a = p.parse_args()
    result = {'schema': 'opsloop-m3-live-observation/v1', 'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'method': 'GET', 'clusterWrites': 0, 'resources': {}, 'errors': []}
    specs = [('nodes', []), ('recoverypolicies.opsloop.io', ['-A']), ('recoveryincidents.opsloop.io', ['-A']), ('deployments', ['-A']), ('pods', ['-A'])]
    for kind, scope in specs:
        r = subprocess.run([a.kubectl, '--kubeconfig', a.kubeconfig, '--request-timeout=5s', 'get', kind, *scope, '-o', 'json'], capture_output=True, timeout=20)
        if r.returncode:
            result['errors'].append({'resource': kind, 'status': 'DISCONNECTED', 'exitCode': r.returncode})
            continue
        records = []
        for obj in json.loads(r.stdout)['items']:
            m, s, status = obj['metadata'], obj.get('spec', {}), obj.get('status', {})
            if kind != 'nodes' and m.get('namespace') not in ['opsloop-demo-a', 'opsloop-demo-b', 'opsloop-demo', 'opsloop-system', 'monitoring']:
                continue
            item = {k: m.get(k) for k in ['name', 'namespace', 'uid', 'generation', 'resourceVersion']}
            if kind == 'nodes':
                item.update(arch=status.get('nodeInfo', {}).get('architecture'), readiness=next((c['status'] for c in status.get('conditions', []) if c['type'] == 'Ready'), 'Unknown'), capacity=status.get('allocatable', {}))
            elif kind.startswith('recoverypolicies'):
                item.update(enabled=s.get('enabled'), mode=s.get('mode'), observedGeneration=status.get('observedGeneration'), activeIncident=status.get('activeIncident'), guard='UNKNOWN_CURRENT_PROCESS_STATE')
                item['conditions'] = [{k: c.get(k) for k in ['type', 'status', 'reason', 'observedGeneration', 'lastTransitionTime']} for c in status.get('conditions', [])]
            elif kind.startswith('recoveryincidents'):
                item.update(policyUID=s.get('policyRef', {}).get('uid'), workloadUID=s.get('workload', {}).get('uid'), phase=status.get('runtime', {}).get('phase'), runtimeReason=status.get('runtime', {}).get('reason'), engineeringPhase=status.get('engineering', {}).get('phase'))
            elif kind == 'deployments':
                item.update(replicas=s.get('replicas'), readyReplicas=status.get('readyReplicas', 0), availableReplicas=status.get('availableReplicas', 0), images=[c.get('image') for c in s.get('template', {}).get('spec', {}).get('containers', [])], specSHA256=hashlib.sha256(json.dumps(s, sort_keys=True, separators=(',', ':')).encode()).hexdigest())
                raw_fence = m.get('annotations', {}).get('opsloop.io/execution-fence')
                if raw_fence:
                    try:
                        fence = json.loads(raw_fence)
                        item['executionFence'] = {k: fence.get(k) for k in ['version', 'policyUID', 'policyName', 'deploymentUID', 'closedThroughGeneration']}
                        item['executionFenceSHA256'] = hashlib.sha256(raw_fence.encode()).hexdigest()
                    except (ValueError, AttributeError):
                        item['executionFence'] = {'status': 'INVALID'}
            elif kind == 'pods':
                item.update(node=s.get('nodeName'), phase=status.get('phase'), ready=next((c['status'] for c in status.get('conditions', []) if c['type'] == 'Ready'), 'Unknown'), owners=[{'kind': o.get('kind'), 'uid': o.get('uid')} for o in m.get('ownerReferences', [])])
            records.append(item)
        result['resources'][kind] = records
    result['status'] = 'PASS' if not result['errors'] else 'PARTIAL'
    a.output.parent.mkdir(parents=True, exist_ok=True)
    with a.output.open('x') as f:
        json.dump(result, f, indent=2)
        f.write('\n')
    print(json.dumps({'status': result['status'], 'output': str(a.output), 'counts': {k: len(v) for k, v in result['resources'].items()}}))


if __name__ == '__main__':
    main()
