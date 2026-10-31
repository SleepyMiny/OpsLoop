"""Read-only v3 heartbeat progress validation; never issues execution authority.

Controller-produced progress is accepted only alongside fresh target/owner/runtime
proof. A private, run-bound observation custody prevents replacing an already
observed source across heartbeat calls or process restarts. Controller remains
responsible for healthy history, classification, and authorization of actions.
"""
import base64
import copy
import hashlib
import json
from pathlib import Path
from tools.engineering_evidence.seal import EvidenceError, policy_snapshots_equal


def conditions(obj):
    from tools.exhibition_demo_b_trial import require
    result = {}
    values = obj.get('status', {}).get('conditions', [])
    require(isinstance(values, list), 'heartbeat conditions malformed')
    for value in values:
        require(isinstance(value, dict) and isinstance(value.get('type'), str)
                and value['type'] and value['type'] not in result,
                'heartbeat duplicate/malformed condition type')
        result[value['type']] = value
    return result


def _json(raw):
    from tools.exhibition_demo_b_trial import require
    def pairs(values):
        result = {}
        for key, value in values:
            require(key not in result, 'heartbeat duplicate JSON field')
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=pairs)


def _template(value):
    result = copy.deepcopy(value)
    result.get('metadata', {}).get('labels', {}).pop('pod-template-hash', None)
    return result


def _owner(obj, kind, name, uid):
    refs = [r for r in obj.get('metadata', {}).get('ownerReferences', []) if r.get('controller') is True]
    return len(refs) == 1 and all(refs[0].get(k) == v for k, v in {'kind': kind, 'name': name, 'uid': uid}.items())


