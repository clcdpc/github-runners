import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

path = Path(__file__).resolve().parents[1] / 'guest/trace_common.py'
spec = importlib.util.spec_from_file_location('trace_common', path)
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)

class CommonTests(unittest.TestCase):
    def test_rejects_all_arbitrary_data(self):
        with tempfile.TemporaryDirectory() as root:
            log = common.TraceLog('bus', directory=root)
            for event, fields in [('unknown-secret', {}), ('bus_attempt', {'unit': 'private-value'}),
                                  ('bus_attempt', {'unit': {'secret': 'value'}}),
                                  ('bus_attempt', {'source': 1}), ('bus_attempt', {'pid': 2**65})]:
                self.assertFalse(log.emit(event, **fields))
            self.assertTrue(log.emit('bus_attempt', pid=123, unit=1, identity=True))
            log.close()
            text = (Path(root) / 'bus.log').read_text()
            self.assertNotIn('private', text); self.assertNotIn('secret', text)
            self.assertIn('rejected=5', text); self.assertIn('pid=123', text)

    def test_rotation_and_separate_signal_budget(self):
        with tempfile.TemporaryDirectory() as root:
            with patch.object(common.time, 'monotonic_ns', return_value=100_000_000_000):
                log = common.TraceLog('kernel', directory=root, max_bytes=4096, rate=30)
                for _ in range(50): log.emit('process_fork', pid=123)
                self.assertEqual(log.dropped, 20)
                self.assertTrue(log.emit('signal_generated', signal=15))
                log.close()
            self.assertLessEqual((Path(root) / 'kernel.log').stat().st_size, 4096)
            self.assertLessEqual((Path(root) / 'kernel.log.1').stat().st_size, 4096)
            text = (Path(root) / 'kernel.log').read_text()
            self.assertIn('signal_generated', text); self.assertIn('dropped=20', text)
            self.assertGreater(log.rotations, 0)

    def test_disk_failure_and_short_writes(self):
        with tempfile.TemporaryDirectory() as root:
            log = common.TraceLog('state', directory=root)
            real_write = os.write
            with patch.object(common.os, 'write', side_effect=lambda fd,data: real_write(fd,data[:5])):
                self.assertTrue(log.emit('state_health', state=1))
            with patch.object(common.os, 'write', side_effect=OSError('private exception')):
                self.assertFalse(log.emit('state_health', state=2))
            self.assertEqual(log.io_errors, 1)
            self.assertTrue(log.emit('state_health', state=3)); log.close()
            text=(Path(root)/'state.log').read_text()
            self.assertNotIn('private',text);self.assertIn('io_errors=1',text)

    def test_closed_or_unknown_sink(self):
        with self.assertRaises(ValueError): common.TraceLog('unknown')
        with tempfile.TemporaryDirectory() as root:
            log=common.TraceLog('state',directory=root);log.close()
            self.assertFalse(log.emit('state_health',state=1))

    def test_preexisting_snapshot_never_records_comm_or_command(self):
        class Log:
            def __init__(self): self.events=[]
            def emit(self,event,**fields): self.events.append((event,fields))
        with tempfile.TemporaryDirectory() as root:
            proc=Path(root);(proc/'123').mkdir()
            values=['S','1']+['0']*17+['12345']
            (proc/'123'/'stat').write_text('123 (tiny-private-value) '+' '.join(values))
            (proc/'123'/'status').write_text('Uid:\t1000\t1000\t1000\t1000\n')
            log=Log()
            with patch.object(common.os,'sysconf',return_value=100,create=True):
                common.process_snapshot(log,proc)
            self.assertEqual(log.events[0][0],'process_snapshot')
            self.assertEqual(log.events[0][1]['program_class'],0)
            self.assertEqual(log.events[0][1]['pid_start_ticks'],12345)
            self.assertNotIn('tiny-private-value',repr(log.events))

if __name__=='__main__': unittest.main()
