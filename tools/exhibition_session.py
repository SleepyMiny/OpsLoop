"""Host entrypoint for one already sealed exhibition plan and cleanup-only reentry.

No permit issuance, plan generation, retries, Publisher or cluster setup here.
Uses the same durable Coordinator and adapters as the Dashboard.
"""
import argparse
import json
import time
from pathlib import Path
from tools.exhibition_control import Coordinator, pinned, validate_plan, instant
from tools.exhibition_runtime import RuntimeAdapter
from tools.exhibition_engineering import EngineeringAdapter
from tools.exhibition_hashes import require


def run_once(config_ref, state, plan_id, request_id, *, stop_at, reconcile_run=None):
    cfg=pinned(config_ref['path'],config_ref['sha256'])
    require(time.time()<instant(stop_at),'SESSION_EXPIRED')
    coordinator=Coordinator(Path(state),cfg,{'runtime':RuntimeAdapter(),'engineering':EngineeringAdapter()})
    started=None
    try:
        if reconcile_run is not None:
            coordinator.reconcile(reconcile_run)
            # Reconciliation is asynchronous. Return the durable final result,
            # never its initial RECONCILING acknowledgement.
            while time.time()<instant(stop_at):
                current=next((r for r in coordinator._runs() if r['runId']==reconcile_run),None)
                require(current is not None,'RUN_LEDGER_MISSING')
                if coordinator.worker is None or not coordinator.worker.is_alive():
                    return current
                time.sleep(.25)
            raise RuntimeError('SESSION_EXPIRED_CLEANUP_REQUIRED')
        # Prepare is local validation; start's durable claim prevents duplicate
        # fault/model execution even on identical request or previous run binding.
        plan=coordinator._plan(plan_id)
        validate_plan(plan)
        from tools.exhibition_permit import bound_permit
        bound_permit(plan,model=plan['track']=='engineering')
        require(instant(plan['expiresAt'])+180<=instant(stop_at),'RUN_EXCEEDS_SESSION')
        coordinator.prepare(plan_id)
        started=coordinator.start(plan_id,request_id)
        run_id=started['runId']
        while time.time()<instant(stop_at):
            current=next((r for r in coordinator._runs() if r['runId']==run_id),None)
            require(current is not None,'RUN_LEDGER_MISSING')
            if coordinator.worker is None or not coordinator.worker.is_alive():
                return current
            time.sleep(.25)
        coordinator.stop(run_id)
        raise RuntimeError('SESSION_EXPIRED_CLEANUP_REQUIRED')
    except BaseException:
        if started is not None:
            # Cancels only this consumed run; never restarts or creates another.
            coordinator.stop(started['runId'])
        raise
    finally:
        coordinator.close()


def outcome_summary(run):
    """Process exit, original trial result and later cleanup are separate facts."""
    execution=run.get('executionState',run.get('state'))
    return {'runId':run.get('runId'),'planId':run.get('planId'),'state':run.get('state'),
            'executionState':execution,'stopConfirmed':run.get('stopConfirmed') is True,
            'lifecycleComplete':run.get('lifecycleComplete') is True,
            'trialSucceeded':execution=='COMPLETED' and run.get('state')=='COMPLETED'
                and run.get('stopConfirmed') is True and run.get('lifecycleComplete') is True,
            'nextRunIsAuthorized':False}


def run_in_process(config_ref, state, plan_id, request_id, *, stop_at, output_root, total_seconds):
    """A round owns its own coordinator PID; terminal child exit precedes host_idle.

    Preserve the existing live-owner check. No model, fault, grant or cleanup
    request is retried here; a failed child is handled by cleanup-only reentry.
    """
    import os,subprocess,sqlite3,sys
    from tools.exhibition_lifecycle import private_dir
    from tools.exhibition_inputs import new
    remaining=instant(stop_at)-time.time()
    require(total_seconds in (300,900) and remaining>=total_seconds+180,'CHILD_ROUND_WINDOW_INSUFFICIENT')
    root=private_dir(str(output_root))
    args=[sys.executable,'-m','tools.exhibition_session','--config',config_ref['path'],'--sha256',config_ref['sha256'],
          '--state',str(state),'--stop-at',stop_at,'--run-plan',plan_id,'--request-id',request_id,'--approve-execution']
    new(root,'child-dispatch.json',{'planId':plan_id,'requestId':request_id,'config':config_ref,'at':time.time(),'dispatchesMaximum':1})
    with (root/'child.stdout').open('xb') as out,(root/'child.stderr').open('xb') as err:
        os.chmod(out.name,0o600);os.chmod(err.name,0o600)
        try:
            result=subprocess.run(args,stdout=out,stderr=err,timeout=min(remaining,total_seconds+180))
        except BaseException as exc:
            new(root,'child-result.json',{'planId':plan_id,'errorType':type(exc).__name__,'replayed':False})
            raise
    process={'planId':plan_id,'exitCode':result.returncode,'replayed':False,'exitCodeMeaning':'PROCESS_EXIT_ONLY'}
    if result.returncode!=0:
        new(root,'child-result.json',process)
        require(False,'ROUND_CHILD_FAILED')
    db=sqlite3.connect('file:'+str(Path(state)/'ledger.sqlite3')+'?mode=ro',uri=True)
    try:matches=[json.loads(row[0]) for row in db.execute('SELECT value FROM runs') if json.loads(row[0]).get('planId')==plan_id]
    finally:db.close()
    require(len(matches)==1 and matches[0].get('state') in ('COMPLETED','STOPPED','FAILED','UNCONFIRMED_TERMINATION'),'ROUND_CHILD_RESULT_UNCONFIRMED')
    run=matches[0]
    require(isinstance(run.get('runId'),str) and bool(run['runId']),'ROUND_RUN_ID_MISSING')
    summary=outcome_summary(run)
    new(root,'child-result.json',{**process,'outcome':summary})
    return {**run,'trialSucceeded':summary['trialSucceeded']}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for flag in ('config','sha256','state','stop-at'):p.add_argument('--'+flag,required=True)
    group=p.add_mutually_exclusive_group(required=True);group.add_argument('--run-plan');group.add_argument('--reconcile-run')
    p.add_argument('--request-id');p.add_argument('--approve-execution',action='store_true',required=True)
    a=p.parse_args()
    result=run_once({'path':a.config,'sha256':a.sha256},a.state,a.run_plan,a.request_id,stop_at=a.stop_at,reconcile_run=a.reconcile_run)
    print(json.dumps(outcome_summary(result)))

if __name__=='__main__':main()
