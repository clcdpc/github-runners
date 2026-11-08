#!/usr/bin/env python3
"""Bounded, read-only PID-1 journal and systemd state evidence.

All persisted values are numeric enums, counts, timestamps or ID halves. Missing
journal coverage is reported, not interpreted as proof that no restart occurred.
Health/gap layer: 1=sampler, 2=state command, 3=policy command,
5=journal subprocess (process liveness, not proof of journal delivery),
6=unobserved private systemd socket.
Event kind 5 is a start-job failure; 6 is a service result (including watchdog or
timeout); 7 is systemd's automatic restart scheduling, not an external request.
"""
import json
import queue
import re
import signal
import subprocess
import threading
import time

UNITS = {s: i + 1 for i, s in enumerate((
    'clc-runner.service', 'docker.service', 'containerd.service',
    'cloud-final.service', 'apt-daily.service', 'apt-daily-upgrade.service',
    'unattended-upgrades.service', 'systemd-oomd.service', 'shutdown.target',
    'reboot.target', 'poweroff.target', 'halt.target', 'rescue.target',
    'emergency.target', 'multi-user.target', 'clc-trace-bus.service',
    'clc-trace-kernel.service', 'clc-trace-state.service'))}

def enums(*values):
    return {s: i + 1 for i, s in enumerate(values)}

ACTIVE = enums('active', 'reloading', 'inactive', 'failed', 'activating', 'deactivating', 'maintenance', 'refreshing')
SUB = enums('dead', 'start-pre', 'start', 'start-post', 'running', 'exited', 'reload', 'reload-signal', 'reload-notify', 'stop', 'stop-watchdog', 'stop-sigterm', 'stop-sigkill', 'stop-post', 'final-watchdog', 'final-sigterm', 'final-sigkill', 'failed', 'auto-restart', 'auto-restart-queued', 'cleaning', 'condition', 'active')
RESULT = enums('success', 'resources', 'timeout', 'exit-code', 'signal', 'core-dump', 'watchdog', 'start-limit-hit', 'oom-kill', 'exec-condition', 'protocol')
JOB_RESULT = enums('done', 'canceled', 'timeout', 'failed', 'dependency', 'skipped', 'invalid', 'assert', 'unsupported', 'collected', 'once')
JOB_TYPE = enums('start', 'verify-active', 'stop', 'reload', 'restart', 'try-restart', 'reload-or-start', 'try-reload', 'reload-or-restart', 'try-reload-or-restart', 'nop')
EXIT = enums('exited', 'killed', 'dumped')
RESTART = enums('no', 'on-success', 'on-failure', 'on-abnormal', 'on-watchdog', 'on-abort', 'always')
OOM = enums('continue', 'stop', 'kill')
LOAD = enums('stub', 'loaded', 'not-found', 'bad-setting', 'error', 'merged', 'masked')
MESSAGE_IDS = {s: i + 1 for i, s in enumerate((
    '7d4958e842da4a758f6c1cdc7b36dcc5', '39f53479d3a045ac8e11786248231fbf',
    'de5b426a63be47a7b6ac3eaac82e2f6f', '9d1aaa27d60140bd96365438aad20286',
    'be02cf6855d2428ba40df7e9d022f03d', 'd9b373ed55a64feb8242e02dbe79a49c',
    '5eb03494b6584870a536b337290809b3', '98e322203f7a4ed290d09fe03c09fe15',
    'd989611b15e44c9dbf31e3c81256e4ed', 'fe6faa94e7774663a0da52717891d8ef'))}
JNUM = {'__MONOTONIC_TIMESTAMP': 'source_mono_us', '__REALTIME_TIMESTAMP': 'source_utc_us', '_PID': 'pid', '_UID': 'uid', '_GID': 'gid', 'JOB_ID': 'job_id', 'EXIT_STATUS': 'exit_status', 'N_RESTARTS': 'n_restarts', 'N_PROCESSES': 'n_processes'}
JENUM = {'JOB_TYPE': ('job_type', JOB_TYPE), 'JOB_RESULT': ('job_result', JOB_RESULT), 'UNIT_RESULT': ('unit_result', RESULT), 'EXIT_CODE': ('exit_code', EXIT)}
JIDS = {'_BOOT_ID': 'source_boot', 'INVOCATION_ID': 'invocation'}
# journalctl emits __CURSOR even when excluded by --output-fields. It is
# transport metadata only: bound/type-check it and discard without persistence.
JFIELDS = tuple(JNUM) + tuple(JENUM) + tuple(JIDS) + ('MESSAGE_ID', 'UNIT')
JOURNAL_ARGV = ('journalctl', '--boot=0', '--follow', '--no-pager', '--output=json', '--output-fields=' + ','.join(JFIELDS), '_PID=1')
SENUM = {'LoadState': ('load_state', LOAD), 'ActiveState': ('active_state', ACTIVE), 'SubState': ('sub_state', SUB), 'Result': ('result', RESULT), 'Restart': ('restart', RESTART), 'OOMPolicy': ('oom_policy', OOM)}
SNUM = {'MainPID': 'main_pid', 'ExecMainCode': 'exec_main_code', 'ExecMainStatus': 'exec_main_status', 'NRestarts': 'n_restarts', 'StateChangeTimestampMonotonic': 'state_change_mono_us', 'ActiveEnterTimestampMonotonic': 'active_enter_mono_us', 'ActiveExitTimestampMonotonic': 'active_exit_mono_us', 'InactiveEnterTimestampMonotonic': 'inactive_enter_mono_us', 'InactiveExitTimestampMonotonic': 'inactive_exit_mono_us', 'ExecMainStartTimestampMonotonic': 'exec_main_start_mono_us', 'ExecMainExitTimestampMonotonic': 'exec_main_exit_mono_us'}
SDURATION = {'WatchdogUSec': 'watchdog_us', 'TimeoutStartUSec': 'timeout_start_us', 'TimeoutStopUSec': 'timeout_stop_us', 'TimeoutAbortUSec': 'timeout_abort_us', 'RestartUSec': 'restart_us'}
GROUPS = (('Id', 'InvocationID') + tuple(SNUM) + ('LoadState', 'ActiveState', 'SubState', 'Result'), ('Id', 'Restart', 'OOMPolicy') + tuple(SDURATION))
COMMANDS = tuple(('systemctl', 'show', '--no-pager', '--property=' + ','.join(p), *UNITS) for p in GROUPS)
MAX_OUTPUT = 256 * 1024
MAX_LINE = 16 * 1024


