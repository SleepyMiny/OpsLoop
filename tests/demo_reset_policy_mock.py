"""Local-only API model for reset tests: atomic JSON Patch UID/RV preconditions."""
import json
import os
from pathlib import Path
import sys

root = Path(os.environ['RUN_DIR'])
failures = set(os.environ['FAIL'].split(','))
args = sys.argv[1:]
state_path = root / 'policy-state.json'
state = json.loads(state_path.read_text()) if state_path.exists() else {
    'metadata': {'uid': 'other' if 'replaced' in failures else 'uid', 'resourceVersion': '1'},
    'spec': {'enabled': True},
    'status': {'circuits': ['original'], 'recentFailures': ['original'], 'activeIncident': None},
}
if 'active' in failures:
    state['status']['activeIncident'] = {'name': 'incident'}


def save():
    state_path.write_text(json.dumps(state))
    (root / 'enabled').write_text(str(state['spec']['enabled']).lower())


def lookup(path):
    value = state
    for part in path.strip('/').split('/'):
        value = value[part]
    return value


save()
if args[0] == 'get':
    if 'get_policy' in failures or 'deleted' in failures:
        sys.exit(1)
    enabled = str(state['spec']['enabled']).lower()
    if 'disable_unconfirmed' in failures and enabled == 'false':
        enabled = 'true'
    if 'enable_read' in failures and enabled == 'true' and (root/'enabled-once').exists():
        sys.exit(1)
    fields = [state['metadata']['uid']]
    template = args[-1]
    if 'resourceVersion' in template:
        fields.append('' if 'missing_rv' in failures else state['metadata']['resourceVersion'])
    fields.append(enabled)
    if 'activeIncident' in template:
        fields.append('incident' if state['status']['activeIncident'] else '')
    if 'deletionTimestamp' in template:
        fields.append('2026-10-18T00:00:00Z' if 'deleting' in failures else '')
    print('|'.join(fields), end='')
    sys.exit(0)

assert args[0] == 'patch' and '--type=json' in args, 'unsafe Policy patch'
patch = json.loads(args[args.index('-p') + 1])
assert any(p['op'] == 'test' and p['path'] == '/metadata/uid' for p in patch)
assert any(p['op'] == 'test' and p['path'] == '/metadata/resourceVersion' for p in patch)
action = 'ledger' if '--subresource=status' in args else (
    'enable' if patch[-1]['value'] is True else 'disable')
marker = root / ('injected-' + action)
if not marker.exists():
    if ('replace_' + action) in failures:
        state['metadata']['uid'] = 'replacement'
    if ('conflict_' + action) in failures:
        state['metadata']['resourceVersion'] = str(int(state['metadata']['resourceVersion']) + 1)
    if ('active_' + action) in failures:
        state['status']['activeIncident'] = {'name': 'new-incident'}
        state['metadata']['resourceVersion'] = str(int(state['metadata']['resourceVersion']) + 1)
    marker.touch()
save()
if (action == 'disable' and 'disable' in failures) or (action == 'ledger' and 'ledger' in failures):
    sys.exit(1)
# Validate ALL preconditions before applying any operation.
if any(p['op'] == 'test' and lookup(p['path']) != p['value'] for p in patch):
    with (root/'rejected-patches').open('a') as f:
        f.write(action + '\n')
    sys.exit(1)
for operation in patch:
    if operation['op'] != 'test':
        parent, key = operation['path'].strip('/').split('/')
        state[parent][key] = operation['value']
state['metadata']['resourceVersion'] = str(int(state['metadata']['resourceVersion']) + 1)
save()
with (root/'accepted-patches').open('a') as f:
    f.write(action + ':' + state['metadata']['uid'] + '\n')
if action == 'enable':
    (root/'enabled-once').touch()
    if 'enable' in failures:
        sys.exit(1)  # remotely applied, transport result ambiguous