class Progress:
    def __init__(self, client, plan, payload, binding, root, envelope):
        from tools import exhibition_demo_b_trial as core
        self.c, self.p, self.payload, self.core = client, plan, payload, core
        self.scope = plan['executionScope']
        self.recipe = payload['approvedReproduction']
        self.binding = binding
        self.path = Path(root) / 'heartbeat-progress-binding.json'
        permit_bytes=base64.b64decode(envelope['payload'], validate=True)
        core.require(_json(permit_bytes) == payload, 'heartbeat permit bytes differ from validated payload')
        self.anchor = {'schema': 'opsloop-heartbeat-progress/v1', 'scope': self.scope,
                       'grantBinding': binding, 'permitSHA256': core.sha(permit_bytes)}
        self.custody = copy.deepcopy(self.anchor)
        if self.path.exists():
            core.require(not self.path.is_symlink(), 'heartbeat custody symlink')
            self.custody = _json(self.path.read_bytes())
            core.require(all(self.custody.get(k) == v for k, v in self.anchor.items()), 'heartbeat custody belongs to another authority')
        self.core.require(payload['schema'] == 'opsloop-exhibition-permit/v3', 'heartbeat progress requires explicit v3')
        for k in ('policyUID', 'deploymentUID'):
            core.require(self.recipe[k] == self.scope[k], 'heartbeat reproduction target changed')

    def pin(self, key, value):
        self.core.require(key not in self.custody or self.custody[key] == value,
                          'heartbeat bound ' + key + ' changed')
        if key not in self.custody:
            self.custody[key] = copy.deepcopy(value)
            self.core.atomic(self.path, self.custody)

    def _condition_state(self, policy):
        req = self.core.require
        cs = conditions(policy)
        req(set(cs) == {'Monitoring', 'Quiesced', 'ReadinessObserved', 'ReproductionSource'},
            'heartbeat missing or unknown condition type')
        gen = policy['metadata']['generation']
        pairs = {'Monitoring': {('True', 'Active'), ('True', 'IncidentActive'), ('False', 'REPRODUCTION_SOURCE_PENDING')},
                 'Quiesced': {('False', 'ExecutionEnabled')},
                 'ReadinessObserved': {('False', 'AwaitingHealthyPod'), ('True', 'HealthyPodObserved')},
                 'ReproductionSource': {('False', 'AwaitingApprovedSource'), ('Unknown', 'SourcePending'), ('True', 'SourceBound')}}
        for typ, v in cs.items():
            req(not set(v) - {'type', 'status', 'reason', 'message', 'observedGeneration', 'lastTransitionTime'},
                'heartbeat unknown condition field')
            req((v.get('status'), v.get('reason')) in pairs[typ], 'heartbeat unsafe ' + typ + ' state')
            req(v.get('observedGeneration', 0) == (0 if typ == 'Monitoring' else gen),
                'heartbeat stale condition generation')
            req(isinstance(v.get('lastTransitionTime'), str), 'heartbeat condition time missing')
            self.core.instant(v['lastTransitionTime'])
        messages = {
            'ReadinessObserved': {'AwaitingHealthyPod': 'Prior healthy sample for this Policy generation/template only; not execution authorization', 'HealthyPodObserved': 'Prior healthy sample for this Policy generation/template only; not execution authorization'},
            'ReproductionSource': {'AwaitingApprovedSource': 'Observing healthy baseline before reproduction; no source bound', 'SourcePending': 'Approved source runtime identity pending; no Incident or recovery action authorized', 'SourceBound': 'Live approved source identity bound; readiness classification remains independently required'},
            'Monitoring': {'Active': '', 'REPRODUCTION_SOURCE_PENDING': 'Awaiting approved source runtime identity; no Incident or recovery action authorized'}}
        for typ, expected in messages.items():
            if typ in cs and cs[typ]['reason'] in expected:
                req(cs[typ].get('message', '') == expected[cs[typ]['reason']], 'heartbeat condition meaning changed')
        req(policy.get('status', {}).get('observedGeneration') == gen, 'heartbeat status generation unconfirmed')
        return cs

    def _incident(self, policy):
        req, s = self.core.require, self.scope
        active = policy.get('status', {}).get('activeIncident')
        seq = policy.get('status', {}).get('incidentSequence', 0)
        req(seq in (self.p['initialIncidentSequence'], self.p['initialIncidentSequence'] + 1), 'heartbeat Incident budget changed')
        if not active and seq == self.p['initialIncidentSequence']:
            return None
        path = '/apis/opsloop.io/v1alpha1/namespaces/' + s['namespace'] + '/recoveryincidents/' + s['incident']
        inc = self.c.get(path, absent=True)
        if inc is None and active:
            # reserve status is committed before Create. A single bounded GET
            # may confirm that asynchronous completion; reservation alone never
            # authorizes a heartbeat or action. Original freshness still applies.
            req(active.get('name') == s['incident'] and active.get('sequence') == self.p['initialIncidentSequence'] + 1
                and active.get('workloadUID') == s['deploymentUID'] and not active.get('incidentUID'),
                'heartbeat unresolved reservation identity changed')
            inc = self.c.get(path, absent=True)
        req(isinstance(inc, dict), 'heartbeat reserved Incident unconfirmed')
        meta, spec = inc['metadata'], inc['spec']
        req(meta['name'] == s['incident'] and meta['namespace'] == s['namespace'] and meta.get('uid') and not meta.get('deletionTimestamp'), 'heartbeat Incident identity changed')
        req(spec['policyRef'] == {'name': s['policy'], 'uid': s['policyUID']} and spec['workload'] == {'name': s['deployment'], 'uid': s['deploymentUID']}, 'heartbeat Incident target changed')
        try:
            snapshot_equal = policy_snapshots_equal(spec['policy'], policy['spec'])
        except EvidenceError as exc:
            raise self.core.Stop('heartbeat Incident snapshot invalid: ' + str(exc)) from exc
        req(spec['policyGeneration'] == s['policyGeneration'] and spec['policyHash'] == s['policyHash'] and snapshot_equal and spec['sequence'] == seq == self.p['initialIncidentSequence'] + 1, 'heartbeat Incident snapshot changed')
        a = meta.get('annotations', {})
        req(a.get('opsloop.io/execution-grant-binding') == self.binding
            and a.get('opsloop.io/exhibition-run') == s['runID'] and a.get('opsloop.io/exhibition-epoch') == s['epoch']
            and a.get('opsloop.io/exhibition-permit-sha256') == self.anchor['permitSHA256'], 'heartbeat Incident authority changed')
        occurrence = _json(a['opsloop.io/current-exhibition-occurrence'])
        req(set(occurrence) == {'runID','epoch','policyUID','deploymentUID','sourcePodUID','sourcePodName','replicaSetUID','nodeUID','nodeName','containerIdentitySHA256','sourceImageID','healthyObservedAt','signature'}, 'heartbeat occurrence fields changed')
        for k in ('runID', 'epoch', 'policyUID', 'deploymentUID'):
            req(occurrence.get(k) == s[k], 'heartbeat occurrence target changed')
        trig = spec['trigger']
        for k in ('class', 'reason', 'ruleVersion'):
            req(trig[k] == self.recipe['classification'][k], 'heartbeat Incident classification changed')
        for k, target in {'podUID': 'sourcePodUID', 'podName': 'sourcePodName', 'nodeUID': 'nodeUID', 'nodeName': 'nodeName', 'signature': 'signature'}.items():
            req(trig.get(k) == occurrence.get(target) and occurrence.get(target), 'heartbeat trigger source changed')
        req(any(('containerIdentitySHA256=' + occurrence['containerIdentitySHA256']) in e.split() for e in trig.get('evidence', [])), 'heartbeat trigger runtime unconfirmed')
        req(self.core.instant(occurrence['healthyObservedAt']) <= self.core.instant(trig['observedAt']), 'heartbeat healthy history unconfirmed')
        rt = inc.get('status', {}).get('runtime', {})
        req(not rt.get('safetyHaltReason'), 'heartbeat Incident safety halt')
        req(rt.get('phase', '') in ('', 'WAITING', 'VERIFYING', 'RECOVERED', 'FAILED', 'MANUAL_REQUIRED', 'UNSUPPORTED_WORKLOAD'), 'heartbeat Incident stopped or unknown phase')
        req(len(rt.get('attempts', [])) <= 1, 'heartbeat Runtime action budget changed')
        if active:
            expected = {'name': s['incident'], 'sequence': seq, 'workloadUID': s['deploymentUID'],
                        'reservedAt': spec['detectedAt'], 'trigger': trig, 'baseline': spec['baseline']}
            if 'incidentUID' in active: expected['incidentUID'] = meta['uid']
            req(active == expected, 'heartbeat reservation differs from Incident')
        else:
            req(rt.get('completedAt') and rt.get('phase') in ('RECOVERED', 'FAILED', 'MANUAL_REQUIRED', 'UNSUPPORTED_WORKLOAD'), 'heartbeat release without terminal Incident')
        self.pin('incident', {'uid': meta['uid'], 'spec': spec, 'occurrence': occurrence})
        return inc

    def _source(self, policy, cs, inc):
        req, s = self.core.require, self.scope
        base = '/apis/apps/v1/namespaces/' + s['namespace']
        dep = self.c.get(base + '/deployments/' + s['deployment'])
        dm = dep['metadata']
        req(dm['uid'] == s['deploymentUID'] and dm['name'] == s['deployment'] and dm['namespace'] == s['namespace'] and not dm.get('deletionTimestamp'), 'heartbeat Deployment identity changed')
        fence = _json(dm.get('annotations', {}).get('opsloop.io/execution-fence', '{}'))
        req(not set(fence) - {'version','policyUID','policyName','deploymentUID','closedThroughGeneration','safetyHold'} and type(fence.get('version')) is int and fence.get('version') in (1,2), 'heartbeat fence contract unknown')
        req(fence.get('policyUID') == s['policyUID'] and fence.get('deploymentUID') == s['deploymentUID'] and fence.get('policyName') == s['policy']
            and type(fence.get('closedThroughGeneration')) is int and 0 <= fence['closedThroughGeneration'] < s['policyGeneration'] and 'safetyHold' not in fence, 'heartbeat workload fence unconfirmed')
        template = dep['spec']['template']
        baseline, fault = self.recipe['baselineTemplate'], self.recipe['template']
        if isinstance(baseline, str): baseline = _json(baseline)
        if isinstance(fault, str): fault = _json(fault)
        expected = copy.deepcopy(self.p['sourceDeploymentSpec']); expected['template'] = template
        req(dep['spec'] == expected, 'heartbeat Deployment non-template spec changed')
        stage = 'baseline' if template == baseline else 'source' if template == fault else 'action'
        source_reason = cs.get('ReproductionSource', {}).get('reason')
        if 'source' in self.custody:
            req(stage in ('source','action') and source_reason == 'SourceBound', 'heartbeat bound source regressed')
        if stage == 'action':
            req(inc is not None, 'heartbeat unapproved template')
            attempts = inc.get('status', {}).get('runtime', {}).get('attempts', [])
            req(len(attempts) == 1, 'heartbeat action template without reservation')
            at = attempts[0]; changed = copy.deepcopy(fault)
            changed.setdefault('metadata', {}).setdefault('annotations', {})['opsloop.io/attempt-id'] = at['actionID']
            changed['spec'].setdefault('nodeSelector', {})['kubernetes.io/hostname'] = at['hostname']
            req(template == changed and at['actionID'] == s['incident'] + '-a1' and at.get('outcome') in ('Reserved', 'Patched', 'Failed', 'Succeeded'), 'heartbeat action template changed')
            # The source may have been deleted by the authorized rollout. Its
            # immutable Incident occurrence remains separate from replacement.
            req('source' in self.custody, 'heartbeat action lacks original source custody')
            occurrence = self.custody['incident']['occurrence']
            req(all(occurrence.get(k) == v for k, v in self.custody['source'].items()), 'heartbeat action source occurrence changed')
            return stage
        req(not (stage == 'baseline' and source_reason in ('SourceBound', 'SourcePending')), 'heartbeat source phase/template mismatch')
        rss = self.c.listing(base + '/replicasets')['items']
        pods = self.c.listing('/api/v1/namespaces/' + s['namespace'] + '/pods')['items']
        eligible = []
        revision = dm.get('annotations', {}).get('deployment.kubernetes.io/revision')
        req(isinstance(revision, str) and revision, 'heartbeat rollout revision unknown')
        for rs in rss:
            rm = rs['metadata']
            if not _owner(rs, 'Deployment', s['deployment'], s['deploymentUID']): continue
            req(rm.get('namespace') == s['namespace'] and rm.get('uid') and not rm.get('deletionTimestamp'), 'heartbeat ReplicaSet identity missing')
            if _template(rs['spec']['template']) != template or rm.get('annotations', {}).get('deployment.kubernetes.io/revision') != revision: continue
            for pod in pods:
                pm = pod['metadata']
                if _owner(pod, 'ReplicaSet', rm['name'], rm['uid']) and not pm.get('deletionTimestamp') and pod.get('status', {}).get('phase') not in ('Succeeded', 'Failed'):
                    eligible.append((pod, rs))
        req(len(eligible) <= 1, 'heartbeat source selection ambiguous')
        if not eligible:
            req(source_reason != 'SourceBound' and 'source' not in self.custody, 'heartbeat bound source absent')
            return stage
        pod, rs = eligible[0]
        req(pod['metadata'].get('uid') and pod['metadata'].get('namespace') == s['namespace'], 'heartbeat Pod identity missing')
        req(pod['spec']['containers'] == template['spec']['containers'] and not pod['spec'].get('initContainers'), 'heartbeat Pod container template changed')
        statuses = pod.get('status', {}).get('containerStatuses', [])
        req(len(statuses) <= 1, 'heartbeat unexpected container status')
        complete = bool(pod.get('status', {}).get('phase') == 'Running' and statuses and statuses[0].get('containerID') and statuses[0].get('imageID') and 'running' in statuses[0].get('state', {}) and pod['spec'].get('nodeName'))
        if statuses:
            st = statuses[0]
            req(st['name'] == self.recipe['containerName'] and not st.get('state', {}).get('terminated'), 'heartbeat container rejected')
            req(not st.get('imageID') or st['imageID'].endswith('@' + self.recipe['workloadImage'].split('@')[1]), 'heartbeat Pod image changed')
        if not complete:
            req(source_reason != 'SourceBound' and 'source' not in self.custody, 'heartbeat bound runtime incomplete')
            return stage
        node = self.c.get('/api/v1/nodes/' + pod['spec']['nodeName'])
        req(node['metadata'].get('uid') and conditions(node).get('Ready', {}).get('status') == 'True', 'heartbeat source Node unconfirmed')
        identity = hashlib.sha256(('readiness-container-v1\0' + statuses[0]['name'] + '/' + statuses[0]['containerID'] + '/' + str(statuses[0].get('restartCount', 0))).encode()).hexdigest()
        source = {'sourcePodUID': pod['metadata']['uid'], 'sourcePodName': pod['metadata']['name'], 'replicaSetUID': rs['metadata']['uid'], 'nodeUID': node['metadata']['uid'], 'nodeName': pod['spec']['nodeName'], 'containerIdentitySHA256': identity, 'sourceImageID': statuses[0]['imageID']}
        if 'source' in self.custody:
            req(self.custody['source'] == source, 'heartbeat bound source changed')
        if stage == 'source' and source_reason == 'SourceBound':
            self.pin('source', source)
        if inc:
            occurrence = self.custody['incident']['occurrence']
            req(all(occurrence.get(k) == v for k, v in source.items()), 'heartbeat Incident/live source mismatch')
        return stage

    def terminal(self, policy):
        from tools.exhibition_terminal import terminal_proof
        return terminal_proof(self, policy)

    def observe(self, policy):
        cs = self._condition_state(policy)
        inc = self._incident(policy)
        stage = self._source(policy, cs, inc)
        monitoring = cs.get('Monitoring', {})
        active = policy.get('status', {}).get('activeIncident')
        if monitoring.get('reason') == 'IncidentActive':
            self.core.require(active is not None and inc is not None and monitoring.get('message') == self.scope['incident'], 'heartbeat Incident progress unbound')
        if monitoring.get('reason') == 'REPRODUCTION_SOURCE_PENDING':
            self.core.require(cs.get('ReproductionSource', {}).get('reason') == 'SourcePending' and not active, 'heartbeat pending state inconsistent')
        projection = copy.deepcopy(policy)
        for k in ('resourceVersion', 'managedFields'): projection['metadata'].pop(k, None)
        status = projection.setdefault('status', {})
        for k in ('conditions', 'observedGeneration', 'activeIncident', 'incidentSequence'): status.pop(k, None)
        # Historical records remain exact. Only entries proved to arise from the
        # same terminal Incident may be added; no other history is discarded.
        histories = {k: copy.deepcopy(status.get(k, [])) for k in ('circuits', 'recentFailures')}
        for field, identity in (('circuits', 'signature'), ('recentFailures', 'actionID')):
            entries = histories[field]
            self.core.require(isinstance(entries, list) and all(isinstance(x, dict) and isinstance(x.get(identity), str) and x[identity] for x in entries), 'heartbeat history malformed')
            self.core.require(len({x[identity] for x in entries}) == len(entries), 'heartbeat duplicate history identity')
            pinned = self.custody.get(field + 'Entry')
            self.core.require(pinned is None or pinned in entries, 'heartbeat preserved history removed/changed')
        if inc and inc.get('status', {}).get('runtime', {}).get('completedAt'):
            rt, spec = inc['status']['runtime'], inc['spec']
            circuits = []
            for item in status.get('circuits', []):
                if item.get('incident') != self.scope['incident']: circuits.append(item); continue
                self.core.require(set(item) == {'signature','class','incident','phase','openedAt'} and item.get('signature') == spec['trigger']['signature'] and item.get('class') == spec['trigger']['class'] and item.get('phase') == rt['phase'] and item.get('openedAt'), 'heartbeat circuit record not proved')
                self.core.require(self.core.instant(item['openedAt']) >= self.core.instant(rt['completedAt']), 'heartbeat circuit time precedes completion')
                self.pin('circuitsEntry', item)
            if 'circuits' in status: status['circuits'] = circuits
            failures = []
            for item in status.get('recentFailures', []):
                if item.get('incident') != self.scope['incident']: failures.append(item); continue
                matches = [a for a in rt.get('attempts', []) if a.get('outcome') == 'Failed' and all(a.get(ak) == item.get(ik) for ak, ik in [('actionID','actionID'),('nodeName','nodeName'),('nodeUID','nodeUID'),('failureReason','reason'),('completedAt','at')])]
                self.core.require(set(item) == {'actionID','nodeName','nodeUID','at','reason','incident'} and len(matches) == 1, 'heartbeat failure record not proved')
                self.pin('recentFailuresEntry', item)
            if 'recentFailures' in status: status['recentFailures'] = failures
        for field in ('circuits', 'recentFailures'):
            if status.get(field) == []: status.pop(field)
        return {'projection': projection, 'histories': histories, 'conditions': cs, 'stage': stage, 'sequence': policy.get('status', {}).get('incidentSequence', 0), 'active': copy.deepcopy(active)}

    def compatible(self, first, latest):
        req = self.core.require
        req(first['projection'] == latest['projection'], 'heartbeat non-progress safety state changed')
        for field in ('circuits','recentFailures'):
            req(all(item in latest['histories'][field] for item in first['histories'][field]), 'heartbeat existing history removed/changed')
        req(latest['sequence'] >= first['sequence'], 'heartbeat sequence regressed')
        # Source custody is append-only. A bound source cannot return to startup.
        ranks = {'AwaitingApprovedSource': 0, 'SourcePending': 1, 'SourceBound': 2}
        a = first['conditions'].get('ReproductionSource', {}).get('reason')
        b = latest['conditions'].get('ReproductionSource', {}).get('reason')
        req(a in ranks and b in ranks and ranks[b] >= ranks[a], 'heartbeat source progress regressed/unknown')
        req(not (first['stage'] == 'action' and latest['stage'] != 'action'), 'heartbeat action phase regressed')
        before_readiness = first['conditions'].get('ReadinessObserved', {})
        after_readiness = latest['conditions'].get('ReadinessObserved', {})
        if first['stage'] == latest['stage'] and a == b:
            req(not (before_readiness.get('status') == 'True' and after_readiness.get('status') != 'True'), 'heartbeat healthy history regressed')
        if latest['stage'] == 'action':
            req(before_readiness == after_readiness, 'heartbeat source observation changed after action')
        for typ in ('Quiesced',):
            req(first['conditions'].get(typ) == latest['conditions'].get(typ), 'heartbeat safety condition changed')
