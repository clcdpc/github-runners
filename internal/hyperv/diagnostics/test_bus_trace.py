import importlib.util
import pathlib
import unittest
import types
from unittest.mock import patch

PATH = pathlib.Path(__file__).resolve().parents[1] / 'guest' / 'trace-bus.py'
spec = importlib.util.spec_from_file_location('trace_bus', PATH)
bus = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bus)


class Message:
    def __init__(self, **values):
        self.values = dict(type=1, interface=bus.MANAGER, member='RestartUnit',
                           path=bus.ROOT, signature='ss', sender=':1.77',
                           destination=bus.SYSTEMD, serial=17, reply_serial=17, error_name='SECRET_ERROR',
                           args_list=['clc-runner.service', 'replace'])
        self.values.update(values)
    def __getattr__(self, name):
        if name.startswith('get_'):
            return lambda: self.values[name[4:]]
        raise AttributeError(name)


class BusTraceTest(unittest.TestCase):
    def setUp(self):
        self.events = []
        self.lookups = []
        def credentials(sender):
            self.lookups.append(sender)
            return {'ProcessID': 120, 'UnixUserID': 0}
        self.observer = bus.Observer(lambda event, **fields: self.events.append((event, fields)), credentials,
                                     {bus.SYSTEMD: ':1.1', bus.LOGIN: ':1.2'}, lambda pid: 999, clock_ticks=100)

    def test_attempt_identity_and_no_payload(self):
        with patch.object(bus.time, 'monotonic_ns', return_value=123456):
            self.observer.handle(Message(args_list=['clc-runner.service', 'SECRET_MODE']))
        self.assertEqual(self.events, [('bus_attempt', dict(action=3, unit=1, identity=1,
                          pid=120, uid=0, pid_start_ticks=999, clock_ticks=100, proc_known=1, request_serial=17,
                          sender_major=1, sender_minor=77, observed_mono_ns=123456))])
        self.assertEqual(self.lookups, [':1.77', ':1.77'])
        self.assertNotIn('SECRET', repr(self.events))

    def test_unknown_units_and_arbitrary_methods_excluded(self):
        for message in (Message(args_list=['SECRET.service', 'replace']),
                        Message(member='StartTransientUnit', signature='ssa(sv)a(sa(sv))'),
                        Message(member='SetUnitProperties', signature='sba(sv)'),
                        Message(signature='ssa{sv}')):
            self.observer.handle(message)
        self.assertEqual(self.events, [])
        self.assertEqual(self.lookups, [])

    def test_unit_and_logind_signatures(self):
        path = next(p for p, unit in bus.PATHS.items() if unit == 'docker.service')
        self.observer.handle(Message(interface=bus.UNIT, path=path, member='Stop', signature='s'))
        self.observer.handle(Message(interface=bus.LOGIN_MANAGER, destination=bus.LOGIN,
                                     path='/org/freedesktop/login1', member='Reboot', signature='b'))
        self.observer.handle(Message(interface=bus.LOGIN_MANAGER, destination=bus.LOGIN,
                                     path='/org/freedesktop/login1', member='Reboot', signature='a{sv}'))
        self.assertEqual([fields['action'] for _, fields in self.events], [2, 8])

    def test_sender_unknown_failure_and_pid_reuse(self):
        self.observer.handle(Message(sender='forged-secret'))
        self.assertEqual(self.events[-1][1]['identity'], 0)
        self.assertEqual(self.lookups, [])
        def vanished(sender):
            raise RuntimeError('SECRET_PAYLOAD')
        self.observer.credentials = vanished
        self.observer.handle(Message())
        self.assertEqual(self.events[-1][1]['identity'], 0)
        self.assertNotIn('SECRET', repr(self.events))
        self.observer.credentials = lambda sender: {'ProcessID': 120, 'UnixUserID': 0}
        values = iter([999, 1000])
        self.observer.start_reader = lambda pid: next(values)
        self.observer.handle(Message())
        self.assertEqual(self.events[-1][1]['identity'], 1)
        self.assertEqual(self.events[-1][1]['proc_known'], 0)
        self.assertNotIn('pid_start_ticks', self.events[-1][1])

    def test_no_identity_cache(self):
        self.observer.handle(Message())
        self.observer.credentials = lambda sender: {}
        self.observer.handle(Message())
        self.assertEqual(self.events[-1][1]['identity'], 0)

    def test_jobs_remain_separate_from_coalesced_attempts(self):
        self.observer.handle(Message())
        self.observer.handle(Message(serial=18))
        self.observer.handle(Message(type=4, member='JobNew', sender=':1.1', signature='uos',
                                     args_list=[31, '/org/freedesktop/systemd1/job/31', 'clc-runner.service']))
        self.observer.handle(Message(type=4, member='JobRemoved', sender=':1.1', signature='uoss',
                                     args_list=[31, '/org/freedesktop/systemd1/job/31', 'clc-runner.service', 'done']))
        self.assertEqual([event for event, _ in self.events], ['bus_attempt', 'bus_attempt', 'bus_job', 'bus_job'])
        self.assertEqual(self.events[-1][1], dict(phase=2, job_id=31, unit=1, result=1))
        self.assertNotIn('pid', self.events[-1][1])

    def test_untrusted_signal_and_unknown_result(self):
        msg = dict(type=4, member='JobRemoved', signature='uoss', args_list=[31, 'SECRET_PATH', 'clc-runner.service', 'SECRET_RESULT'])
        self.observer.handle(Message(**msg))
        self.assertEqual(self.events, [])
        self.observer.handle(Message(sender=':1.1', **msg))
        self.assertEqual(self.events[-1][1]['result'], 0)
        self.assertNotIn('SECRET', repr(self.events))
        self.observer.owners.clear()
        self.observer.handle(Message(sender=':1.1', **msg))
        self.assertEqual(len(self.events), 1)

    def test_properties_only_fixed_state(self):
        path = next(iter(bus.PATHS))
        self.observer.handle(Message(type=4, sender=':1.1', interface=bus.PROPERTIES,
                                     path=path, member='PropertiesChanged', signature='sa{sv}as',
                                     args_list=[bus.UNIT, {'ActiveState': 'failed', 'Environment': ['SECRET']}, []]))
        self.assertEqual(self.events, [('bus_state', dict(unit=1, state=4))])
        self.assertNotIn('SECRET', repr(self.events))

    def test_malformed_message_error_has_no_text(self):
        self.observer.handle(Message(args_list=[]))
        self.assertEqual(self.events, [('bus_health', dict(state=4, error=2, gap_private=1))])

    def test_name_owner_change_and_missing_destination(self):
        self.observer.handle(Message(type=4, sender='org.freedesktop.DBus',
            interface='org.freedesktop.DBus', member='NameOwnerChanged',
            path='/org/freedesktop/DBus', signature='sss',
            args_list=[bus.SYSTEMD, ':1.1', ':1.90']))
        self.assertEqual(self.observer.owners[bus.SYSTEMD], ':1.90')
        self.assertEqual(self.events[-1], ('bus_health', dict(state=6, error=0, gap_private=1, evicted=0)))
        self.observer.owners.clear()
        self.observer.handle(Message(destination=None))
        self.assertEqual(len(self.events), 1)

    def test_runtime_monitor_denial_and_disconnect_are_explicit(self):
        for reason in ('disconnect', 'denied', 'sigterm', 'sigint', 'subscribe_denied', 'owner_restart', 'already_subscribed', 'subscribe_recovery'):
            denied = reason == 'denied'
            events, connections, calls, signal_handlers, closed, subscriptions = [], [], [], {}, [], []
            heartbeats = []
            class FakeBus:
                def __init__(self, private):
                    self.private = private
                    connections.append(self)
                def close(self):
                    closed.append(self)
                    if hasattr(self, 'disconnect'):
                        self.disconnect(self)
                    raise RuntimeError(bus.os.environ['TRACE_TEST_SECRET'])
                def set_exit_on_disconnect(self, value):
                    self.exit = value
                def get_object(self, *args, **kwargs):
                    return self
                def call_on_disconnection(self, callback):
                    self.disconnect = callback
                def add_message_filter(self, callback):
                    self.filter = callback
                def Subscribe(self, timeout):
                    subscriptions.append(self)
                    if reason == 'subscribe_denied' or (reason == 'subscribe_recovery' and len(subscriptions) == 1):
                        raise RuntimeError(bus.os.environ['TRACE_TEST_SECRET'])
                    if reason == 'already_subscribed':
                        class AlreadySubscribed(Exception):
                            def get_dbus_name(self):
                                return 'org.freedesktop.systemd1.AlreadySubscribed'
                        raise AlreadySubscribed(bus.os.environ['TRACE_TEST_SECRET'])
                def GetNameOwner(self, name, timeout):
                    return ':1.1' if name == bus.SYSTEMD else ':1.2'
                def BecomeMonitor(self, rules, flags, timeout):
                    calls.append((self, rules, timeout))
                    if denied:
                        raise RuntimeError(bus.os.environ['TRACE_TEST_SECRET'])
            class Loop:
                def run(self):
                    if reason == 'subscribe_recovery':
                        heartbeats[0]()
                    if reason == 'owner_restart':
                        connections[1].filter(connections[1], Message(type=4, sender='org.freedesktop.DBus',
                            interface='org.freedesktop.DBus', member='NameOwnerChanged',
                            path='/org/freedesktop/DBus', signature='sss',
                            args_list=[bus.SYSTEMD, ':1.1', ':1.90']))
                    if reason in ('sigterm', 'sigint'):
                        signal_handlers[bus.signal.SIGTERM if reason == 'sigterm' else bus.signal.SIGINT]()
                    else:
                        connections[1].disconnect(connections[1])
                def quit(self):
                    pass
            dbus = types.ModuleType('dbus')
            dbus.SystemBus = FakeBus
            dbus.Interface = lambda obj, interface: obj
            dbus.Array = lambda values, signature: values
            dbus.UInt32 = int
            glib = types.ModuleType('dbus.mainloop.glib')
            glib.DBusGMainLoop = lambda **kwargs: None
            repo = types.ModuleType('gi.repository')
            def add_signal(priority, signum, callback):
                signal_handlers[signum] = callback
                return int(signum)
            repo.GLib = types.SimpleNamespace(MainLoop=Loop, timeout_add_seconds=lambda seconds, callback: (heartbeats.append(callback) or 99),
                PRIORITY_DEFAULT=0, unix_signal_add=add_signal,
                MainContext=types.SimpleNamespace(default=lambda: types.SimpleNamespace(find_source_by_id=lambda source: None)))
            common = types.ModuleType('trace_common')
            common.TraceLog = lambda layer: types.SimpleNamespace(emit=lambda event, **fields: events.append((event, fields)), close=lambda: closed.append('log'))
            with patch.dict('sys.modules', {'dbus': dbus, 'dbus.mainloop': types.ModuleType('dbus.mainloop'),
                'dbus.mainloop.glib': glib, 'gi': types.ModuleType('gi'), 'gi.repository': repo, 'trace_common': common}):
                with patch.dict('os.environ', {'TRACE_TEST_SECRET': 'SECRET_ENV_EXCEPTION_PAYLOAD'}):
                    self.assertEqual(bus.main(), 0 if reason in ('sigterm', 'sigint') else 1)
            self.assertEqual(len(connections), 2)
            self.assertIs(calls[0][0], connections[1])
            self.assertTrue(all(c.private and not c.exit for c in connections))
            states = [fields['state'] for _, fields in events]
            expected = {'denied': [1, 4], 'subscribe_denied': [1, 4, 2, 3], 'owner_restart': [1, 2, 6, 3], 'subscribe_recovery': [1, 4, 2, 5, 3]}
            self.assertEqual(states, expected.get(reason, [1, 2, 7] if reason in ('sigterm', 'sigint') else [1, 2, 3]))
            self.assertTrue(all(connection is connections[0] for connection in subscriptions))
            self.assertEqual(len(subscriptions), 0 if denied else 2 if reason in ('owner_restart', 'subscribe_recovery') else 1)
            if not denied:
                ready = next(fields for _, fields in events if fields['state'] == 2)
                self.assertEqual(ready['subscribe_ready'], 0 if reason in ('subscribe_denied', 'subscribe_recovery') else 1)
                if reason == 'subscribe_recovery':
                    self.assertEqual(next(fields['subscribe_ready'] for _, fields in events if fields['state'] == 5), 1)
                if reason in ('subscribe_denied', 'subscribe_recovery'):
                    self.assertEqual(ready['error'], 7)
            self.assertEqual(closed, [connections[1], connections[0], 'log'])
            self.assertEqual(set(signal_handlers), {bus.signal.SIGTERM, bus.signal.SIGINT})
            self.assertNotIn('SECRET', repr(events))

    def test_reply_correlation_and_coalescing(self):
        for cookie in (17, 18):
            self.observer.handle(Message(serial=cookie))
        for cookie in (17, 18):
            self.observer.handle(Message(type=2, sender=':1.1', destination=':1.77', reply_serial=cookie,
                signature='o', args_list=['/org/freedesktop/systemd1/job/44']))
        replies = [fields for event, fields in self.events if event == 'bus_reply']
        self.assertEqual([fields['job_id'] for fields in replies], [44, 44])
        self.assertEqual([fields['request_serial'] for fields in replies], [17, 18])
        self.assertTrue(all(fields['outcome'] == 1 for fields in replies))

    def test_unmatched_reply_body_never_decoded_and_errors_private(self):
        class Unreadable(Message):
            def get_args_list(self):
                raise AssertionError('Unmatched body was accessed')
        self.observer.handle(Unreadable(type=2, sender=':1.1', destination=':1.77', signature='o'))
        self.assertEqual(self.events, [])
        self.observer.handle(Message())
        self.observer.handle(Unreadable(type=2, sender=':1.666', destination=':1.77', signature='o'))
        self.assertEqual(len(self.events), 1)
        self.observer.handle(Unreadable(type=3, sender=':1.1', destination=':1.77', signature='s'))
        self.assertEqual(self.events[-1][0], 'bus_reply')
        self.assertEqual(self.events[-1][1]['outcome'], 2)
        self.assertEqual(self.events[-1][1]['error'], 0)
        self.assertNotIn('SECRET', repr(self.events))

    def test_pending_bound_expiry_and_owner_reset(self):
        clock = [0]
        self.observer.now = lambda: clock[0]
        self.observer.capacity = 1
        self.observer.handle(Message())
        self.observer.handle(Message(serial=18))
        self.assertEqual(len(self.observer.pending), 1)
        self.assertTrue(any(fields.get('error') == 6 for _, fields in self.events))
        clock[0] = 31
        self.observer.expire()
        self.assertEqual(len(self.observer.pending), 0)
        self.assertEqual(self.events[-1][1]['error'], 5)
        self.observer.handle(Message())
        self.observer.handle(Message(type=4, sender='org.freedesktop.DBus',
            interface='org.freedesktop.DBus', member='NameOwnerChanged',
            path='/org/freedesktop/DBus', signature='sss',
            args_list=[bus.SYSTEMD, ':1.1', ':1.90']))
        self.assertEqual(len(self.observer.pending), 0)
        self.assertEqual(self.events[-1][1]['evicted'], 1)

    def test_sender_parts_and_observation_precede_credentials(self):
        def credentials(sender):
            self.assertEqual(bus.time.monotonic_ns(), 200)
            return {}
        self.observer.credentials = credentials
        for sender in (':1.77', ':1.78'):
            with patch.object(bus.time, 'monotonic_ns', side_effect=[100, 200]):
                self.observer.handle(Message(sender=sender))
        for sender in (':1.77', ':1.78'):
            with patch.object(bus.time, 'monotonic_ns', return_value=300):
                self.observer.handle(Message(type=2, sender=':1.1', destination=sender,
                    signature='o', args_list=['/org/freedesktop/systemd1/job/44']))
        attempts = [fields for event, fields in self.events if event == 'bus_attempt']
        replies = [fields for event, fields in self.events if event == 'bus_reply']
        self.assertEqual([fields['sender_minor'] for fields in attempts], [77, 78])
        self.assertEqual([fields['sender_minor'] for fields in replies], [77, 78])
        self.assertTrue(all(fields['request_serial'] == 17 and fields['sender_major'] == 1 and
                            fields['identity'] == 0 and fields['observed_mono_ns'] == 100
                            for fields in attempts + replies))
        self.assertTrue(all(fields['reply_observed_mono_ns'] == 300 for fields in replies))

    def test_rules_are_fixed_and_narrow(self):
        rules = bus.match_rules()
        self.assertTrue(rules)
        self.assertTrue(all(('interface=' in rule and 'path=' in rule and 'member=' in rule) or ("type='method_return'" in rule or "type='error'" in rule) for rule in rules))
        self.assertFalse(any('SetUnitProperties' in rule or 'StartTransientUnit' in rule for rule in rules))
        self.assertEqual(len(rules), len(set(rules)))


if __name__ == '__main__':
    unittest.main()
