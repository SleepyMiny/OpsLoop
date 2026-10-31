"""Complete, GET-only exhibition readiness checks shared by planning and execution.

A saved report is never an execution permit. Missing observations are NOT_CHECKED;
independent checks continue. The only accepted historical inventory is explicitly
pinned in the plan, with byte-pinned historical identity/purpose evidence.
"""
from __future__ import annotations
import copy
import hashlib
from decimal import Decimal
from pathlib import Path
from tools.exhibition_control import ControlError
from tools.exhibition_hashes import describe, digest, parse, private_read, require, verify

SCHEMA = 'opsloop-exhibition-readiness/v1'


class CheckFailure(ControlError):
    def __init__(self, code, details):
        super().__init__(code);self.details=details


class MissingObservation(Exception):
    pass


def pointer(value, path):
    for part in path.lstrip('/').split('/') if path else []:
        part = part.replace('~1', '/').replace('~0', '~')
        value = value[int(part)] if isinstance(value, list) else value[part]
    return value


def complete_list(obj):
    require(isinstance(obj, dict) and isinstance(obj.get('items'), list)
            and not obj.get('metadata', {}).get('continue'), 'INVENTORY_INCOMPLETE')
    return obj['items']


def proof(ref):
    # Proof files may be historical public projections; they are NEVER raw hash pins.
    path = Path(ref['path'])
    require(path.is_file() and not path.is_symlink(), 'HISTORICAL_PROOF_MISSING')
    raw = path.read_bytes()
    require(hashlib.sha256(raw).hexdigest() == ref['sha256'], 'HISTORICAL_PROOF_CHANGED')
    value = pointer(parse(raw), ref['pointer'])
    require(value == ref['expected'], 'HISTORICAL_PROOF_FACT_CHANGED')
    return value


def typed_resource(value, api_version, kind):
    # Kubernetes typed-list items may omit TypeMeta. Only /spec is hashed here;
    # use the fixed endpoint type, retaining every original spec field. Never
    # use this for full-object Incident custody hashes.
    require(value.get('apiVersion',api_version)==api_version and value.get('kind',kind)==kind,'RESOURCE_TYPE_CHANGED')
    result=copy.deepcopy(value);result.update(apiVersion=api_version,kind=kind);return result


def historical_purpose(facts, job):
    m = facts['metadata']; j = job['metadata']
    require(m['namespace'] == j['namespace'] and
            (m['uid'] == j['uid'] and m['name'] == j['name'] or
             any(o.get('uid') == j['uid'] and o.get('name') == j['name'] and o.get('kind') == 'Job'
                 for o in m.get('ownerReferences', []))), 'HISTORICAL_PURPOSE_OWNER_CHANGED')
    value = m.get('labels', {}).get('opsloop.io/demo-a-observation')
    require(isinstance(value, str) and value, 'HISTORICAL_PURPOSE_MISSING')
    return value


def historical_record(job, pods, *, identity_proof, purpose_proof):
    """Explicit generator input only: no auto-discovery or adoption by Job name."""
    job=typed_resource(job,'batch/v1','Job');pods=[typed_resource(p,'v1','Pod') for p in pods]
    facts = proof(identity_proof); purpose_facts = proof(purpose_proof)
    purpose = historical_purpose(purpose_facts, job)
    m = job['metadata']
    require(facts['metadata']['namespace'] == m['namespace'] and
            any(o.get('kind') == 'Job' and o.get('name') == m['name'] and o.get('uid') == m['uid']
                for o in facts['metadata'].get('ownerReferences', [])), 'HISTORICAL_OWNER_PROOF_MISMATCH')
    label = 'opsloop.io/demo-a-observation'
    require(isinstance(purpose, str) and purpose and
            m.get('labels', {}).get(label) == purpose, 'HISTORICAL_PURPOSE_MISMATCH')
    owned = [p for p in pods if any(o.get('uid') == m['uid'] for o in p['metadata'].get('ownerReferences', []))]
    require(len(owned) == 1 and owned[0]['metadata']['uid'] == facts['metadata']['uid'], 'HISTORICAL_POD_IDENTITY_MISMATCH')
    return {'object': describe(job, '/spec'), 'purpose': purpose,
            'purposeLabel': label, 'identityProof': identity_proof, 'purposeProof': purpose_proof,
            'pods': [describe(p, '/spec') for p in owned], 'action': 'PRESERVE_NEVER_DELETE'}


