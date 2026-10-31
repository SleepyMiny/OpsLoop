"""Read-only public projection of the host Coordinator's existing ledger.

Never acquires execution ownership, prepares a run, repairs state, or returns plans.
"""
import json,os,sqlite3
from pathlib import Path
from tools.exhibition_control import ControlError,identifier,public_data

class LedgerView:
    def __init__(self,state):self.path=Path(state)/'ledger.sqlite3'
    def rows(self,query,args=()):
        if not self.path.exists():return []
        if any(p.is_symlink() for p in (self.path,*self.path.parents)) or self.path.stat().st_uid!=os.getuid() or self.path.stat().st_mode&0o077:
            raise ControlError('PRIVATE_LEDGER_REQUIRED')
        db=sqlite3.connect(self.path.as_uri()+'?mode=ro',uri=True,timeout=1)
        try:return [json.loads(r[0]) for r in db.execute(query,args)]
        finally:db.close()
    def state(self):
        keys=('runId','planId','track','state','source','binding','lastSeq','stopConfirmed','lifecycleComplete','lifecycle','executionState','lifecycleReceiptSHA256')
        runs=[{k:r[k] for k in keys if k in r} for r in self.rows('SELECT value FROM runs ORDER BY rowid DESC LIMIT 100')]
        return {'active':next((r for r in runs if not r.get('lifecycleComplete')),None),'runs':runs,'plans':[],'nextRunAllowed':False}
    def events(self,run_id,after=0):
        if not identifier(run_id) or type(after)!=int or after<0:raise ControlError('INVALID_CURSOR')
        rows=self.rows('SELECT value FROM events WHERE run=? AND seq>? ORDER BY seq LIMIT 200',(run_id,after))
        return {'events':[{**{k:e[k] for k in ('runId','seq','at','actor','kind','source')},'data':public_data(e.get('data',{}))} for e in rows]}
