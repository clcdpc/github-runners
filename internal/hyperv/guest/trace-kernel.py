#!/usr/bin/env python3
"""Independent, bounded numeric-only kernel observer; never controls services.

Requires Noble bpftrace 0.20, BTF and permitted tracing. Preflight is necessary,
not sufficient: compiler/attach failures remain explicit coverage gaps.
Reason codes: 1 unsupported OS, 2 not root, 3 missing executable/script,
4 missing BTF, 5 missing tracepoint, 6 launch failure, 7 startup/heartbeat
stall, 8 tracer ended, 9 parser rejection, 10 queue overflow, 11 perf loss,
12 kernel rate loss, 13 requested stop, 14 pipe failure, 15 CPU counter reset, 16 CPU limit,
17 stale CPU sample.
Clock contract: event_mono_ns is CLOCK_MONOTONIC (excludes suspend), matching
Python monotonic_ns and journal monotonic timestamps. start_ns,
parent_start_ns and target_start_ns are task_struct.start_boottime values in
CLOCK_BOOTTIME (includes suspend), used only as process identity components.
Never subtract a process start field from event_mono_ns.
Diagnostic error_flags are hints only: permission=1, BTF=2, compiler=4,
missing path=8, unavailable helper=16, resource exhaustion=32. Only fixed
substrings are recognized; their surrounding bytes are never persisted.
No exception strings, child diagnostics or arbitrary bytes reach the logger.
"""
import os
from pathlib import Path
import queue
import re
import signal
import subprocess
import sys
import threading
import time

MAX_LINE = 1024
COMMON = ('event_mono_ns', 'pid', 'tgid', 'start_ns', 'parent_pid',
          'parent_tgid', 'parent_start_ns', 'uid', 'gid', 'cgroup_id', 'program_class')
SCHEMAS = {
    b'F': ('process_fork', COMMON),
    b'E': ('process_exec', COMMON + ('exec_old_pid',)),
    b'X': ('process_exit', COMMON + ('exit_code',)),
    b'S': ('signal_generated', COMMON + ('target_pid', 'target_tgid',
           'target_start_ns', 'signal', 'signal_code', 'signal_group', 'signal_result', 'signal_code_known')),
    b'H': ('observer_heartbeat', ('event_mono_ns',)),
    b'C': ('cpu_counters', ('event_mono_ns', 'cpu_id', 'seen', 'lifecycle_lost', 'signal_lost')),
}
SIGNED = {'signal_code', 'exit_code'}
PID_FIELDS = {'pid', 'tgid', 'parent_pid', 'parent_tgid', 'target_pid',
              'target_tgid', 'exec_old_pid'}
PROBES = ('sched/sched_process_fork', 'sched/sched_process_exec',
          'sched/sched_process_exit', 'signal/signal_generate')


def parse_line(line, stderr=False):
    """Return only a fixed event name and bounded integers, or None."""
    if not isinstance(line, bytes) or len(line) > MAX_LINE:
        return None
    # bpftrace 0.20 TextOutput emits losses on stdout. Accept the same strict
    # numeric format on either pipe, never an arbitrary diagnostic prefix.
    match = re.fullmatch(rb'Lost ([0-9]{1,20}) events\n?', line)
    if match and int(match[1]) <= 2**64 - 1:
        return 'tracer_lost', {'lost': int(match[1])}
    if stderr:
        return None
    if not re.fullmatch(rb'KT1 [FEXSHC](?: -?[0-9]{1,20})+\n?', line):
        return None
    parts = line.rstrip(b'\n').split(b' ')
    event, names = SCHEMAS[parts[1]]
    if len(parts) != len(names) + 2:
        return None
    values = {}
    for name, token in zip(names, parts[2:]):
        value = int(token)
        lower, upper = (-2**31, 2**31 - 1) if name in SIGNED else (0, 2**64 - 1)
        if name in PID_FIELDS:
            upper = 2**31 - 1
        elif name in {'uid', 'gid', 'cpu_id'}:
            upper = 2**32 - 1
        elif name == 'program_class':
            upper = 20
        elif name == 'signal':
            lower, upper = 1, 64
        elif name in {'signal_group', 'signal_code_known'}:
            upper = 1
        elif name == 'signal_result':
            upper = 4
        if not lower <= value <= upper:
            return None
        values[name] = value
    return event, values


