"""Synthetic transaction failure boundaries; no power-loss durability claim."""
import json
from contextlib import closing
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from tools.exhibition_control import ControlError
from tools.exhibition_permit import issue, verify
from tests.test_exhibition_finite_rounds import FiniteFixture


class FiniteIssuanceDurabilityTests(unittest.TestCase):
    def scenario(self, boundary):
        with tempfile.TemporaryDirectory() as directory:
            f = FiniteFixture(Path(directory))
            payload = f.payload()
            request = f.request(payload, 'interrupted')
            original_connect = sqlite3.connect
            triggered = []

            class Connection:
                def __init__(self, *args, **kwargs):
                    self.db = original_connect(*args, **kwargs)
                    self.commits = 0
                def execute(self, sql, *args):
                    if boundary == 'issued-before-write' and sql.startswith('UPDATE permits SET state'):
                        triggered.append(boundary)
                        raise sqlite3.OperationalError('synthetic state update failure')
                    result = self.db.execute(sql, *args)
                    if sql == 'COMMIT':
                        self.commits += 1
                    if boundary == 'reservation-committed-process-exit' and sql == 'COMMIT':
                        triggered.append(boundary)
                        raise SystemExit('synthetic exit after reservation commit')
                    if boundary == 'issued-committed-response-lost' and sql == 'COMMIT' and self.commits == 2:
                        triggered.append(boundary)
                        raise sqlite3.OperationalError('synthetic commit acknowledgement loss')
                    return result
                def close(self):
                    self.db.close()

            with mock.patch('tools.exhibition_permit.sqlite3.connect', side_effect=Connection), self.assertRaises((sqlite3.OperationalError, SystemExit)):
                issue(*request)
            self.assertEqual(triggered, [boundary])
            with closing(original_connect(f.root/'issuer/issuance.sqlite3')) as db:
                state = db.execute('SELECT state FROM permits WHERE authorization=?', (payload['authorizationID'],)).fetchone()[0]
                self.assertEqual(db.execute('SELECT count(*) FROM finite_rounds').fetchone()[0], 1)
            expected = 'ISSUED' if boundary == 'issued-committed-response-lost' else 'RESERVED_NO_RETRY'
            self.assertEqual(state, expected)
            if boundary == 'reservation-committed-process-exit':
                self.assertFalse(request[-1].exists())
            else:
                self.assertEqual(verify(json.loads(request[-1].read_text()), f.public), payload)
            with self.assertRaises(ControlError):
                f.mint(payload, 'duplicate-after-restart')
            if expected == 'RESERVED_NO_RETRY':
                # Even a separately fabricated safe completion cannot unburn a
                # partially issued slot. Real receipts must never be fabricated.
                f.complete(payload)
                f.select(1)
                with self.assertRaisesRegex(ControlError, 'ISSUANCE_PREDECESSOR_NOT_COMPLETE'):
                    f.mint(f.payload(), 'next-after-restart')
            else:
                # Durable successful issuance alone is not evidence of execution
                # and safe shutdown. No next authority without that completion.
                f.select(1)
                with self.assertRaises(ControlError):
                    f.payload()

    def test_committed_reservation_survives_process_exit(self):
        self.scenario('reservation-committed-process-exit')

    def test_issued_update_failure_preserves_blocked_reservation(self):
        self.scenario('issued-before-write')

    def test_durable_issued_response_loss_cannot_duplicate_or_skip_completion(self):
        self.scenario('issued-committed-response-lost')


if __name__ == '__main__':
    unittest.main()