def terminal_pod(pod):
    require(not pod['metadata'].get('deletionTimestamp') and pod.get('status', {}).get('phase') == 'Succeeded', 'HISTORICAL_POD_NOT_TERMINAL')
    for specs, statuses in [('containers', 'containerStatuses'), ('initContainers', 'initContainerStatuses'), ('ephemeralContainers', 'ephemeralContainerStatuses')]:
        wanted = {c['name'] for c in pod['spec'].get(specs, [])}
        actual = pod['status'].get(statuses, [])
        require(len(actual) == len(wanted) and {c['name'] for c in actual} == wanted, 'HISTORICAL_POD_STATUS_UNKNOWN')
        for c in actual:
            state = c.get('state', {}); end = state.get('terminated', {})
            require(set(state) == {'terminated'} and end.get('exitCode') == 0 and end.get('finishedAt')
                    and not c.get('ready') and c.get('started') is not True, 'HISTORICAL_POD_STATUS_UNKNOWN')
    require(bool(pod['spec'].get('containers')), 'HISTORICAL_POD_STATUS_UNKNOWN')


def historical_jobs(records, jobs, pods, forbidden_uids, reserved_runs=()):
    jobs=[typed_resource(j,'batch/v1','Job') for j in complete_list(jobs)]
    pods=[typed_resource(p,'v1','Pod') for p in complete_list(pods)]
    expected = {(r['object']['object']['namespace'], r['object']['object']['name']): r for r in records}
    require(len(expected) == len(records) and len(jobs) == len(records), 'UNEXPECTED_JOB_INVENTORY')
    protected = []
    for job in jobs:
        m = job['metadata']; record = expected.get((m['namespace'], m['name']))
        require(record is not None, 'UNEXPECTED_JOB_INVENTORY')
        verify(job, record['object'], fields='/spec')
        facts = proof(record['identityProof']); purpose = historical_purpose(proof(record['purposeProof']), job)
        require(record.get('action') == 'PRESERVE_NEVER_DELETE' and purpose == record['purpose']
                and facts['metadata']['namespace'] == m['namespace']
                and any(o.get('uid') == m['uid'] and o.get('kind') == 'Job' and o.get('name') == m['name']
                        for o in facts['metadata'].get('ownerReferences', [])), 'HISTORICAL_PROOF_MISMATCH')
        require(m.get('labels', {}).get(record['purposeLabel']) == purpose, 'HISTORICAL_PURPOSE_CHANGED')
        s = job.get('status', {})
        require(not m.get('deletionTimestamp') and not s.get('active') and not s.get('terminating')
                and not s.get('ready') and s.get('completionTime')
                and any(c.get('type') == 'Complete' and c.get('status') == 'True' for c in s.get('conditions', []))
                and not any(c.get('type') in ('Failed','FailureTarget','Suspended') and c.get('status') == 'True' for c in s.get('conditions', []))
                and not any(s.get('uncountedTerminatedPods', {}).values()), 'HISTORICAL_JOB_NOT_TERMINAL')
        owned = [p for p in pods if any(o.get('uid') == m['uid'] for o in p['metadata'].get('ownerReferences', []))]
        require({p['metadata']['uid'] for p in owned} == {p['object']['uid'] for p in record['pods']}
                and len(owned) == len(record['pods']) > 0
                and facts['metadata']['uid'] in {p['metadata']['uid'] for p in owned}, 'HISTORICAL_POD_SET_CHANGED')
        for pod in owned:
            pm = pod['metadata']; pin = next(p for p in record['pods'] if p['object']['uid'] == pm['uid'])
            verify(pod, pin, fields='/spec'); terminal_pod(pod)
            require(any(o.get('controller') is True and o.get('kind') == 'Job' and o.get('uid') == m['uid']
                        and o.get('name') == m['name'] for o in pm.get('ownerReferences', [])), 'HISTORICAL_POD_OWNER_CHANGED')
        for obj in [job, *owned]:
            meta = obj['metadata']; annotations = meta.get('annotations', {})
            require(meta['uid'] not in forbidden_uids and
                    not any(o.get('uid') in forbidden_uids for o in meta.get('ownerReferences', [])), 'HISTORICAL_ACTIVE_REFERENCE')
            require(not any(key in annotations or key in meta.get('labels', {}) for key in (
                'opsloop.io/exhibition-run', 'opsloop.io/exhibition-epoch', 'opsloop.io/exhibition-permit',
                'opsloop.io/execution-grant', 'opsloop.io/execution-grant-binding')), 'HISTORICAL_CURRENT_RUN_CONFLICT')
            require(not any(run and run in str(meta) for run in reserved_runs), 'HISTORICAL_CURRENT_RUN_CONFLICT')
            protected.append(meta['uid'])
    return {'preservedJobCount': len(jobs), 'preservedPodCount': len(protected)-len(jobs), 'protectedUIDs': protected}


