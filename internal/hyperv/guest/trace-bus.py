#!/usr/bin/env python3
"""Passive, metadata-only public system-bus trace. Never proves job causality.

The systemd private socket is deliberately NOT observed. Job merging means an
attempt and a later job outcome are separate facts, not attribution evidence.
"""
import os
import collections
import time
import re
import signal

UNITS = {name: i + 1 for i, name in enumerate((
    'clc-runner.service', 'docker.service', 'containerd.service',
    'apt-daily.service', 'apt-daily-upgrade.service', 'unattended-upgrades.service',
    'shutdown.target', 'reboot.target', 'poweroff.target', 'halt.target',
    'kexec.target', 'soft-reboot.target', 'clc-trace-bus.service',
    'clc-trace-kernel.service', 'clc-trace-state.service'))}
ACTIONS = {name: i + 1 for i, name in enumerate((
    'Start', 'Stop', 'Restart', 'TryRestart', 'ReloadOrRestart',
    'ReloadOrTryRestart', 'Enqueue', 'Reboot', 'PowerOff', 'Halt',
    'Suspend', 'Hibernate', 'HybridSleep', 'SuspendThenHibernate'))}
RESULTS = {name: i + 1 for i, name in enumerate((
    'done', 'canceled', 'timeout', 'failed', 'dependency', 'skipped',
    'invalid', 'assert', 'unsupported', 'collected', 'once'))}
STATES = {name: i + 1 for i, name in enumerate((
    'active', 'reloading', 'inactive', 'failed', 'activating', 'deactivating',
    'maintenance', 'refreshing'))}
UNIQUE = re.compile(r'^:([0-9]{1,10})\.([0-9]{1,10})$')
SYSTEMD = 'org.freedesktop.systemd1'
MANAGER = SYSTEMD + '.Manager'
UNIT = SYSTEMD + '.Unit'
LOGIN = 'org.freedesktop.login1'
LOGIN_MANAGER = LOGIN + '.Manager'
JOB_PATH = re.compile(r'^/org/freedesktop/systemd1/job/([0-9]{1,10})$')
ERRORS = {name: i + 1 for i, name in enumerate(('org.freedesktop.DBus.Error.AccessDenied', 'org.freedesktop.DBus.Error.NoReply', 'org.freedesktop.DBus.Error.ServiceUnknown', 'org.freedesktop.systemd1.NoSuchUnit', 'org.freedesktop.systemd1.UnitMasked', 'org.freedesktop.systemd1.TransactionIsDestructive', 'org.freedesktop.systemd1.JobTypeNotApplicable'))}
PROPERTIES = 'org.freedesktop.DBus.Properties'
ROOT = '/org/freedesktop/systemd1'
PATHS = {ROOT + '/unit/' + ''.join(c if c.isalnum() and c.isascii() else '_%02x' % ord(c) for c in name): name for name in UNITS}


def number(value, maximum=0xffffffff):
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    return int(value) if 0 <= value <= maximum else None


def proc_start(pid):
    # Do not record comm, argv, environment, or any other proc contents.
    with open('/proc/%d/stat' % pid, encoding='utf-8') as stream:
        stat = stream.read(8192)
    ticks = int(stat[stat.rfind(')') + 2:].split()[19])
    return ticks


