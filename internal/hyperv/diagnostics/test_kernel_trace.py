"""Numeric parser/preflight/pipe tests only. Never installs or runs eBPF."""
import importlib.util
import io
from pathlib import Path
import subprocess
import threading
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('kernel_trace', ROOT / 'guest/trace-kernel.py')
trace = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(trace)
COMMON = b'1000000000 42 42 900 1 1 5 1000 1000 11 2'


class KernelTraceTests(unittest.TestCase):
    def test_all_event_shapes(self):
        for tag, tail, event in [(b'F', b'', 'process_fork'),
                                 (b'E', b' 43', 'process_exec'),
                                 (b'X', b' 9', 'process_exit'),
                                 (b'S', b' 99 99 800 9 -6 1 0 1', 'signal_generated')]:
            name, fields = trace.parse_line(b'KT1 ' + tag + b' ' + COMMON + tail + b'\n')
            self.assertEqual(name, event)
            self.assertEqual(fields['program_class'], 2)
            self.assertEqual(fields['start_ns'], 900)
            self.assertTrue(all(type(v) is int for v in fields.values()))
        self.assertEqual(trace.parse_line(b'KT1 C 100 0 200 3 2\n')[1]['signal_lost'], 2)

    def test_sensitive_and_malformed_lines_rejected(self):
        for line in [b'token=SECRET', b'Attaching 5 probes...\n', b'KT1 F ' + COMMON + b' SECRET\n',
                     b'KT1 H 1 2 3 4 5\r\n', b'KT1 H 1 2 3 4 5\nSECRET',
                     b'KT1 H -1 2 3 4 5\n', b'KT1 H 18446744073709551616 2 3 4 5\n',
                     b'KT1 H 1 2 3 4', b'\xff', b'a' * 1025]:
            self.assertIsNone(trace.parse_line(line), line)

    def test_numeric_ranges_and_signal_sentinels(self):
        for common in [COMMON.rsplit(b' ', 1)[0] + b' 21', COMMON.replace(b' 42 ', b' 2147483648 ', 1)]:
            self.assertIsNone(trace.parse_line(b'KT1 F ' + common))
        for tail in [b' 99 99 800 0 0 1 0 1', b' 99 99 800 65 0 1 0 1',
                     b' 99 99 800 9 0 2 0 1', b' 99 99 800 9 0 1 5 1',
                     b' 99 99 800 9 0 1 0 2']:
            self.assertIsNone(trace.parse_line(b'KT1 S ' + COMMON + tail))
        self.assertEqual(trace.parse_line(b'KT1 S ' + COMMON + b' 99 99 800 9 0 1 0 0')[1]['signal_code_known'], 0)

    def test_stderr_only_accepts_exact_numeric_loss(self):
        for stderr in (False, True):
            self.assertEqual(trace.parse_line(b'Lost 123 events\n', stderr), ('tracer_lost', {'lost': 123}))
        for line in [b'error SECRET', b'Lost 1 events SECRET', b'Lost -1 events', b'Lost 18446744073709551616 events']:
            for stderr in (False, True):
                self.assertIsNone(trace.parse_line(line, stderr))

    def test_secret_diagnostics_become_only_numeric_hint_flags(self):
        cases = [(b'Operation not permitted SECRET', 1),
                 (b'permission denied SECRET', 1),
                 (b'failed BTF SECRET', 2), (b'ERROR: SECRET', 4),
                 (b'No such file or directory SECRET', 8),
                 (b'BPF_FUNC_x not available SECRET', 16),
                 (b'helper not available SECRET', 16),
                 (b'Cannot allocate memory SECRET', 32),
                 (b'Resource temporarily unavailable SECRET', 32),
                 (b'SECRET unrelated text', 0)]
        for line, expected in cases:
            self.assertEqual(trace.diagnostic_flags(line), expected)
        intake = trace.Intake()
        intake.read(io.BytesIO(b'ERROR: SECRET BTF Permission denied\n'), True)
        self.assertEqual(intake.flags(), 7)
        self.assertEqual(intake.counts(), (1, 0, 0))
        self.assertTrue(intake.events.empty())
        self.assertNotIn('SECRET', repr(vars(intake)))
        self.assertEqual(trace.diagnostic_flags(b'ERROR:' + b'SECRET' * 1000), 0)

    def test_preflight_fail_closed_without_launch(self):
        self.assertEqual(trace.preflight('script', platform='win32'), 1)
        self.assertEqual(trace.preflight('script', platform='linux', euid=1000), 2)
        for missing, reason in [('/usr/bin/bpftrace', 3), ('script', 3), ('/sys/kernel/btf/vmlinux', 4)]:
            self.assertEqual(trace.preflight('script', platform='linux', euid=0,
                                            is_file=lambda p: str(p) != missing), reason)
        self.assertEqual(trace.preflight('script', platform='linux', euid=0,
                                        is_file=lambda p: not str(p).endswith('/format')), 5)
        self.assertEqual(trace.preflight('script', platform='linux', euid=0, is_file=lambda p: True), 0)
        log = Mock()
        with patch.object(trace, 'preflight', return_value=4), patch.object(trace.subprocess, 'Popen') as popen:
            self.assertEqual(trace.run(log), 1)
            popen.assert_not_called()
        log.emit.assert_any_call('attach_failed', reason=4, error_flags=0)

    def test_bounded_intake_and_oversized_secret_discard(self):
        intake = trace.Intake(capacity=1)
        intake.read(io.BytesIO(b'SECRET' * 1000 + b'\nKT1 H 1\nKT1 H 2\n'))
        self.assertEqual(intake.counts(), (1, 1, 0))
        self.assertEqual(intake.events.qsize(), 1)
        self.assertEqual(intake.events.get()[1]['event_mono_ns'], 1)

    def test_launch_failure_does_not_log_exception(self):
        log = Mock()
        with patch.object(trace, 'preflight', return_value=0), patch.object(trace.subprocess, 'Popen', side_effect=OSError('SECRET')):
            self.assertEqual(trace.run(log), 1)
        self.assertNotIn('SECRET', str(log.mock_calls))
        log.emit.assert_any_call('attach_failed', reason=6, error_flags=0)

    def test_child_protocol_and_numeric_final_health(self):
        child = Mock()
        child.stdout = io.BytesIO(b'Lost 3 events\nKT1 H 100\nKT1 C 100 0 20 3 2\nKT1 F ' + COMMON + b'\n')
        child.stderr = io.BytesIO(b'Lost 2 events\nERROR: SECRET Operation not permitted BPF_FUNC_x not available\n')
        child.poll.return_value = 0
        child.returncode = 0
        log = Mock()
        with patch.object(trace, 'preflight', return_value=0), patch.object(trace.subprocess, 'Popen', return_value=child) as popen:
            self.assertEqual(trace.run(log), 1)  # Early tracer exit is degraded even status zero.
        args, kwargs = popen.call_args
        self.assertEqual(args[0][:4], ['/usr/bin/bpftrace', '-q', '-B', 'line'])
        self.assertNotIn('shell', kwargs)
        self.assertEqual(kwargs['env']['BPFTRACE_MAX_MAP_KEYS'], '8192')
        self.assertNotIn('SECRET', str(log.mock_calls))
        log.emit.assert_any_call('observer_ready', ready=1)
        final = [c for c in log.emit.call_args_list if c.args[0] == 'tracer_exit'][0]
        self.assertEqual(final.kwargs['perf_lost'], 5)
        self.assertEqual(final.kwargs['rate_lost'], 5)
        self.assertEqual(final.kwargs['rejected'], 1)
        self.assertEqual(final.kwargs['error_flags'], 21)

    def test_requested_stop_terminates_child(self):
        child = Mock()
        child.stdout, child.stderr = io.BytesIO(), io.BytesIO()
        child.poll.return_value = None
        child.returncode = -15
        stop = threading.Event()
        stop.set()
        with patch.object(trace, 'preflight', return_value=0), patch.object(trace.subprocess, 'Popen', return_value=child):
            self.assertEqual(trace.run(Mock(), stop), 0)
        child.terminate.assert_called_once()

    def test_heartbeat_timeout_is_degraded_and_terminates(self):
        child = Mock()
        child.stdout, child.stderr = io.BytesIO(), io.BytesIO()
        child.poll.return_value = None
        child.returncode = -15
        log = Mock()
        with patch.object(trace, 'preflight', return_value=0), patch.object(trace.subprocess, 'Popen', return_value=child), patch.object(trace.time, 'monotonic', side_effect=[0, 31]):
            self.assertEqual(trace.run(log), 1)
        log.emit.assert_any_call('coverage_gap', reason=7, error_flags=0)
        child.terminate.assert_called_once()

    def test_pipe_failure_only_increments_numeric_counter(self):
        intake = trace.Intake()
        pipe = Mock()
        pipe.readline.side_effect = OSError('SECRET')
        intake.read(pipe)
        self.assertEqual(intake.counts(), (0, 0, 1))
        self.assertTrue(intake.events.empty())

    def test_terminate_timeout_escalates_only_own_child(self):
        child = Mock()
        child.stdout, child.stderr = io.BytesIO(), io.BytesIO()
        child.poll.return_value = None
        child.returncode = -9
        child.wait.side_effect = [subprocess.TimeoutExpired('bpftrace', 5), 0]
        stop = threading.Event()
        stop.set()
        with patch.object(trace, 'preflight', return_value=0), patch.object(trace.subprocess, 'Popen', return_value=child):
            self.assertEqual(trace.run(Mock(), stop), 0)
        child.kill.assert_called_once()

    def test_cpu_counter_aggregation_is_bounded_monotonic_and_reports_resets(self):
        counters = trace.CpuCounters(limit=2)
        def sample(cpu, stamp, seen, lifecycle, signals):
            return counters.update(dict(cpu_id=cpu, event_mono_ns=stamp, seen=seen,
                                        lifecycle_lost=lifecycle, signal_lost=signals))
        self.assertEqual(sample(0, 10, 100, 3, 2), 0)
        self.assertEqual(sample(1, 10, 50, 1, 1), 0)
        self.assertEqual(counters.totals, [150, 4, 3])
        self.assertEqual(sample(0, 20, 110, 4, 3), 0)
        self.assertEqual(counters.totals, [160, 5, 4])
        self.assertEqual(sample(0, 15, 105, 3, 2), 17)
        self.assertEqual(counters.totals, [160, 5, 4])
        self.assertEqual(sample(0, 30, 2, 0, 0), 15)
        self.assertEqual(counters.totals, [162, 5, 4])
        self.assertEqual(sample(0, 40, 3, 1, 1), 0)
        self.assertEqual(counters.totals, [163, 6, 5])
        self.assertEqual(sample(2, 50, 1000, 500, 500), 16)
        self.assertEqual(len(counters.samples), 2)
        self.assertEqual(counters.totals, [163, 6, 5])

    def test_clock_only_heartbeat_is_valid_without_cpu_samples(self):
        self.assertEqual(trace.parse_line(b'KT1 H 123\n'),
                         ('observer_heartbeat', {'event_mono_ns': 123}))
        self.assertEqual(trace.CpuCounters().totals, [0, 0, 0])

    def test_explicit_monotonic_event_clock_and_boottime_identity(self):
        source = (ROOT / 'guest/trace-kernel.bt').read_text()
        self.assertEqual(source.count('nsecs(monotonic) /'), 4)
        self.assertEqual(source.count('nsecs(monotonic),'), 6)
        self.assertIn('nsecs(monotonic));', source)
        self.assertNotIn('nsecs /', source)
        self.assertNotIn('nsecs,', source)
        self.assertIn('CLOCK_BOOTTIME', trace.__doc__)
        self.assertIn('Never subtract a process start field from event_mono_ns', trace.__doc__)
        self.assertIn('process_snapshot(log)', (ROOT / 'guest/trace-kernel.py').read_text())

    def test_source_privacy_and_bounded_probe_contract(self):
        source = (ROOT / 'guest/trace-kernel.bt').read_text()
        for probe in ('sched_process_fork', 'sched_process_exec', 'sched_process_exit', 'signal_generate'):
            self.assertIn('rawtracepoint:' + probe, source)
        for unsafe in ('%s', '->filename', '->args', '->env', 'ustack(', 'kstack(', 'system('):
            self.assertNotIn(unsafe, source)
        self.assertIn('if (arg1 > 2)', source)
        self.assertIn('$target = (struct task_struct *)arg2', source)
        self.assertIn('@used[$cpu, $kind] < 128', source)
        self.assertIn('@used[$cpu, $kind] < 256', source)
        self.assertEqual(source.count('$name = $p->comm;'), 4)
        self.assertNotIn('str($p->comm', source)
        self.assertIn('$name == "systemctl"', source)
        self.assertNotIn('@used[pid', source)
        self.assertNotIn('= count()', source)
        self.assertEqual(source.count('@lifecycle_dropped[$cpu] = @lifecycle_dropped[$cpu] + 1;'), 3)
        self.assertEqual(source.count('@signal_dropped[$cpu] = @signal_dropped[$cpu] + 1;'), 1)
        self.assertIn('profile:hz:1', source)
        self.assertIn('@seen[$cpu] = @seen[$cpu] + 1;', source)
        self.assertIn('\"deb-systemd-inv\"', source)
        self.assertNotIn('deb-systemd-invo', source)


if __name__ == '__main__':
    unittest.main()