def quantity(value):
    text = str(value)
    suffixes = {'Ki':1024,'Mi':1024**2,'Gi':1024**3,'Ti':1024**4,'m':Decimal('0.001'),'k':1000,'M':1000**2,'G':1000**3}
    for suffix, factor in suffixes.items():
        if text.endswith(suffix): return Decimal(text[:-len(suffix)]) * factor
    return Decimal(text)


def quota_headroom(quotas, inventory, budget):
    """Count *all* objects for count/*; pods counts only nonterminal Pods.

    status.used is also a lower bound, never subtract intended future deletions.
    Budget is a pinned upper bound, not an implicit default of zero.
    """
    rows = []; quotas = complete_list(quotas)
    require(bool(quotas), 'QUOTA_MISSING')
    for q in quotas:
        hard = q['spec']['hard']; status = q.get('status', {})
        require(hard == status.get('hard'), 'QUOTA_STATUS_UNCONFIRMED')
        require(not q['spec'].get('scopes') and not q['spec'].get('scopeSelector'), 'SCOPED_QUOTA_NOT_EVALUATED')
        for resource, limit in hard.items():
            require(resource in budget and resource in status.get('used', {}), 'QUOTA_BUDGET_OR_USAGE_MISSING')
            used = quantity(status['used'][resource]); extra = quantity(budget[resource])
            require(extra >= 0, 'QUOTA_NEGATIVE_RESERVATION')
            if resource in inventory: used = max(used, Decimal(inventory[resource]))
            rows.append({'quota':q['metadata']['name'], 'resource':resource, 'used':str(used), 'additional':str(extra), 'hard':str(quantity(limit)), 'fits':used+extra <= quantity(limit)})
    if not all(r['fits'] for r in rows): raise CheckFailure('QUOTA_HEADROOM_INSUFFICIENT',rows)
    return rows


def delete_inventory(plan):
    """Deletion is exact Incident custody only, not a discovery-based cleanup list."""
    allowed = plan['migration']['legacyObjectsAllowed']
    protected = {r['object']['object']['uid'] for r in plan['readinessContract']['historicalJobs']}
    protected |= {p['object']['uid'] for r in plan['readinessContract']['historicalJobs'] for p in r['pods']}
    require(all(o.get('kind') == 'RecoveryIncident' and o['uid'] not in protected for o in allowed), 'HISTORICAL_DELETE_FORBIDDEN')
    return copy.deepcopy(allowed)