class Observer:
    def __init__(self, emit, credentials, owners, start_reader=proc_start, clock_ticks=None, now=time.monotonic, capacity=1024, ttl=30):
        self.on_owner_change = lambda name: None
        self.pending = collections.OrderedDict()
        self.now, self.capacity, self.ttl = now, capacity, ttl
        self.emit = emit
        self.credentials = credentials
        self.owners = owners
        self.start_reader = start_reader
        if clock_ticks is None:
            try:
                clock_ticks = os.sysconf('SC_CLK_TCK')
            except (AttributeError, OSError, ValueError):
                clock_ticks = 0
        self.clock_ticks = number(clock_ticks) or 0

    def identity(self, sender):
        # No cache: unique names must never survive a bus-daemon restart.
        if not isinstance(sender, str) or not UNIQUE.fullmatch(sender):
            return {'identity': 0}
        try:
            first = self.credentials(sender)
            pid, uid = number(first.get('ProcessID')), number(first.get('UnixUserID'))
            if pid is None or uid is None or pid == 0:
                return {'identity': 0}
            start = None
            try:
                candidate = number(self.start_reader(pid), 2**64 - 1)
                if candidate is not None and self.start_reader(pid) == candidate:
                    start = candidate
            except Exception:
                pass
            second = self.credentials(sender)
            if second.get('ProcessID') != pid or second.get('UnixUserID') != uid:
                return {'identity': 0}
            fields = {'identity': 1, 'pid': pid, 'uid': uid, 'proc_known': int(start is not None)}
            if start is not None:
                fields['pid_start_ticks'] = start
                fields['clock_ticks'] = self.clock_ticks
            return fields
        except Exception:
            return {'identity': 0}

    def expire(self):
        evicted = 0
        while self.pending and next(iter(self.pending.values()))[0] + self.ttl <= self.now():
            self.pending.popitem(last=False)
            evicted += 1
        if evicted:
            self.emit('bus_health', state=4, error=5, gap_private=1, evicted=evicted)

    def track(self, sender, cookie, fields, owner):
        if cookie is None or not isinstance(sender, str) or not UNIQUE.fullmatch(sender) or owner is None:
            return
        key = (sender, cookie)
        if key in self.pending:
            self.pending.pop(key)
        if len(self.pending) >= self.capacity:
            self.pending.popitem(last=False)
            self.emit('bus_health', state=4, error=6, gap_private=1, evicted=1)
        self.pending[key] = (self.now(), dict(fields), owner)

    def handle(self, message):
        observed_mono_ns = time.monotonic_ns()
        try:
            self.expire()
            self._handle(message, observed_mono_ns)
        except Exception:
            # Exception strings can contain an entire D-Bus message. Never log.
            self.emit('bus_health', state=4, error=2, gap_private=1)

    def _handle(self, message, observed_mono_ns):
        kind, interface, member = message.get_type(), message.get_interface(), message.get_member()
        path, signature = message.get_path(), str(message.get_signature())
        sender = message.get_sender()
        if kind in (2, 3):
            # Never decode a reply body until its authenticated sender and header
            # match one of our bounded, allowlisted pending requests.
            key = (message.get_destination(), number(message.get_reply_serial()))
            pending = self.pending.get(key)
            if pending is None or sender != pending[2]:
                return
            self.pending.pop(key)
            fields = dict(pending[1])
            fields['reply_observed_mono_ns'] = observed_mono_ns
            fields['outcome'] = 2 if kind == 3 else 1
            fields['error'] = ERRORS.get(message.get_error_name(), 0) if kind == 3 else 0
            if kind == 2 and signature == 'o':
                args = message.get_args_list()
                match = JOB_PATH.fullmatch(args[0]) if len(args) == 1 and isinstance(args[0], str) else None
                job_id = number(int(match.group(1))) if match else None
                if job_id is not None:
                    fields['job_id'] = job_id
                else:
                    fields['outcome'] = 3
            elif kind == 2 and signature != '':
                # EnqueueUnitJob's compound response is not persisted or parsed.
                fields['outcome'] = 3
            self.emit('bus_reply', **fields)
        elif kind == 4 and sender == 'org.freedesktop.DBus' and interface == 'org.freedesktop.DBus' and member == 'NameOwnerChanged' and path == '/org/freedesktop/DBus' and signature == 'sss':
            args = message.get_args_list()
            if args[0] in (SYSTEMD, LOGIN):
                if isinstance(args[2], str) and UNIQUE.fullmatch(args[2]):
                    self.owners[args[0]] = str(args[2])
                else:
                    self.owners.pop(args[0], None)
                evicted = len(self.pending)
                self.pending.clear()
                self.emit('bus_health', state=6, error=0, gap_private=1, evicted=evicted)
                self.on_owner_change(args[0])
        elif kind == 1:
            dest = message.get_destination()
            if not dest:
                return
            action, unit = None, None
            if dest in (SYSTEMD, self.owners.get(SYSTEMD)) and interface == MANAGER and path == ROOT:
                if member in tuple(a + 'Unit' for a in ACTIONS if a in ('Start', 'Stop', 'Restart', 'TryRestart', 'ReloadOrRestart', 'ReloadOrTryRestart')) and signature == 'ss':
                    args = message.get_args_list()
                    unit, action = UNITS.get(args[0]), ACTIONS.get(member[:-4])
                elif member == 'EnqueueUnitJob' and signature == 'sss':
                    args = message.get_args_list()
                    unit = UNITS.get(args[0])
                    action = {'start': 1, 'stop': 2, 'restart': 3, 'try-restart': 4, 'reload-or-restart': 5, 'reload-or-try-restart': 6}.get(args[1])
            elif dest in (SYSTEMD, self.owners.get(SYSTEMD)) and interface == UNIT and path in PATHS and member in ('Start', 'Stop', 'Restart', 'TryRestart', 'ReloadOrRestart', 'ReloadOrTryRestart') and signature == 's':
                unit, action = UNITS[PATHS[path]], ACTIONS[member]
            elif dest in (LOGIN, self.owners.get(LOGIN)) and interface == LOGIN_MANAGER and path == '/org/freedesktop/login1' and member in ('Reboot', 'PowerOff', 'Halt', 'Suspend', 'Hibernate', 'HybridSleep', 'SuspendThenHibernate') and signature == 'b':
                action, unit = ACTIONS[member], 0
            if action is not None and unit is not None:
                fields = {'action': action, 'unit': unit, 'observed_mono_ns': observed_mono_ns}
                sender_parts = UNIQUE.fullmatch(sender) if isinstance(sender, str) else None
                if sender_parts is not None:
                    fields['sender_major'] = int(sender_parts.group(1))
                    fields['sender_minor'] = int(sender_parts.group(2))
                fields.update(self.identity(sender))
                cookie = number(message.get_serial())
                if cookie is not None:
                    fields['request_serial'] = cookie
                self.emit('bus_attempt', **fields)
                self.track(sender, cookie, fields, self.owners.get(LOGIN if interface == LOGIN_MANAGER else SYSTEMD))
        elif kind == 4 and sender == self.owners.get(SYSTEMD) and sender is not None:
            if interface == MANAGER and path == ROOT and (member, signature) in (('JobNew', 'uos'), ('JobRemoved', 'uoss')):
                args = message.get_args_list()
                job_id, unit = number(args[0]), UNITS.get(args[2])
                if job_id is not None and unit is not None:
                    self.emit('bus_job', phase=1 if member == 'JobNew' else 2,
                              job_id=job_id, unit=unit,
                              result=RESULTS.get(args[3], 0) if member == 'JobRemoved' else 0)
            elif interface == PROPERTIES and member == 'PropertiesChanged' and path in PATHS and signature == 'sa{sv}as':
                args = message.get_args_list()
                if args[0] == UNIT and isinstance(args[1], dict):
                    state = STATES.get(args[1].get('ActiveState'))
                    if state is not None:
                        self.emit('bus_state', unit=UNITS[PATHS[path]], state=state)


