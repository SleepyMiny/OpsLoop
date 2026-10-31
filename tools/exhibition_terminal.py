"""A same-run Runtime terminal closes renewal; it never grants execution authority."""
import copy


class TerminalObserved(Exception):
    def __init__(self, proof):
        super().__init__('same-run Runtime terminal verified; stop-only capture required')
        self.proof = proof


def terminal_proof(progress, policy):
    """Validate the terminal/action against durable source and Incident custody.

    The narrowly recognized consumed display state is usable only for closure.
    Ordinary heartbeat Progress.observe still rejects that state unchanged.
    """
    core, s = progress.core, progress.scope
    req = core.require
    before = copy.deepcopy(progress.custody)
    inc = progress._incident(policy)
    if inc is None:
        return None
    rt = inc.get('status', {}).get('runtime', {})
    if rt.get('phase') in ('', 'WAITING', 'VERIFYING'):
        return None
    req(rt.get('phase') == 'FAILED' and rt.get('reason') == 'RUNTIME_BUDGET_EXHAUSTED'
        and rt.get('completedAt') and not rt.get('safetyHaltReason'), 'terminal is not exhausted application failure')
    req('source' in before and 'incident' in before and before['incident'] == progress.custody['incident'],
        'terminal lacks prior exact source/Incident custody')
    attempts = rt.get('attempts', [])
    req(len(attempts) == 1, 'terminal action budget changed')
    at = attempts[0]
    req(at.get('actionID') == s['incident']+'-a1' and at.get('outcome') == 'Failed'
        and at.get('podUID') and at.get('podUID') != before['source']['sourcePodUID']
        and at.get('podName') and at.get('patchedAt') and at.get('completedAt'), 'terminal applied failed replacement missing')
    decision = at.get('decision', {})
    req(decision.get('actionID') == at['actionID'] and decision.get('workloadUID') == s['deploymentUID']
        and decision.get('policyHash') == s['policyHash'] and decision.get('policyGeneration') == s['policyGeneration'],
        'terminal Runtime decision changed')
    grant = progress.payload['grant']
    req(core.instant(grant['notBefore']) <= core.instant(at['reservedAt']) <= core.instant(at['patchedAt'])
        <= core.instant(at['completedAt']) <= core.instant(rt['completedAt']) < core.instant(grant['notAfter']),
        'terminal action chronology outside grant')
    from tools.exhibition_heartbeat_progress import conditions, _json, _owner, _template
    cs = conditions(policy)
    projected = copy.deepcopy(policy)
    consumed = cs.get('Monitoring', {}).get('reason') == 'EXECUTION_AUTHORITY_UNAVAILABLE' or cs.get('ReproductionSource', {}).get('reason') == 'SourceRejected'
    if consumed:
        m, source = cs.get('Monitoring', {}), cs.get('ReproductionSource', {})
        req(not policy.get('status', {}).get('activeIncident') and
            m.get('status') == 'False' and m.get('reason') == 'EXECUTION_AUTHORITY_UNAVAILABLE' and
            m.get('message') == 'execution authority: single incident already consumed' and
            source.get('status') == 'False' and source.get('reason') == 'SourceRejected' and
            source.get('message') == 'Source authorization unavailable; execution blocked',
            'terminal authority loss is not proved Incident consumption')
        # Closing proof only, never fed to a heartbeat PATCH or written to API.
        for c in projected['status']['conditions']:
            if c['type'] == 'Monitoring':c.update(status='True', reason='Active', message='')
            if c['type'] == 'ReproductionSource':c.update(status='True', reason='SourceBound', message='Live approved source identity bound; readiness classification remains independently required')
    observation = progress.observe(projected)
    req(observation['stage'] == 'action', 'terminal lacks approved action template')
    replacement_identity=verify_sources(core.require,progress.c,s,progress.recipe,before['source'],at)
    progress.pin('terminalReplacement',replacement_identity)
    return {'policy':copy.deepcopy(policy),'incident':copy.deepcopy(inc),'observation':observation,'replacement':replacement_identity}