def readiness_contract(plan):
    c = plan.get('readinessContract')
    require(isinstance(c, dict) and c.get('schema') == SCHEMA, 'READINESS_CONTRACT_REQUIRED')
    require(c.get('entrypoint') == 'python3 -m tools.exhibition_preflight --full', 'READINESS_ENTRYPOINT_CHANGED')
    require(set(c['quotaAdditional']) == {'a','b','system'}, 'QUOTA_BUDGET_REQUIRED')
    require(c.get('writesDuringPreflight') == 0 and c.get('unknownObjects') == 'BLOCK', 'READINESS_FAIL_CLOSED_REQUIRED')
    delete_inventory(plan)
    ids=[v['id'] for v in c['comparisons']+c['requiredEvidence']]
    required={'controller.instance','registry.tag','operator.permissions','host.stop','host.model-budget','tools.entrypoints',
              'nodes','schema.policy','schema.incident','controller.static-config','controller.rbac','system.quota','metrics','a.service','b.service'}
    require(len(ids)==len(set(ids)) and required<=set(ids),'READINESS_CHECK_COVERAGE_INCOMPLETE')
    return c


class Checks:
    def __init__(self, get): self.get = get; self.rows = []; self.cache = {}
    def read(self, path):
        if path not in self.cache:
            try: self.cache[path] = self.get(path)
            except MissingObservation: self.cache[path] = MissingObservation()
        value = self.cache[path]
        if isinstance(value, MissingObservation): raise value
        return value
    def check(self, name, basis, purpose, fn, *, blocks='preparation', details=False):
        row = {'id':name, 'basis':basis, 'purpose':purpose, 'blocks':blocks}
        try:
            value = fn(); row.update(status='PASS')
            if details and value is not None: row['details'] = value
        except MissingObservation:
            row.update(status='NOT_CHECKED', reason='REQUIRED_OBSERVATION_UNAVAILABLE')
        except (ControlError, KeyError, ValueError, TypeError, IndexError, OSError) as exc:
            # Never reflect arbitrary source values, command stderr or credentials.
            code = str(exc) if isinstance(exc, ControlError) else 'INVALID_'+type(exc).__name__.upper()
            row.update(status='FAIL', reason=code if code.replace('_','').isalnum() else 'CHECK_REJECTED')
            if isinstance(exc,CheckFailure): row['details']=exc.details
        self.rows.append(row)