def match_rules():
    rules = ["type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',path='/org/freedesktop/DBus',member='NameOwnerChanged',arg0='%s'" % name for name in (SYSTEMD, LOGIN)]
    for name in (SYSTEMD, LOGIN):
        for kind in ('method_return', 'error'):
            rules.append("type='%s',sender='%s'" % (kind, name))
    for member in ('StartUnit', 'StopUnit', 'RestartUnit', 'TryRestartUnit', 'ReloadOrRestartUnit', 'ReloadOrTryRestartUnit', 'EnqueueUnitJob'):
        rules.append("type='method_call',interface='%s',path='%s',member='%s'" % (MANAGER, ROOT, member))
    for path in PATHS:
        for member in ('Start', 'Stop', 'Restart', 'TryRestart', 'ReloadOrRestart', 'ReloadOrTryRestart'):
            rules.append("type='method_call',interface='%s',path='%s',member='%s'" % (UNIT, path, member))
        rules.append("type='signal',sender='%s',interface='%s',path='%s',member='PropertiesChanged',arg0='%s'" % (SYSTEMD, PROPERTIES, path, UNIT))
    for member in ('JobNew', 'JobRemoved'):
        rules.append("type='signal',sender='%s',interface='%s',path='%s',member='%s'" % (SYSTEMD, MANAGER, ROOT, member))
    for member in ('Reboot', 'PowerOff', 'Halt', 'Suspend', 'Hibernate', 'HybridSleep', 'SuspendThenHibernate'):
        rules.append("type='method_call',interface='%s',path='/org/freedesktop/login1',member='%s'" % (LOGIN_MANAGER, member))
    return rules