def number(value):
    if not isinstance(value, str) or not re.fullmatch(r'[0-9]{1,20}', value):
        raise ValueError()
    n = int(value)
    if n > 0xffffffffffffffff:
        raise ValueError()
    return n


def identifier(value, prefix):
    if not isinstance(value, str) or not re.fullmatch('[0-9a-fA-F]{32}', value):
        raise ValueError()
    return {prefix + '_hi': int(value[:16], 16), prefix + '_lo': int(value[16:], 16)}


def journal_record(raw):
    if len(raw) > MAX_LINE:
        raise ValueError()
    data = json.loads(raw, object_pairs_hook=unique_pairs)
    if not isinstance(data, dict) or set(data) - (set(JFIELDS) | {'__CURSOR'}):
        raise ValueError()
    if data.get('_PID') != '1' or data.get('UNIT') not in UNITS:
        raise ValueError()
    fields = {'unit': UNITS[data['UNIT']]}
    for key, value in data.items():
        if not isinstance(value, str):
            raise ValueError()
        if key in JNUM:
            fields[JNUM[key]] = number(value)
        elif key in JENUM:
            name, mapping = JENUM[key]
            fields[name] = mapping[value]
        elif key in JIDS:
            fields.update(identifier(value, JIDS[key]))
    message_id = data.get('MESSAGE_ID', '')
    kind = MESSAGE_IDS.get(message_id)
    if kind is None:
        if message_id:
            identifier(message_id, 'unused')
        if not ('JOB_TYPE' in data and 'JOB_RESULT' in data):
            raise ValueError()
        kind = 11
    fields['kind'] = kind
    return fields


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError()
        result[key] = value
    return result


def duration(value):
    if value == 'infinity':
        return 0xffffffffffffffff
    # systemctl formats usec properties as human-readable, fixed-unit timespans.
    units = {'us': 1, 'ms': 1000, 's': 1000000, 'min': 60000000, 'h': 3600000000, 'd': 86400000000, 'w': 604800000000}
    if value == '0':
        return 0
    total = 0
    for part in value.split(' '):
        match = re.fullmatch(r'([0-9]{1,12})(?:\.([0-9]{1,6}))?(us|ms|s|min|h|d|w)', part)
        if not match:
            raise ValueError()
        whole, fraction, unit = match.groups()
        total += int(whole) * units[unit] + (int(fraction) * units[unit] // 10**len(fraction) if fraction else 0)
    if total > 0xffffffffffffffff:
        raise ValueError()
    return total


def snapshots(raw, group):
    text = raw.decode('utf-8', errors='strict')
    if len(raw) > MAX_OUTPUT:
        raise ValueError()
    records = []
    for block in text.strip().split('\n\n'):
        if not block:
            continue
        data = unique_pairs(line.split('=', 1) for line in block.splitlines())
        if set(data) - set(GROUPS[group]) or data.get('Id') not in UNITS:
            raise ValueError()
        fields = {'unit': UNITS[data['Id']], 'group': group}
        for key, value in data.items():
            if value == '' or key == 'Id':
                continue
            if key in SENUM:
                name, mapping = SENUM[key]
                fields[name] = mapping[value]
            elif key in SNUM:
                fields[SNUM[key]] = number(value)
            elif key in SDURATION:
                fields[SDURATION[key]] = duration(value)
            elif key == 'InvocationID':
                fields.update(identifier(value, 'invocation'))
        records.append(fields)
    if not records:
        raise ValueError()
    return records


def bounded_command(argv, timeout=2, limit=MAX_OUTPUT):
    """Drain at most limit+1 bytes; no stderr, argv or exception text is logged."""
    process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    output = bytearray()
    done = threading.Event()
    def read():
        try:
            while len(output) <= limit:
                block = process.stdout.read(min(4096, limit + 1 - len(output)))
                if not block:
                    break
                output.extend(block)
        finally:
            done.set()
    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    try:
        if not done.wait(timeout):
            raise TimeoutError()
        if len(output) > limit:
            raise OverflowError()
        code = process.wait(timeout=max(0.01, timeout / 10))
        if code:
            raise ChildProcessError()
        return bytes(output)
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=1)
        reader.join(timeout=1)
        process.stdout.close()


