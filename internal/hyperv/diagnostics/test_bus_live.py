#!/usr/bin/env python3
"""Opt-in real D-Bus integration, ONLY inside a disposable Docker container.

Run: CLC_DIAGNOSTICS_DISPOSABLE=1 python3 -B -m unittest discover \
    -s internal/hyperv/diagnostics -p test_bus_live.py -v
Needs python3-dbus, python3-gi and dbus-daemon. Starts a PRIVATE session-configured
bus and a FAKE systemd Manager, never PID 1, the real system bus, or service
control. This validates dbus-python/GLib wire behavior, NOT a real guest systemd
lifecycle or production readiness. No network, installation or containers are
launched by this test. The observer wrapper only redirects its private log sink.
"""
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest

ALLOWED = sys.platform.startswith('linux') and os.environ.get('CLC_DIAGNOSTICS_DISPOSABLE') == '1' and Path('/.dockerenv').is_file()
HERE = Path(__file__).resolve()
GUEST = HERE.parents[1] / 'guest'
CANARY = 'CLC_PRIVATE_PAYLOAD_CANARY_739d6341'
SYSTEMD = 'org.freedesktop.systemd1'
MANAGER = SYSTEMD + '.Manager'
ROOT = '/org/freedesktop/systemd1'
UNIT_PATH = ROOT + '/unit/clc_2drunner_2eservice'


def require_disposable():
    if not ALLOWED:
        raise SystemExit('Private-bus integration requires the explicit disposable-container guard.')


def serve(address):
    require_disposable()
    import dbus
    import dbus.service
    from dbus.mainloop.glib import DBusGMainLoop
    from gi.repository import GLib
    DBusGMainLoop(set_as_default=True)
    connection = dbus.bus.BusConnection(address)
    name = dbus.service.BusName(SYSTEMD, bus=connection)

    class FakeUnit(dbus.service.Object):
        @dbus.service.signal('org.freedesktop.DBus.Properties', signature='sa{sv}as')
        def PropertiesChanged(self, interface, changed, invalidated):
            pass

    unit = FakeUnit(name, UNIT_PATH)

    class FakeManager(dbus.service.Object):
        def __init__(self):
            super().__init__(name, ROOT)
            self.subscribers = set()
            self.next_job = 40

        @dbus.service.method(MANAGER, in_signature='', out_signature='', sender_keyword='sender')
        def Subscribe(self, sender=None):
            self.subscribers.add(sender)

        @dbus.service.signal(MANAGER, signature='uos')
        def JobNew(self, job_id, job_path, unit_name):
            pass

        @dbus.service.signal(MANAGER, signature='uoss')
        def JobRemoved(self, job_id, job_path, unit_name, result):
            pass

        def job(self, unit_name):
            if str(unit_name) != 'clc-runner.service':
                raise dbus.exceptions.DBusException(CANARY, name='org.freedesktop.systemd1.NoSuchUnit')
            self.next_job += 1
            job_id = self.next_job
            job_path = ROOT + '/job/' + str(job_id)
            def complete():
                # Deliberately gate lifecycle signals on Subscribe, like v255.
                if self.subscribers:
                    self.JobNew(job_id, job_path, unit_name)
                    unit.PropertiesChanged(SYSTEMD + '.Unit', {'ActiveState': 'inactive', 'Environment': [CANARY]}, [])
                    self.JobRemoved(job_id, job_path, unit_name, 'done')
                return False
            GLib.timeout_add(30, complete)
            return dbus.ObjectPath(job_path)

        @dbus.service.method(MANAGER, in_signature='ss', out_signature='o')
        def StopUnit(self, unit_name, mode):
            return self.job(unit_name)

        @dbus.service.method(MANAGER, in_signature='ss', out_signature='o')
        def RestartUnit(self, unit_name, mode):
            return self.job(unit_name)

        @dbus.service.method(MANAGER, in_signature='s', out_signature='s')
        def UnknownCanary(self, value):
            return value

        @dbus.service.method(MANAGER, in_signature='sba(sv)', out_signature='')
        def SetUnitProperties(self, unit_name, runtime, properties):
            pass

    manager = FakeManager()
    loop = GLib.MainLoop()
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGTERM, lambda: (loop.quit() or False))
    try:
        loop.run()
    finally:
        connection.close()


def run_observer(directory):
    require_disposable()
    import runpy
    sys.path.insert(0, str(GUEST))
    import trace_common
    trace_common.DIRECTORY = Path(directory)
    runpy.run_path(str(GUEST / 'trace-bus.py'), run_name='__main__')


def records(path):
    if not path.exists():
        return []
    result = []
    for line in path.read_text().splitlines():
        if line.startswith('CLC_TRACE '):
            fields = dict(item.split('=', 1) for item in line.split()[1:])
            result.append({key: int(value) if value.isdecimal() else value for key, value in fields.items()})
    return result