def verify_sources(require,client,s,recipe,source,at):
    """Read-only source/replacement custody, usable after the owned Policy stops."""
    req=require
    src = source
    from tools.exhibition_heartbeat_progress import _json, _owner, _template
    original = client.get('/api/v1/namespaces/'+s['namespace']+'/pods/'+src['sourcePodName'], absent=True)
    if original is not None:
        fault=recipe['template'];fault=_json(fault) if isinstance(fault,str) else fault
        req(original.get('spec',{}).get('containers')==fault['spec']['containers'] and not original.get('spec',{}).get('initContainers'), 'terminal original container changed')
        req(original['metadata'].get('uid') == src['sourcePodUID'] and original['metadata'].get('namespace') == s['namespace'], 'terminal original source replaced')
        owners=[x for x in original['metadata'].get('ownerReferences',[]) if x.get('controller')]
        req(len(owners)==1 and owners[0].get('kind')=='ReplicaSet' and owners[0].get('uid')==src['replicaSetUID'], 'terminal original owner changed')
        statuses=original.get('status',{}).get('containerStatuses',[])
        req(len(statuses)==1 and statuses[0].get('name')==recipe['containerName'], 'terminal original runtime absent')
        import hashlib
        st=statuses[0]
        identity=hashlib.sha256(('readiness-container-v1\0'+st['name']+'/'+st.get('containerID','')+'/'+str(st.get('restartCount',0))).encode()).hexdigest()
        req(identity==src['containerIdentitySHA256'] and st.get('imageID')==src['sourceImageID'], 'terminal original runtime changed')
    replacement=client.get('/api/v1/namespaces/'+s['namespace']+'/pods/'+at['podName'])
    pm=replacement.get('metadata',{})
    req(pm.get('uid')==at['podUID'] and pm.get('namespace')==s['namespace'] and not pm.get('deletionTimestamp')
        and pm.get('annotations',{}).get('opsloop.io/attempt-id')==at['actionID'], 'terminal replacement identity changed')
    owners=[x for x in pm.get('ownerReferences',[]) if x.get('controller')]
    req(len(owners)==1 and owners[0].get('kind')=='ReplicaSet', 'terminal replacement owner ambiguous')
    owner=owners[0];rs=client.get('/apis/apps/v1/namespaces/'+s['namespace']+'/replicasets/'+owner['name'])
    req(rs['metadata'].get('namespace')==s['namespace'] and not rs['metadata'].get('deletionTimestamp') and rs['metadata'].get('uid')==owner['uid'] and _owner(rs,'Deployment',s['deployment'],s['deploymentUID']), 'terminal replacement owner changed')
    fault=recipe['template'];fault=_json(fault) if isinstance(fault,str) else fault
    expected=copy.deepcopy(fault);expected.setdefault('metadata',{}).setdefault('annotations',{})['opsloop.io/attempt-id']=at['actionID'];expected['spec'].setdefault('nodeSelector',{})['kubernetes.io/hostname']=at['hostname']
    req(_template(rs['spec']['template'])==expected and replacement['spec']['containers']==expected['spec']['containers']
        and not replacement['spec'].get('initContainers') and replacement['spec'].get('nodeName')==at['nodeName'], 'terminal replacement scope changed')
    statuses=replacement.get('status',{}).get('containerStatuses',[])
    req(replacement.get('status',{}).get('phase')=='Running' and len(statuses)==1,'terminal replacement runtime unconfirmed')
    st=statuses[0]
    req(st.get('name')==recipe['containerName'] and st.get('containerID') and 'running' in st.get('state',{})
        and st.get('imageID','').endswith('@'+recipe['workloadImage'].split('@')[1]), 'terminal replacement image/runtime changed')
    replacement_identity={'podUID':pm['uid'],'replicaSetUID':owner['uid'],'containerID':st['containerID'],'imageID':st['imageID'],'restartCount':st.get('restartCount',0)}
    return replacement_identity
