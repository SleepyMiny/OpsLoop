import datetime as dt
import unittest
from unittest.mock import Mock,patch
from tools.exhibition_control import ControlError
from tools.exhibition_session import run_once

class SessionTests(unittest.TestCase):
    def setUp(self):
        self.c=Mock();self.c._plan.return_value={'track':'runtime','expiresAt':(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=5)).isoformat()}
        self.c.start.return_value={'runId':'run-1'};self.c._runs.return_value=[{'runId':'run-1','state':'COMPLETED','lifecycleComplete':True}]
        self.c.worker.is_alive.return_value=False
        self.end=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=10)).isoformat()
    def call(self,**kw):
        with patch('tools.exhibition_session.pinned',return_value={}),patch('tools.exhibition_session.Coordinator',return_value=self.c),patch('tools.exhibition_session.validate_plan'),patch('tools.exhibition_permit.bound_permit'):
            return run_once({'path':'private','sha256':'pin'},'/tmp/synthetic','plan','request',stop_at=self.end,**kw)
    def test_existing_coordinator_single_dispatch_and_completed_cleanup(self):
        result=self.call();self.assertTrue(result['lifecycleComplete']);self.c.start.assert_called_once_with('plan','request');self.c.close.assert_called_once()
    def test_cleanup_only_does_not_prepare_or_start(self):
        self.c._runs.return_value=[{'runId':'run-old','state':'STOPPED','lifecycleComplete':True}]
        result=self.call(reconcile_run='run-old');self.assertTrue(result['lifecycleComplete']);self.c.reconcile.assert_called_once_with('run-old');self.c.start.assert_not_called();self.c.prepare.assert_not_called()
    def test_bound_permit_failure_zero_start(self):
        with patch('tools.exhibition_session.pinned',return_value={}),patch('tools.exhibition_session.Coordinator',return_value=self.c),patch('tools.exhibition_session.validate_plan'),patch('tools.exhibition_permit.bound_permit',side_effect=ControlError('PERMIT_EXPIRED')):
            with self.assertRaises(ControlError):run_once({'path':'private','sha256':'pin'},'/tmp/synthetic','plan','request',stop_at=self.end)
        self.c.start.assert_not_called();self.c.close.assert_called_once()
    def test_missing_cleanup_reserve_zero_start(self):
        self.end=(dt.datetime.now(dt.timezone.utc)+dt.timedelta(minutes=6)).isoformat()
        with self.assertRaises(ControlError):self.call()
        self.c.start.assert_not_called()
    def test_interruption_stops_consumed_run_without_retry(self):
        self.c._runs.side_effect=KeyboardInterrupt
        with self.assertRaises(KeyboardInterrupt):self.call()
        self.c.start.assert_called_once();self.c.stop.assert_called_once_with('run-1');self.c.close.assert_called_once()

if __name__=='__main__':unittest.main()