def full_preflight(plan, get, *, mode='saved'):
    from tools.exhibition_preflight import contract_pins, object_path, preflight, verify_sources
    require(mode in ('saved','live'), 'PREFLIGHT_MODE_INVALID')
    checks = Checks(get); c = readiness_contract(plan); pins = contract_pins(plan)
    read = checks.read
    checks.check('candidate.sources','/candidate','Exact local source, image and verification pins',lambda: verify_sources(plan))
    # Each raw target is checked independently, then the existing combined contract.
    for key, pin in pins.items():
        checks.check('raw.'+key, '/objectHashContract/objects/'+key, 'Original representation, full field scope and exact UID',
                     lambda pin=pin: verify(read(object_path(pin['object'])), pin, fields=pin['fields']))
    checks.check('integrity.binding', '/objectHashContract', 'Same common hash, generation, fence, baseline derivation and custody gate',
                 lambda: preflight(plan, read))
    checks.check('cluster', '/clusterUID', 'Reject another cluster',
                 lambda: require(read('/api/v1/namespaces/kube-system')['metadata']['uid'] == plan['clusterUID'], 'CLUSTER_CHANGED'))
    legacy_uids = {o['uid'] for o in plan['migration']['legacyObjectsAllowed']}
    for key, t in plan['fixedTargets'].items():
        ns = t['namespace']; core = '/api/v1/namespaces/'+ns
        ops = '/apis/opsloop.io/v1alpha1/namespaces/'+ns
        dep_path = object_path(pins[key+'-deployment']['object'])
        pol_path = object_path(pins[key+'-policy']['object'])
        paths = {'jobs':'/apis/batch/v1/namespaces/'+ns+'/jobs?limit=100', 'pods':core+'/pods?limit=100',
                 'replicasets':'/apis/apps/v1/namespaces/'+ns+'/replicasets?limit=100'}
        def policy_count(t=t, ops=ops):
            items = complete_list(read(ops+'/recoverypolicies?limit=100'))
            require(len(items) == 1 and items[0]['metadata']['uid'] == t['policyUID'], 'POLICY_COUNT_OR_UID_CHANGED')
        def fence(pol_path=pol_path, t=t):
            p = read(pol_path); m = p['metadata']; s = p.get('status', {})
            require(not p['spec']['enabled'] and not s.get('activeIncident') and not m.get('deletionTimestamp') and
                m['uid'] == t['policyUID'] and m['generation'] == t['observedPolicyGeneration'] and
                s.get('incidentSequence',0) == t['observedIncidentSequence'] and any(
                    q.get('type')=='Quiesced' and q.get('status')=='True' and q.get('reason')=='FenceClosed' and
                    q.get('observedGeneration')==m['generation'] for q in s.get('conditions', [])), 'POLICY_FENCE_UNCONFIRMED')
        def ready(dep_path=dep_path):
            d=read(dep_path); s=d.get('status', {}); n=d['spec'].get('replicas',1)
            require(n == 1 and s.get('observedGeneration') == d['metadata']['generation'] and
                    s.get('readyReplicas',0)==n and s.get('updatedReplicas',0)==n and s.get('availableReplicas',0)==n
                    and not d['metadata'].get('deletionTimestamp'), 'BASELINE_NOT_READY')
        def custody(ops=ops, ns=ns, paths=paths):
            items=complete_list(read(ops+'/recoveryincidents?limit=100'))
            expected=[o for o in plan['migration']['legacyObjectsAllowed'] if o['namespace']==ns]
            require({v['metadata']['uid'] for v in items}=={v['uid'] for v in expected} and len(items)==len(expected), 'INCIDENT_INVENTORY_CHANGED')
            for desc in expected:
                inc=read(object_path(desc['hash']['object'])); verify(inc,desc['hash'],fields='/')
                s=inc.get('status',{}).get('runtime',{});m=inc['metadata']
                require(s.get('phase') in ('RECOVERED','FAILED') and s.get('completedAt') and not s.get('safetyHaltReason')
                        and not s.get('oldPodsOutstanding') and not m.get('finalizers') and not m.get('deletionTimestamp'), 'INCIDENT_NOT_TERMINAL')
            for path in paths.values():
                require(not any(any(o.get('uid') in legacy_uids for o in v['metadata'].get('ownerReferences',[])) for v in complete_list(read(path))), 'INCIDENT_ACTIVE_REFERENCE')
        def quota(key=key, core=core, paths=paths, ops=ops):
            jobs=complete_list(read(paths['jobs'])); pods=complete_list(read(paths['pods'])); rs=complete_list(read(paths['replicasets']))
            inventory={'count/jobs.batch':len(jobs),'count/pods':len(pods), 'pods':sum(p.get('status',{}).get('phase') not in ('Succeeded','Failed') for p in pods),
                'count/replicasets.apps':len(rs), 'count/recoverypolicies.opsloop.io':len(complete_list(read(ops+'/recoverypolicies?limit=100'))),
                'count/recoveryincidents.opsloop.io':len(complete_list(read(ops+'/recoveryincidents?limit=100')))}
            qs=read(core+'/resourcequotas?limit=100')
            require(any(q['spec']['hard'].get('count/recoverypolicies.opsloop.io')=='1' and q.get('status',{}).get('used',{}).get('count/recoverypolicies.opsloop.io')=='1' for q in complete_list(qs)), 'POLICY_QUOTA_ONE_REQUIRED')
            return quota_headroom(qs,inventory,c['quotaAdditional'][key])
        checks.check(key+'.namespace','/fixedTargets/'+key+'/namespaceUID','Namespace identity',lambda core=core,t=t: require(read(core)['metadata']['uid']==t['namespaceUID'],'NAMESPACE_CHANGED'))
        checks.check(key+'.policy-count','/fixedTargets/'+key+'/quotaPolicyCount','Fixed Policy count and UID',policy_count)
        checks.check(key+'.fence','/preflight/hardStop','Disabled, current-generation FenceClosed and cumulative history',fence)
        checks.check(key+'.baseline','/trials/'+key.upper()+'/success','Actual Ready baseline before Enforce; a planned restore is not a PASS',ready,blocks='Enforce')
        checks.check(key+'.custody','/migration/legacyObjectsAllowed','Terminal Incident, exact hash/UID and no cascade reference',custody)
        records=[r for r in c['historicalJobs'] if r['object']['object']['namespace']==ns]
        checks.check(key+'.historical-jobs','/readinessContract/historicalJobs','Proven history plus all owned Pods terminated; preserve objects',
                     lambda records=records, paths=paths: historical_jobs(records,read(paths['jobs']),read(paths['pods']),legacy_uids,c.get('reservedRuns',[])), details=True)
        checks.check(key+'.quota','/readinessContract/quotaAdditional/'+key,'Existing usage including retained objects plus pinned peak additions',quota, details=True)
    checks.check('a.cpu-floor','/trials/A/resourceConditions','900 permille must actually exceed fixed CPU exclusion threshold',
        lambda: require(900 > read(object_path(pins['a-policy']['object']))['spec']['filter']['maxCPUUtilizationPercent']*10,'CPU_FLOOR_NOT_EXCLUDING'))
    # All auxiliary comparisons are exact contracts fixed before approval; no
    # caller-supplied PASS flags. Logs/host stop checks are separate typed evidence.
    for spec in c['comparisons']:
        def compare(spec=spec):
            value=read(spec['path'])
            for item in spec['equals']:
                require(pointer(value,item['pointer'])==item['value'],'PINNED_STATE_CHANGED')
        checks.check(spec['id'],spec['basis'],spec['purpose'],compare)
    for item in c['requiredEvidence']:
        checks.check(item['id'],item['basis'],item['purpose'],lambda item=item: auxiliary_check(item,read,plan))
    statuses=[r['status'] for r in checks.rows]
    return {'schema':SCHEMA,'state':'LOCAL_CHECKS_PASS' if all(s=='PASS' for s in statuses) and mode=='saved' else
            ('LIVE_CHECKS_PASS' if all(s=='PASS' for s in statuses) else 'NO_GO'),
            'scope':'SAVED_RESPONSE_LOCAL_ONLY' if mode=='saved' else 'CURRENT_GET_ONLY',
            'checks':checks.rows,'mutationRequests':0,'executionAuthorized':False}