def preflight(script, platform=None, euid=None, is_file=None):
    platform = sys.platform if platform is None else platform
    if platform != 'linux':
        return 1
    euid = os.geteuid() if euid is None else euid
    if euid != 0:
        return 2
    exists = (lambda p: Path(p).is_file()) if is_file is None else is_file
    if not exists('/usr/bin/bpftrace') or not exists(script):
        return 3
    if not exists('/sys/kernel/btf/vmlinux'):
        return 4
    if not any(all(exists(root + '/' + p + '/format') for p in PROBES)
               for root in ('/sys/kernel/tracing/events', '/sys/kernel/debug/tracing/events')):
        return 5
    return 0


def diagnostic_flags(line):
    """Extract safe bit flags from bounded stderr; never retain its content."""
    if not isinstance(line, bytes) or len(line) > MAX_LINE:
        return 0
    lower = line.lower()
    flags = 0
    if b'operation not permitted' in lower or b'permission denied' in lower:
        flags |= 1
    if b'btf' in lower:
        flags |= 2
    if b'ERROR:' in line:
        flags |= 4
    if b'no such file or directory' in lower:
        flags |= 8
    if b'not available' in lower and (b'helper' in lower or b'bpf_func_' in lower):
        flags |= 16
    if any(hint in lower for hint in (b'cannot allocate memory',
            b'resource temporarily unavailable', b'too many open files',
            b'no space left on device')):
        flags |= 32
    return flags


class CpuCounters:
    """Bounded CPU cache, cumulative totals never decrease across reset samples.

    Samples from separate CPUs arrive independently and are not an atomic global
    snapshot. Offline CPUs retain their last totals. Resets/stale samples/CPU
    limits return explicit gap reasons rather than silently replacing evidence.
    """
    def __init__(self, limit=4096):
        self.limit = limit
        self.samples = {}
        self.totals = [0, 0, 0]

    def update(self, fields):
        cpu = fields['cpu_id']
        if not 0 <= cpu < self.limit:
            return 16
        timestamp = fields['event_mono_ns']
        values = (fields['seen'], fields['lifecycle_lost'], fields['signal_lost'])
        old = self.samples.get(cpu)
        if old is not None and timestamp <= old[0]:
            return 17
        previous = (0, 0, 0) if old is None else old[1]
        reset = any(value < prior for value, prior in zip(values, previous))
        for i, (value, prior) in enumerate(zip(values, previous)):
            self.totals[i] += value if value < prior else value - prior
        self.samples[cpu] = (timestamp, values)
        return 15 if reset else 0


class Intake:
    """Reader threads discard untrusted bytes before entering a bounded queue."""
    def __init__(self, capacity=1024):
        self.events = queue.Queue(maxsize=capacity)
        self.lock = threading.Lock()
        self.rejected = self.queue_lost = self.pipe_failed = 0
        self.error_flags = 0

    def increment(self, key):
        with self.lock:
            setattr(self, key, getattr(self, key) + 1)

    def flags(self):
        with self.lock:
            return self.error_flags

    def counts(self):
        with self.lock:
            return self.rejected, self.queue_lost, self.pipe_failed

    def read(self, pipe, stderr=False):
        try:
            while True:
                line = pipe.readline(MAX_LINE + 1)
                if not line:
                    break
                if len(line) > MAX_LINE:
                    while line and not line.endswith(b'\n'):
                        line = pipe.readline(MAX_LINE + 1)
                    self.increment('rejected')
                    continue
                if stderr:
                    with self.lock:
                        self.error_flags |= diagnostic_flags(line)
                parsed = parse_line(line, stderr)
                if parsed is None:
                    self.increment('rejected')
                else:
                    try:
                        self.events.put_nowait(parsed)
                    except queue.Full:
                        self.increment('queue_lost')
        except (OSError, ValueError):
            self.increment('pipe_failed')
        finally:
            pipe.close()


