"""Host-pinned, explicitly finite same-instance approval; never Controller wire.

These local documents are trusted only through the existing host approval/pin
boundary. An `approved` boolean is not authentication. Each listed slot still
needs its own pinned intent and exact-payload issuance approval after the actual
predecessor completion exists. No next slot is inferred or automatically run.
"""
import re
import time
from pathlib import Path
from tools.exhibition_control import identifier, instant, pinned, ControlError

INTENT = 'opsloop-circuit-replay-intent/v4'
APPROVAL = 'opsloop-finite-round-approval/v1'
CONTROLLER_FIELDS = {'deploymentUID', 'imageID', 'instance', 'podUID'}
SCOPE_FIELDS = {'clusterUID','namespaceUID','namespace','policy','policyUID','policyGeneration','policyHash','deployment','deploymentUID','incident','runID','epoch'}
SLOT_FIELDS = {'authorizationID','ordinal','baseIncidentSequence','scope','predecessor'}


def require(ok, reason):
    if not ok:
        raise ControlError(reason)


def finite_approval(intent):
    """Validate the entire finite allowlist, then select one exact slot."""
    from tools.exhibition_replay import projection_hash, verified_predecessor
    ref = intent.get('finiteRoundsApproval')
    require(isinstance(ref, dict) and set(ref) == {'path','sha256'}, 'FINITE_APPROVAL_PIN_REQUIRED')
    a = pinned(ref['path'], ref['sha256'])
    fields = {'schema','approved','purpose','controller','issuerLedger','notBefore','notAfter',
              'workloadImage','approvedReproductionSHA256','maxIncidents','maxActions','modelBudget','rounds'}
    require(set(a) == fields and a['schema'] == APPROVAL and a['approved'] is True
            and a['purpose'] == 'FINITE_SAME_INSTANCE_REPRODUCTION', 'FINITE_APPROVAL_REQUIRED')
    begin, end = instant(a['notBefore']), instant(a['notAfter'])
    require(begin <= time.time() < end and 0 < end-begin <= 21600, 'FINITE_APPROVAL_EXPIRED')
    require(begin <= instant(intent['notBefore']) < instant(intent['notAfter']) <= end, 'FINITE_INTENT_WINDOW_CHANGED')
    c = a['controller']
    require(isinstance(c, dict) and set(c) == CONTROLLER_FIELDS
            and all(identifier(c[k]) for k in CONTROLLER_FIELDS-{'imageID'})
            and isinstance(c['imageID'], str) and re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}', c['imageID']), 'FINITE_CONTROLLER_INVALID')
    require(intent['controller'] == c, 'FINITE_CONTROLLER_CHANGED')
    require(isinstance(a['issuerLedger'], str) and Path(a['issuerLedger']).is_absolute(), 'FINITE_LEDGER_REQUIRED')
    require(a['workloadImage'] == intent['workloadImage']
            and a['approvedReproductionSHA256'] == projection_hash(intent['approvedReproduction']), 'FINITE_REPRODUCTION_CHANGED')
    require(type(a['maxIncidents']) is int and type(a['maxActions']) is int
            and a['maxIncidents'] == a['maxActions'] == 1, 'FINITE_ACTION_BUDGET_CHANGED')
    require(a['modelBudget'] == {'model':'gpt-6-luna','reasoningEffort':'medium','authMode':'chatgpt',
                               'maxModelCalls':1,'totalSeconds':900}
            and type(a['modelBudget']['maxModelCalls']) is int
            and type(a['modelBudget']['totalSeconds']) is int, 'FINITE_MODEL_BUDGET_CHANGED')
    slots = a['rounds']
    require(isinstance(slots, list) and 1 <= len(slots) <= 2, 'FINITE_ROUNDS_REQUIRED')
    seen = {k:set() for k in ('authorizationID','runID','epoch','ordinal')}
    target = None
    for index, slot in enumerate(slots):
        require(isinstance(slot, dict) and set(slot) == SLOT_FIELDS, 'FINITE_SLOT_INVALID')
        scope = slot['scope']; pred = slot['predecessor']
        require(isinstance(scope, dict) and set(scope) == SCOPE_FIELDS
                and all(identifier(scope[k]) for k in SCOPE_FIELDS-{'policyGeneration','policyHash'})
                and type(scope['policyGeneration']) is int and scope['policyGeneration'] > 0
                and isinstance(scope['policyHash'], str) and re.fullmatch('[a-f0-9]{64}', scope['policyHash']), 'FINITE_SCOPE_INVALID')
        require(identifier(slot['authorizationID']) and type(slot['ordinal']) is int and 0 < slot['ordinal'] < 2**63-1
                and type(slot['baseIncidentSequence']) is int and 0 <= slot['baseIncidentSequence'] < 2**62, 'FINITE_SEQUENCE_INVALID')
        require(scope['incident'] == scope['policy']+'-'+scope['policyUID'][:8]+'-'+str(slot['baseIncidentSequence']+1).zfill(5), 'FINITE_INCIDENT_CHANGED')
        require(isinstance(pred, dict) and set(pred) == {'runID','epoch'} and all(identifier(v) for v in pred.values())
                and all(pred[k] != scope[k] for k in pred), 'FINITE_PREDECESSOR_INVALID')
        for key in seen:
            value = slot[key] if key in slot else scope[key]
            require(value not in seen[key], 'FINITE_DUPLICATE_IDENTITY')
            seen[key].add(value)
        identity = {k:scope[k] for k in ('clusterUID','namespaceUID','namespace','policy','policyUID','deployment','deploymentUID')}
        if target is None:
            target = identity
        require(identity == target, 'FINITE_TARGET_CHANGED')
        if index:
            prev = slots[index-1]
            require(slot['ordinal'] > prev['ordinal'] and slot['baseIncidentSequence'] == prev['baseIncidentSequence']+1
                    and scope['policyGeneration'] > prev['scope']['policyGeneration']
                    and pred == {k:prev['scope'][k] for k in ('runID','epoch')}, 'FINITE_ORDER_CHANGED')
    matches = [(i,s) for i,s in enumerate(slots) if s['authorizationID'] == intent['authorizationID']]
    require(len(matches) == 1, 'FINITE_ALLOWLIST_EXHAUSTED_OR_UNKNOWN')
    index, slot = matches[0]
    require(all(intent[k] == slot[k] for k in SLOT_FIELDS-{'predecessor'}), 'FINITE_SLOT_BINDING_CHANGED')
    predecessor = verified_predecessor(intent['predecessorCompletion'], intent['scope'])
    require(slot['predecessor'] == {k:predecessor[k] for k in ('runID','epoch')}, 'FINITE_PREDECESSOR_CHANGED')
    return a, index


def reserve_finite_round(db, intent, ledger_path):
    """Called inside the issuer's existing BEGIN IMMEDIATE reservation."""
    a, index = finite_approval(intent)
    require(Path(ledger_path).resolve() == Path(a['issuerLedger']).resolve(), 'FINITE_LEDGER_CHANGED')
    checksum = intent['finiteRoundsApproval']['sha256']
    consumed = db.execute('SELECT ordinal,authorization FROM finite_rounds WHERE approvalSHA256=? ORDER BY ordinal', (checksum,)).fetchall()
    expected = [(s['ordinal'],s['authorizationID']) for s in a['rounds'][:index]]
    require(consumed == expected, 'FINITE_PRIOR_SLOT_UNCONFIRMED_OR_CONSUMED')
    if index:
        prior = a['rounds'][index-1]
        row = db.execute('SELECT instance,state FROM permits WHERE authorization=?', (prior['authorizationID'],)).fetchone()
        require(row == (a['controller']['instance'], 'ISSUED'), 'FINITE_PRIOR_ISSUANCE_UNCONFIRMED')
    db.execute('INSERT INTO finite_rounds VALUES (?,?,?)', (checksum,intent['ordinal'],intent['authorizationID']))
