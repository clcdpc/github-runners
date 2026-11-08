import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import unittest

PATH = Path(__file__).resolve().parents[1] / 'guest' / 'trace-state.py'
spec = importlib.util.spec_from_file_location('state_trace', PATH)
state = importlib.util.module_from_spec(spec)
spec.loader.exec_module(state)


class StateTests(unittest.TestCase):
    def setUp(self):
        self.events = []
        self.observer = state.Observer(lambda event, **fields: self.events.append((event, fields)))

    def record(self, kind=7, **extra):
        data = {'_PID': '1', 'UNIT': 'clc-runner.service', 'MESSAGE_ID': list(state.MESSAGE_IDS)[kind - 1], '__MONOTONIC_TIMESTAMP': '1200'}
        data.update(extra)
        return json.dumps(data).encode()

    def test_restart_and_service_failure_are_distinct(self):
        self.observer.journal(self.record(N_RESTARTS='2'))
        self.observer.journal(self.record(6, UNIT_RESULT='watchdog'))
        self.observer.journal(self.record(5, JOB_RESULT='failed'))
        self.assertEqual([e[1]['kind'] for e in self.events], [7, 6, 5])
        self.assertEqual(self.events[1][1]['unit_result'], state.RESULT['watchdog'])

    def test_unknown_id_structured_job(self):
        result = state.journal_record(self.record(MESSAGE_ID='0' * 32, JOB_TYPE='stop', JOB_RESULT='done'))
        self.assertEqual(result['kind'], 11)
        self.assertEqual(result['job_type'], state.JOB_TYPE['stop'])

    def test_strict_privacy(self):
        canary = 'SECRET_CANARY_TOKEN'
        bad = [self.record(_PID='1200'), self.record(_PID=['1']), self.record(UNIT='worker-' + canary), self.record(MESSAGE=canary), self.record(COMMAND=canary), self.record(EXIT_STATUS=canary), self.record(UNIT_RESULT=canary), self.record(INVOCATION_ID=canary), self.record(MESSAGE_ID=canary), self.record(N_RESTARTS='-1'), self.record(N_RESTARTS='9' * 21), b'{"_PID":"1","_PID":"1"}', b'[]', b'\xff', b'x' * (state.MAX_LINE + 1)]
        for raw in bad:
            self.observer.journal(raw)
        self.assertEqual(len(self.events), len(bad))
        self.assertTrue(all(event == 'state_reject' for event, fields in self.events))
        self.assertNotIn(canary, repr(self.events))

    def test_mandatory_cursor_is_discarded(self):
        self.observer.journal(self.record(__CURSOR='SECRET_CANARY_TOKEN'))
        self.assertEqual(self.events[0][0], 'state_journal')
        self.assertNotIn('SECRET_CANARY_TOKEN', repr(self.events))

    def test_numeric_schema_and_ids(self):
        self.observer.journal(self.record(INVOCATION_ID='f' * 32, _BOOT_ID='a' * 32, _UID='0', EXIT_CODE='killed', EXIT_STATUS='9'))
        for event, fields in self.events:
            self.assertTrue(all(type(value) in (int, bool) for value in fields.values()))
        self.assertEqual(self.events[0][1]['invocation_hi'], 2**64 - 1)

    def test_changed_snapshots_and_fixed_commands(self):
        calls = []
        def command(argv):
            calls.append(argv)
            if argv == state.COMMANDS[0]:
                return b'Id=clc-runner.service\nActiveState=active\nSubState=running\nNRestarts=1\nMainPID=100\n'
            return b'Id=clc-runner.service\nRestart=always\nOOMPolicy=stop\nWatchdogUSec=1min 30s\nTimeoutStopUSec=infinity\n'
        self.observer.command = command
        self.observer.sample()
        self.observer.sample()
        self.assertEqual(calls, list(state.COMMANDS) * 2)
        snapshots = [fields for event, fields in self.events if event == 'state_snapshot']
        self.assertEqual(len(snapshots), 2)
        self.assertEqual(snapshots[1]['watchdog_us'], 90000000)
        self.assertEqual(snapshots[1]['timeout_stop_us'], 2**64 - 1)

    def test_failed_command_recovery_and_gap(self):
        def fail(argv):
            raise TimeoutError('SECRET_CANARY_TOKEN')
        self.observer.command = fail
        self.observer.sample()
        self.assertEqual(sum(event == 'state_command_failure' for event, _ in self.events), 2)
        self.observer.command = lambda argv: b'Id=clc-runner.service\n'
        self.observer.clock = lambda: self.observer.last_tick + 12
        self.observer.sample()
        self.assertTrue(any(event == 'state_gap' and fields['duration_ms'] == 12000 for event, fields in self.events))
        self.assertEqual(len(self.observer.failed), 0)
        self.assertNotIn('SECRET_CANARY_TOKEN', repr(self.events))

    def test_bad_snapshot(self):
        for raw in (b'Id=secret.service\n', b'Id=clc-runner.service\nEnvironment=SECRET_CANARY_TOKEN\n', b'Id=clc-runner.service\nMainPID=bad\n', b'Id=clc-runner.service\nId=clc-runner.service\n', b'', b'Id=clc-runner.service\nActiveState=SECRET_CANARY_TOKEN\n'):
            with self.assertRaises((ValueError, KeyError)):
                state.snapshots(raw, 0)

    def test_bounded_commands(self):
        self.assertEqual(state.bounded_command((sys.executable, '-c', 'print(123)'), timeout=2).strip(), b'123')
        with self.assertRaises(OverflowError):
            state.bounded_command((sys.executable, '-c', 'print("x" * 4096)'), limit=32)
        with self.assertRaises(TimeoutError):
            state.bounded_command((sys.executable, '-c', 'import time; time.sleep(5)'), timeout=.05)
        with self.assertRaises(ChildProcessError):
            state.bounded_command((sys.executable, '-c', 'raise SystemExit(2)'))

    def test_bounded_journal_lines_and_queue(self):
        class Process:
            stdout = io.BytesIO((b'x' * (state.MAX_LINE * 3) + b'\n') + (b'{}\n' * 300))
        feed = state.JournalFeed(Process())
        feed.thread.join(2)
        self.assertTrue(feed.done.is_set())
        self.assertEqual(feed.queue.qsize(), 256)
        self.assertEqual(feed.dropped, 45)

    def test_shared_sink_accepts_all_fields(self):
        import tempfile
        sink_spec = importlib.util.spec_from_file_location('state_test_common', PATH.with_name('trace_common.py'))
        common = importlib.util.module_from_spec(sink_spec)
        sink_spec.loader.exec_module(common)
        with tempfile.TemporaryDirectory() as directory:
            log = common.TraceLog('state', directory=directory)
            observer = state.Observer(log.emit, command=lambda argv: b'Id=clc-runner.service\n')
            observer.sample()
            observer.journal(self.record())
            observer.journal(b'invalid')
            log.emit('state_gap', layer=5, lost_records=1)
            self.assertEqual(log.rejected, 0)
            log.close()

    def test_command_scope_and_private_gap(self):
        self.assertEqual(state.JOURNAL_ARGV[-1], '_PID=1')
        self.assertNotIn('MESSAGE', state.JFIELDS)
        self.assertNotIn('COMMAND', state.JFIELDS)
        source = PATH.read_text(encoding='utf-8-sig')
        self.assertIn("state_gap', layer=6", source)
        for argv in state.COMMANDS:
            self.assertEqual(argv[:2], ('systemctl', 'show'))
            self.assertEqual(set(argv[4:]), set(state.UNITS))


if __name__ == '__main__':
    unittest.main()