def main():
    from trace_common import TraceLog
    log = TraceLog('bus')
    lookup = monitor = GLib = None
    sources = []
    closing = False
    requested_stop = False
    owners = {}
    subscribed_owner = None
    def emit(event, **fields):
        if event == 'bus_health':
            fields['subscribe_ready'] = int(subscribed_owner is not None and owners.get(SYSTEMD) == subscribed_owner)
        log.emit(event, **fields)
    try:
        emit('bus_health', state=1, error=0, gap_private=1)
        import dbus
        from dbus.mainloop.glib import DBusGMainLoop
        from gi.repository import GLib
        DBusGMainLoop(set_as_default=True)
        # Never send credential requests on the monitor-only connection.
        lookup = dbus.SystemBus(private=True)
        monitor = dbus.SystemBus(private=True)
        lookup.set_exit_on_disconnect(False)
        monitor.set_exit_on_disconnect(False)
        daemon = dbus.Interface(lookup.get_object('org.freedesktop.DBus', '/org/freedesktop/DBus', introspect=False), 'org.freedesktop.DBus')
        for name in (SYSTEMD, LOGIN):
            try:
                owner = str(daemon.GetNameOwner(name, timeout=0.5))
                if UNIQUE.fullmatch(owner):
                    owners[name] = owner
            except Exception:
                pass
        if SYSTEMD not in owners:
            raise RuntimeError()
        def credentials(sender):
            return daemon.GetConnectionCredentials(sender, timeout=0.25)
        observer = Observer(emit, credentials, owners)
        def subscribe():
            nonlocal subscribed_owner
            owner = owners.get(SYSTEMD)
            if owner is not None and owner == subscribed_owner:
                return True
            subscribed_owner = None
            if owner is None:
                emit('bus_health', state=4, error=7, gap_private=1)
                return False
            try:
                manager = dbus.Interface(lookup.get_object(owner, ROOT, introspect=False), MANAGER)
                manager.Subscribe(timeout=0.5)
            except Exception as error:
                try:
                    already = error.get_dbus_name() == 'org.freedesktop.systemd1.AlreadySubscribed'
                except Exception:
                    already = False
                if not already:
                    emit('bus_health', state=4, error=7, gap_private=1)
                    return False
            subscribed_owner = owner
            return True
        observer.on_owner_change = lambda name: subscribe() if name == SYSTEMD else None
        loop = GLib.MainLoop()
        def disconnect(_connection):
            if closing or requested_stop:
                return
            owners.clear()
            observer.pending.clear()
            emit('bus_health', state=3, error=3, gap_private=1)
            loop.quit()
        def stop():
            nonlocal requested_stop
            if not requested_stop:
                requested_stop = True
                owners.clear()
                observer.pending.clear()
                emit('bus_health', state=7, error=0, gap_private=1)
                loop.quit()
            return False
        for signum in (signal.SIGTERM, signal.SIGINT):
            sources.append(GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signum, stop))
        lookup.call_on_disconnection(disconnect)
        monitor.call_on_disconnection(disconnect)
        monitor.add_message_filter(lambda _bus, message: observer.handle(message))
        monitoring = dbus.Interface(monitor.get_object('org.freedesktop.DBus', '/org/freedesktop/DBus', introspect=False), 'org.freedesktop.DBus.Monitoring')
        monitoring.BecomeMonitor(dbus.Array(match_rules(), signature='s'), dbus.UInt32(0), timeout=2.0)
        ready = subscribe()
        emit('bus_health', state=2, error=0 if ready else 7, gap_private=1)
        def heartbeat():
            observer.expire()
            # Re-resolve names so a restarted system manager cannot be misidentified.
            try:
                for name in (SYSTEMD, LOGIN):
                    try:
                        owner = str(daemon.GetNameOwner(name, timeout=0.5))
                        if UNIQUE.fullmatch(owner):
                            if owners.get(name) != owner:
                                evicted = len(observer.pending)
                                observer.pending.clear()
                                emit('bus_health', state=6, error=0, gap_private=1, evicted=evicted)
                            owners[name] = owner
                        else:
                            owners.pop(name, None)
                    except Exception:
                        if name in owners:
                            evicted = len(observer.pending)
                            observer.pending.clear()
                            emit('bus_health', state=6, error=4, gap_private=1, evicted=evicted)
                        owners.pop(name, None)
                ready = subscribe()
                emit('bus_health', state=5, error=0 if ready else 7, gap_private=1)
            except Exception:
                emit('bus_health', state=4, error=4, gap_private=1)
            return True
        sources.append(GLib.timeout_add_seconds(30, heartbeat))
        loop.run()
        return 0 if requested_stop else 1  # restart only unexpected disconnects
    except Exception:
        emit('bus_health', state=4, error=1, gap_private=1)
        return 1
    finally:
        closing = True
        for source in sources:
            try:
                # A one-shot signal source may already have removed itself.
                if GLib.MainContext.default().find_source_by_id(source) is not None:
                    GLib.source_remove(source)
            except Exception:
                pass
        for connection in (monitor, lookup):
            if connection is not None:
                try:
                    connection.close()
                except Exception:
                    pass
        try:
            log.close()
        except Exception:
            pass


if __name__ == '__main__':
    raise SystemExit(main())