@unittest.skipUnless(ALLOWED, 'requires Linux, /.dockerenv and CLC_DIAGNOSTICS_DISPOSABLE=1')
class RealBusTest(unittest.TestCase):
    def test_private_bus_observer_wire_contract(self):
        import dbus
        processes = []
        client = None
        def wait_for(predicate, description, timeout=10):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if predicate():
                    return
                time.sleep(0.03)
            self.fail(description)
        with tempfile.TemporaryDirectory(prefix='clc-bus-test-') as temporary:
            root = Path(temporary)
            logdir = root / 'trace'
            log = logdir / 'bus.log'
            address = 'unix:path=' + str(root / 'bus.sock')
            env = os.environ.copy()
            env.update(DBUS_SYSTEM_BUS_ADDRESS=address, DBUS_SESSION_BUS_ADDRESS=address,
                       CLC_TEST_SECRET=CANARY, PYTHONDONTWRITEBYTECODE='1')
            env.pop('DBUS_STARTER_ADDRESS', None)
            env.pop('DBUS_STARTER_BUS_TYPE', None)
            with (root / 'observer.stdout').open('wb') as stdout, (root / 'observer.stderr').open('wb') as stderr:
                try:
                    daemon = subprocess.Popen(['dbus-daemon', '--session', '--nofork', '--nopidfile',
                                               '--address=' + address, '--print-address=1'],
                                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env)
                    processes.append(daemon)
                    self.assertTrue(select.select([daemon.stdout], [], [], 10)[0], 'private daemon did not start')
                    actual_address = daemon.stdout.readline().decode().strip()
                    self.assertTrue(actual_address.startswith(address), 'private daemon address mismatch')
                    client = dbus.bus.BusConnection(actual_address)
                    server = subprocess.Popen([sys.executable, '-B', str(HERE), '--server', actual_address],
                                              env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    processes.append(server)
                    wait_for(lambda: client.name_has_owner(SYSTEMD), 'fake manager did not claim private name')
                    observer = subprocess.Popen([sys.executable, '-B', str(HERE), '--observer', str(logdir)],
                                                env=env, stdout=stdout, stderr=stderr)
                    processes.append(observer)
                    wait_for(lambda: any(row.get('event') == 'bus_health' and row.get('state') == 2 and
                                         row.get('subscribe_ready') == 1 for row in records(log)),
                             'real observer never became subscribed and monitor-ready')
                    manager = dbus.Interface(client.get_object(SYSTEMD, ROOT, introspect=False), MANAGER)
                    jobs = [int(str(manager.StopUnit('clc-runner.service', CANARY, timeout=3)).rsplit('/', 1)[1]),
                            int(str(manager.RestartUnit('clc-runner.service', CANARY, timeout=3)).rsplit('/', 1)[1])]
                    self.assertEqual(str(manager.UnknownCanary(CANARY, timeout=3)), CANARY)
                    manager.SetUnitProperties('clc-runner.service', False, [('Environment', dbus.Array([CANARY], signature='s'))], timeout=3)
                    wait_for(lambda: len([row for row in records(log) if row.get('event') == 'bus_reply']) == 2 and
                             len([row for row in records(log) if row.get('event') == 'bus_job' and row.get('phase') == 2]) == 2,
                             'real replies or subscribed lifecycle signals missing')
                    observer.send_signal(signal.SIGTERM)
                    self.assertEqual(observer.wait(timeout=5), 0, 'observer did not stop gracefully')
                    rows = records(log)
                    attempts = [row for row in rows if row['event'] == 'bus_attempt']
                    replies = [row for row in rows if row['event'] == 'bus_reply']
                    self.assertEqual(len(attempts), 2)
                    self.assertEqual({row['action'] for row in attempts}, {2, 3})
                    sender_major, sender_minor = map(int, client.get_unique_name()[1:].split('.'))
                    for attempt in attempts:
                        self.assertEqual((attempt['identity'], attempt['pid'], attempt['uid']), (1, os.getpid(), os.getuid()))
                        self.assertEqual((attempt['sender_major'], attempt['sender_minor']), (sender_major, sender_minor))
                        self.assertEqual(attempt['proc_known'], 1)
                        self.assertGreater(attempt['pid_start_ticks'], 0)
                        self.assertEqual(attempt['clock_ticks'], os.sysconf('SC_CLK_TCK'))
                        matching = [row for row in replies if row['request_serial'] == attempt['request_serial']]
                        self.assertEqual(len(matching), 1)
                        self.assertEqual(matching[0]['outcome'], 1)
                        self.assertEqual(matching[0]['observed_mono_ns'], attempt['observed_mono_ns'])
                    self.assertEqual(sorted(row['job_id'] for row in replies), sorted(jobs))
                    for job_id in jobs:
                        self.assertEqual({row['phase'] for row in rows if row['event'] == 'bus_job' and row['job_id'] == job_id}, {1, 2})
                    self.assertTrue(any(row['event'] == 'bus_state' for row in rows))
                    self.assertTrue(any(row['event'] == 'bus_health' and row['state'] == 7 for row in rows))
                    self.assertTrue(all(row.get('sink_rejected') == 0 for row in rows))
                    stdout.flush()
                    stderr.flush()
                    for path in (log, root / 'observer.stdout', root / 'observer.stderr'):
                        self.assertNotIn(CANARY, path.read_text(), 'private payload leaked into observer output')
                finally:
                    if client is not None:
                        client.close()
                    for process in reversed(processes):
                        if process.poll() is None:
                            process.terminate()
                            try:
                                process.wait(timeout=5)
                            except subprocess.TimeoutExpired:
                                process.kill()
                                process.wait(timeout=5)
                        if process.stdout is not None:
                            process.stdout.close()


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--server':
        serve(sys.argv[2])
    elif len(sys.argv) == 3 and sys.argv[1] == '--observer':
        run_observer(sys.argv[2])
    else:
        unittest.main()
