#!/usr/bin/env python3
"""Strict, bounded metadata sink shared by optional guest observers.

No arbitrary strings, exception messages, argv, environment or payload objects
are accepted. Numeric fields are schema-checked by their observer before emit.
Files stay private and rotate independently. Every record carries loss/rotation
counters: missing evidence must never be interpreted as proof of no caller.
"""
import os
from pathlib import Path
import re
import stat
import threading
import time
import uuid

DIRECTORY = Path('/var/log/clc-diagnostics')
SOURCES = {'kernel', 'bus', 'state'}
EVENTS = {
    'observer_start', 'observer_ready', 'observer_heartbeat', 'observer_stop',
    'coverage_gap', 'tracer_lost', 'tracer_exit', 'parser_error', 'attach_failed',
    'process_fork', 'process_exec', 'process_exit', 'signal_generated',
    'process_snapshot', 'process_snapshot_gap',
    'bus_health', 'bus_attempt', 'bus_reply', 'bus_job', 'bus_state',
    'state_snapshot', 'state_journal', 'state_health', 'state_gap',
    'state_command_failure', 'state_journal_exit', 'state_reject',
    'sink_heartbeat',
}
KEY = re.compile(r'^[a-z][a-z0-9_]{0,39}$')
RESERVED = {'v', 'source', 'event', 'boot_id', 'session_id', 'seq', 'monotonic_ns',
            'utc_ns', 'sink_rotations', 'sink_dropped', 'sink_rejected', 'sink_io_errors'}
PROGRAMS = {name: i + 1 for i, name in enumerate((
    'systemd', 'systemctl', 'sudo', 'sshd', 'apt', 'apt-get', 'dpkg',
    'needrestart', 'deb-systemd-inv', 'invoke-rc.d', 'service',
    'unattended-upgr', 'Runner.Listener', 'Runner.Worker', 'runner.sh',
    'bash', 'sh', 'systemd-oomd', 'reboot', 'shutdown'))}


def boot_id():
    try:
        value = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
        if re.fullmatch(r'[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}', value):
            return value
    except (OSError, UnicodeError):
        pass
    # Do not invent a boot identity. Session identity still prevents collisions.
    return 'unknown'


class TraceLog:
    def __init__(self, source, directory=None, max_bytes=2 << 20, rate=2000):
        if source not in SOURCES or max_bytes < 4096 or rate < 1:
            raise ValueError('invalid trace sink configuration')
        self.source = source
        self.directory = DIRECTORY if directory is None else Path(directory)
        self.path = self.directory / (source + '.log')
        self.max_bytes, self.rate = max_bytes, rate
        self.boot, self.session = boot_id(), uuid.uuid4().hex
        self.seq = self.rotations = self.dropped = self.rejected = self.io_errors = 0
        self.fd = None
        self.size = 0
        self.lock = threading.Lock()
        self.epoch = -1
        self.ordinary_count = self.critical_count = 0
        self.last_sync = self.last_error = 0
        self.closed = False
        self._open()

    def _open(self):
        self.directory.mkdir(mode=0o700, parents=False, exist_ok=True)
        info = self.directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or self.directory.is_symlink():
            raise OSError('unsafe trace directory')
        if os.name == 'posix' and (info.st_uid != os.geteuid() or info.st_mode & 0o077):
            raise OSError('trace directory is not private')
        flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
        flags |= getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_CLOEXEC', 0)
        fd = os.open(self.path, flags, 0o600)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise OSError('unsafe trace file')
            if os.name == 'posix' and (info.st_uid != os.geteuid() or info.st_mode & 0o077):
                raise OSError('trace file is not private')
        except BaseException:
            os.close(fd)
            raise
        self.fd, self.size = fd, info.st_size

    def _rotate(self):
        os.close(self.fd)
        self.fd = None
        os.replace(self.path, self.path.with_suffix('.log.1'))
        self.rotations += 1
        self._open()

    def emit(self, event, **fields):
        with self.lock:
            if self.closed:
                return False
            # Completely reject untrusted string fields, nested data and large
            # integer values rather than attempting a redaction fallback.
            if event not in EVENTS or len(fields) > 40 or any(
                not KEY.fullmatch(k) or k in RESERVED or type(v) not in (int, bool)
                or not -(2**63) <= int(v) <= 2**64 - 1 for k, v in fields.items()
            ):
                self.rejected += 1
                return False
            now = time.monotonic_ns()
            second = now // 1_000_000_000
            if second != self.epoch:
                self.epoch = second
                self.ordinary_count = self.critical_count = 0
            # Reserve independent space for signal and health events so process
            # floods cannot consume their entire writer admission budget.
            critical = event == 'signal_generated' or any(x in event for x in ('health', 'heartbeat', 'gap', 'lost', 'failed'))
            if critical:
                self.critical_count += 1
                over = self.critical_count > 512
            else:
                self.ordinary_count += 1
                over = self.ordinary_count > self.rate
            if over:
                self.dropped += 1
                return False
            self.seq += 1
            base = dict(v=1, source=self.source, event=event, boot_id=self.boot,
                        session_id=self.session, seq=self.seq, monotonic_ns=now,
                        utc_ns=time.time_ns(), sink_rotations=self.rotations,
                        sink_dropped=self.dropped, sink_rejected=self.rejected, sink_io_errors=self.io_errors)
            line = ('CLC_TRACE ' + ' '.join(k + '=' + str(v) for k, v in base.items()) +
                    ''.join(' ' + k + '=' + str(int(v)) for k, v in sorted(fields.items())) + '\n').encode('ascii')
            if len(line) > 4096:
                self.rejected += 1
                return False
            try:
                if self.fd is None:
                    self._open()
                if self.size + len(line) > self.max_bytes:
                    self._rotate()
                    # Rotation counters belong to the file receiving this record.
                    line = line.replace(('sink_rotations=' + str(self.rotations - 1) + ' ').encode(),
                                        ('sink_rotations=' + str(self.rotations) + ' ').encode(), 1)
                remaining = memoryview(line)
                while remaining:
                    written = os.write(self.fd, remaining)
                    if written <= 0:
                        raise OSError('short trace write')
                    self.size += written
                    remaining = remaining[written:]
                if now - self.last_sync >= 10_000_000_000:
                    os.fsync(self.fd)
                    self.last_sync = now
                return True
            except OSError:
                self.io_errors += 1
                # A fixed message can survive disk failure in the service journal.
                # Never emit the error text, path or record payload as a fallback.
                if now - self.last_error >= 60_000_000_000:
                    try:
                        os.write(2, ('CLC_TRACE_IO_ERROR source=' + self.source + '\n').encode())
                    except OSError:
                        pass
                    self.last_error = now
                return False

    def close(self):
        with self.lock:
            self.closed = True
            if self.fd is not None:
                try:
                    os.fsync(self.fd)
                except OSError:
                    self.io_errors += 1
                finally:
                    os.close(self.fd)
                    self.fd = None