def run(log, stop=None):
    stop = threading.Event() if stop is None else stop
    script = Path(__file__).with_suffix('.bt')
    log.emit('observer_start')
    # Every observer session starts after an unobserved interval, including boot.
    log.emit('coverage_gap', reason=0)
    reason = preflight(script)
    if reason:
        log.emit('attach_failed', reason=reason, error_flags=0)
        log.emit('observer_heartbeat', ready=0, reason=reason, error_flags=0)
        return 1
    try:
        child = subprocess.Popen(
            ['/usr/bin/bpftrace', '-q', '-B', 'line', str(script)],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env={'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LANG': 'C', 'LC_ALL': 'C',
                 'BPFTRACE_MAX_MAP_KEYS': '8192', 'BPFTRACE_PERF_RB_PAGES': '64'},
            close_fds=True)
    except OSError:
        log.emit('attach_failed', reason=6, error_flags=0)
        return 1
    intake = Intake()
    counters = CpuCounters()
    readers = [threading.Thread(target=intake.read, args=(child.stdout,), daemon=True),
               threading.Thread(target=intake.read, args=(child.stderr, True), daemon=True)]
    for reader in readers:
        reader.start()
    started = last_kernel = last_report = time.monotonic()
    ready = False
    received = perf_lost = rate_lost = seen = lifecycle_lost = signal_lost = 0
    previous = (0, 0, 0, 0, 0)
    exit_reason = 8
    try:
        while True:
            if stop.is_set():
                exit_reason = 13
                break
            now = time.monotonic()
            if now - (last_kernel if ready else started) > 30:
                exit_reason = 7
                break
            try:
                event, fields = intake.events.get(timeout=0.2)
                if event == 'observer_heartbeat':
                    last_kernel = time.monotonic()
                    if not ready:
                        ready = True
                        log.emit('observer_ready', ready=1)
                elif event == 'cpu_counters':
                    gap = counters.update(fields)
                    if gap:
                        log.emit('coverage_gap', reason=gap, cpu_id=fields['cpu_id'], error_flags=intake.flags())
                    seen, lifecycle_lost, signal_lost = counters.totals
                    rate_lost = lifecycle_lost + signal_lost
                elif event == 'tracer_lost':
                    perf_lost += fields['lost']
                else:
                    received += 1
                    log.emit(event, **fields)
            except queue.Empty:
                pass
            rejected, queue_lost, pipe_failed = intake.counts()
            current = (rejected, queue_lost, pipe_failed, perf_lost, rate_lost)
            if now - last_report >= 5:
                for value, old, code in zip(current, previous, (9, 10, 14, 11, 12)):
                    if value > old:
                        log.emit('coverage_gap', reason=code, lost=value-old, error_flags=intake.flags())
                if rejected > previous[0]:
                    log.emit('parser_error', rejected=rejected)
                if perf_lost > previous[3] or rate_lost > previous[4] or queue_lost > previous[1]:
                    log.emit('tracer_lost', perf_lost=perf_lost,
                             rate_lost=rate_lost, queue_lost=queue_lost)
                log.emit('observer_heartbeat', ready=int(ready), received=received,
                         seen=seen, rejected=rejected, queue_lost=queue_lost,
                         perf_lost=perf_lost, rate_lost=rate_lost,
                         lifecycle_lost=lifecycle_lost, signal_lost=signal_lost, error_flags=intake.flags())
                previous, last_report = current, now
            if child.poll() is not None and all(not r.is_alive() for r in readers) and intake.events.empty():
                break
    finally:
        log.emit('coverage_gap', reason=exit_reason, error_flags=intake.flags())
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)
        for reader in readers:
            reader.join(timeout=1)
        rejected, queue_lost, pipe_failed = intake.counts()
        if not ready and exit_reason != 13:
            log.emit('attach_failed', reason=exit_reason, error_flags=intake.flags())
        log.emit('tracer_exit', return_code=child.returncode or 0,
                 received=received, rejected=rejected, queue_lost=queue_lost,
                 perf_lost=perf_lost, rate_lost=rate_lost, lifecycle_lost=lifecycle_lost,
                 signal_lost=signal_lost, reason=exit_reason, error_flags=intake.flags())
        log.emit('observer_stop', ready=0, reason=exit_reason)
    return 0 if exit_reason == 13 else 1


def main():
    from trace_common import TraceLog, process_snapshot
    stop = threading.Event()
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, lambda *_: stop.set())
    log = TraceLog('kernel')
    try:
        process_snapshot(log)
        return run(log, stop)
    except Exception:
        # No exception representation: it can contain arbitrary child content.
        log.emit('coverage_gap', reason=14)
        return 1
    finally:
        log.close()


if __name__ == '__main__':
    sys.exit(main())