def saved_responses(path, checksum):
    catalog = private_read(path, checksum)
    require(catalog.get('schema') == 'opsloop-readonly-responses/v1' and catalog.get('mutationRequests') == 0, 'READONLY_CAPTURE_REQUIRED')
    rows = catalog['records']; require(len({r['path'] for r in rows}) == len(rows), 'DUPLICATE_RESPONSE')
    by_path = {r['path']:r for r in rows}
    def get(path):
        if path not in by_path: raise MissingObservation()
        r=by_path[path]
        require(r.get('method')=='GET' and r.get('representation')=='kubernetes-raw' and r.get('observedAt'), 'RAW_RESPONSE_REQUIRED')
        value=private_read(r['source']['path'],r['source']['sha256'])
        return pointer(value,r.get('pointer',''))
    return get


def auxiliary_check(item, read, plan):
    """No user-supplied `passed` flag can satisfy any of these checks."""
    kind=item['kind']
    if kind=='nodes':
        nodes=complete_list(read(item['path']))
        expected={n['name']:n for n in item['nodes']}
        require({n['metadata']['name'] for n in nodes}==set(expected),'NODE_SET_CHANGED')
        for n in nodes:
            e=expected[n['metadata']['name']];conditions={c['type']:c['status'] for c in n.get('status',{}).get('conditions',[])}
            require(n['metadata']['uid']==e['uid'] and n.get('status',{}).get('nodeInfo',{}).get('architecture')==e['architecture']
                    and conditions.get('Ready')=='True' and all(conditions.get(k)=='False' for k in ('MemoryPressure','DiskPressure','PIDPressure'))
                    and not n['metadata'].get('deletionTimestamp'),'NODE_UNSAFE_OR_CHANGED')
    elif kind=='controller-instance':
        pods=complete_list(read(item['pods'])); replicas=complete_list(read(item['replicasets']))
        dep_uid=plan['deployment']['controller']['uid']
        rs={r['metadata']['uid'] for r in replicas if any(o.get('uid')==dep_uid and o.get('kind')=='Deployment' and o.get('controller') is True for o in r['metadata'].get('ownerReferences',[]))}
        owned=[p for p in pods if any(o.get('uid') in rs and o.get('kind')=='ReplicaSet' and o.get('controller') is True for o in p['metadata'].get('ownerReferences',[]))]
        require(len(owned)==1,'CONTROLLER_POD_AMBIGUOUS'); p=owned[0]; m=p['metadata']
        require(not m.get('deletionTimestamp') and p.get('status',{}).get('phase')=='Running' and
                any(q.get('type')=='Ready' and q.get('status')=='True' for q in p['status'].get('conditions',[])), 'CONTROLLER_NOT_READY')
        log=read('/api/v1/namespaces/'+m['namespace']+'/pods/'+m['name']+'/log?container=manager&limitBytes=65536')
        require(isinstance(log,str),'CONTROLLER_LOG_REQUIRED')
        identities=[]
        for line in log.splitlines():
            try: value=parse(line)
            except ControlError: continue
            if isinstance(value,dict) and value.get('schema')=='opsloop-controller-execution-identity/v1': identities.append(value)
        require(bool(identities) and {v.get('instance') for v in identities}=={item['expectedInstance']} and
                all(v.get('enforceDefault')=='deny-without-fresh-bound-grant' for v in identities), 'CONTROLLER_INSTANCE_CHANGED')
    elif kind=='registry-absence':
        v=read(item['path']); publication=plan['deployment']['imagePublication']
        require(v.get('method')=='HEAD' and v.get('status')==404 and v.get('repository')==publication['repository']
                and v.get('tag')==publication['newTag'],'REGISTRY_ABSENCE_UNCONFIRMED')
    elif kind=='exact-evidence':
        v=read(item['path'])
        require(bool(item.get('equals')),'EMPTY_EVIDENCE_CHECK')
        for condition in item['equals']:
            require(pointer(v,condition['pointer'])==condition['value'],'REQUIRED_EVIDENCE_CHANGED')
    elif kind=='local-entrypoints':
        verify_entrypoints(item['manifest'])
    else:
        require(False,'UNKNOWN_CHECK_KIND')


def verify_entrypoints(ref):
    import ast
    m=private_read(ref['path'],ref['sha256'])
    required={'preflight','archive','migration','publish','deploy','run','cleanup','restore'}
    require(m.get('schema')=='opsloop-execution-entrypoints/v1' and set(m['stages'])==required, 'EXECUTION_ENTRYPOINTS_INCOMPLETE')
    for stage,entry in m['stages'].items():
        require(entry.get('ready') is True,'EXECUTION_ENTRYPOINT_NOT_READY')
        path=Path(entry['path']);raw=path.read_bytes()
        require(not path.is_symlink() and hashlib.sha256(raw).hexdigest()==entry['sha256'],'EXECUTION_ENTRYPOINT_CHANGED')
        tree=ast.parse(raw)
        names={node.name for node in tree.body if isinstance(node,(ast.FunctionDef,ast.ClassDef))}
        require(entry['symbol'].split('.')[0] in names,'EXECUTION_SYMBOL_MISSING')
    for ref in m['dependencies']:
        require(hashlib.sha256(Path(ref['path']).read_bytes()).hexdigest()==ref['sha256'],'EXECUTION_DEPENDENCY_CHANGED')
    return m