def process_snapshot(log, proc=Path('/proc'), maximum=4096):
    """Best-effort preexisting ancestry. No cmdline/environ or arbitrary names.

/proc start times have clock-tick resolution; do not claim an exact nanosecond
join to kernel task start_boottime. UID/parent can race; this is snapshot context,
not an authenticated origin of any service request.
"""
    try:
        ticks = os.sysconf('SC_CLK_TCK')
        entries = proc.iterdir()
        seen = rejected = 0
        for path in entries:
            if not path.name.isascii() or not path.name.isdecimal():
                continue
            seen += 1
            if seen > maximum:
                log.emit('process_snapshot_gap', reason=1, seen=seen)
                break
            try:
                with (path / 'stat').open('rb') as stream:
                    data = stream.read(8193)
                if len(data) > 8192:
                    raise ValueError('stat bound')
                before, marker, rest = data.rpartition(b') ')
                pid, separator, comm = before.partition(b' (')
                fields = rest.split()
                if not marker or not separator or int(pid) != int(path.name) or len(fields) < 20:
                    raise ValueError('stat shape')
                start, parent = int(fields[19]), int(fields[1])
                with (path / 'status').open('rb') as stream:
                    status = stream.read(65537)
                uid_match = re.search(rb'^Uid:\s+([0-9]+)\s', status, re.MULTILINE)
                if len(status) > 65536 or uid_match is None:
                    raise ValueError('status bound')
                with (path / 'stat').open('rb') as stream:
                    second = stream.read(8193)
                if second != data:
                    # Even harmless concurrent field changes make this snapshot
                    # less trustworthy. Skip instead of inventing stable ancestry.
                    rejected += 1
                    continue
                program = PROGRAMS.get(comm.decode('ascii', errors='replace'), 0)
                log.emit('process_snapshot', pid=int(pid), parent_pid=parent,
                         pid_start_ticks=start, clock_ticks=ticks,
                         uid=int(uid_match[1]), program_class=program)
            except (OSError, ValueError, IndexError, OverflowError):
                rejected += 1
        log.emit('process_snapshot_gap', reason=2, seen=min(seen, maximum), skipped=rejected)
    except (OSError, AttributeError, ValueError):
        log.emit('process_snapshot_gap', reason=3)