class Observer:
    def __init__(self, emit, command=bounded_command, clock=time.monotonic):
        self.emit, self.command, self.clock = emit, command, clock
        self.previous = {}
        self.last_tick = clock()
        self.failed = set()

    def sample(self):
        now = self.clock()
        elapsed = max(0, int((now - self.last_tick) * 1000))
        if elapsed > 10000:
            self.emit('state_gap', layer=1, duration_ms=elapsed)
        self.last_tick = now
        for group, argv in enumerate(COMMANDS):
            try:
                records = snapshots(self.command(argv), group)
                if group in self.failed:
                    self.emit('state_health', layer=group + 2, healthy=True)
                    self.failed.remove(group)
                for fields in records:
                    key = (group, fields['unit'])
                    if self.previous.get(key) != fields:
                        self.emit('state_snapshot', **fields)
                        self.previous[key] = fields
            except (OSError, ValueError, KeyError, TypeError, OverflowError, subprocess.SubprocessError):
                self.failed.add(group)
                self.emit('state_command_failure', probe=group, reason=1)
        self.emit('state_health', layer=1, healthy=not bool(self.failed))

    def journal(self, raw):
        try:
            fields = journal_record(raw)
        except (ValueError, KeyError, TypeError, UnicodeError, RecursionError):
            self.emit('state_reject', layer=5, reason=1)
            return
        self.emit('state_journal', **fields)


class JournalFeed:
    """Bounded reader queue. Oversized lines drain without accumulating content."""
    def __init__(self, process):
        self.process = process
        self.queue = queue.Queue(maxsize=256)
        self.dropped = 0
        self.lock = threading.Lock()
        self.done = threading.Event()
        self.thread = threading.Thread(target=self.read, daemon=True)
        self.thread.start()

    def drop(self):
        with self.lock:
            self.dropped += 1

    def take_dropped(self):
        with self.lock:
            count = self.dropped
            self.dropped = 0
            return count

    def read(self):
        try:
            while True:
                line = self.process.stdout.readline(MAX_LINE + 1)
                if not line:
                    break
                if len(line) > MAX_LINE:
                    self.drop()
                    while line and not line.endswith(b'\n'):
                        line = self.process.stdout.readline(MAX_LINE + 1)
                    continue
                try:
                    self.queue.put_nowait(line)
                except queue.Full:
                    self.drop()
        except (OSError, ValueError):
            self.drop()
        finally:
            self.done.set()

    def close(self):
        if self.process.poll() is None:
            self.process.kill()
        self.process.wait(timeout=2)
        self.thread.join(timeout=2)
        self.process.stdout.close()


def main():
    from trace_common import TraceLog
    log = TraceLog('state')
    observer = Observer(log.emit)
    stop = threading.Event()
    def request_stop(signum, frame):
        stop.set()
    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    feed = None
    retry = next_sample = next_start = 0
    gap_start = time.monotonic()
    log.emit('state_gap', layer=6, duration_ms=0)  # private systemd socket is not observed
    try:
        while not stop.is_set():
            now = time.monotonic()
            if now >= next_sample:
                observer.sample()
                next_sample = time.monotonic() + 5
            if feed is None and now >= next_start:
                try:
                    process = subprocess.Popen(JOURNAL_ARGV, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
                    feed = JournalFeed(process)
                    log.emit('state_health', layer=5, healthy=True)
                    log.emit('state_gap', layer=5, duration_ms=max(0, int((now - gap_start) * 1000)))
                except OSError:
                    retry = min(retry + 1, 5)
                    next_start = now + min(30, 2**retry)
                    log.emit('state_command_failure', probe=2, reason=1)
            if feed:
                for unused in range(256):
                    try:
                        observer.journal(feed.queue.get_nowait())
                    except queue.Empty:
                        break
                dropped = feed.take_dropped()
                if dropped:
                    log.emit('state_gap', layer=5, lost_records=dropped)
                if feed.done.is_set() and feed.queue.empty():
                    code = feed.process.poll()
                    log.emit('state_journal_exit', code=abs(code or 0), signal=bool(code is not None and code < 0))
                    feed.close()
                    feed = None
                    gap_start = now
                    retry = min(retry + 1, 5)
                    next_start = now + min(30, 2**retry)
                    log.emit('state_health', layer=5, healthy=False)
            stop.wait(0.1)
    finally:
        try:
            if feed:
                feed.close()
        finally:
            log.emit('state_gap', layer=5, duration_ms=0)
            log.emit('observer_stop', reason=1)
            log.close()


if __name__ == '__main__':
    main()
