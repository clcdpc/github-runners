"""Check failure accounting and aggregation without SQL/Hyper-V or third-party packages."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

import measure
import report


class MeasurementTests(unittest.TestCase):
    def test_wait_counter_reset_is_rejected(self):
        before = [{"wait_type": name, "wait_time_ms": 10, "waiting_tasks_count": 2, "signal_wait_time_ms": 1}
                  for name in measure.WAITS]
        after = copy.deepcopy(before)
        after[0]["wait_time_ms"] = 0
        with self.assertRaises(ValueError):
            measure.wait_delta(before, after)
        with self.assertRaises(ValueError):
            measure.wait_delta(before, [])

    def test_trx_requires_exact_three_passing_tests(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "tests.trx"
            def trx(names, outcome="Passed"):
                path.write_text('<TestRun xmlns="urn:trx"><Results>' + ''.join(
                    f'<UnitTestResult testName="{name}" outcome="{outcome}" duration="00:00:01.250"/>'
                    for name in names) + '</Results></TestRun>')
            trx(measure.TESTS)
            self.assertEqual(sum(t["seconds"] for t in measure.parse_trx(path)), 3.75)
            for names, outcome in (([], "Passed"), (measure.TESTS, "Failed"),
                                   (measure.TESTS, "NotExecuted"), (measure.TESTS[:2], "Passed"),
                                   (measure.TESTS + ("UnwantedTest",), "Passed")):
                trx(names, outcome)
                with self.assertRaises(ValueError):
                    measure.parse_trx(path)

    def test_weighted_cpu_does_not_average_interval_means(self):
        self.assertEqual(report.weighted([
            {"interval_seconds": 1, "cpu": {"busy_percent": 100}},
            {"interval_seconds": 9, "cpu": {"busy_percent": 0}}], "cpu", "busy_percent"), 10)

    def test_blocked_run_cannot_recommend_redesign(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "execution.json").write_text(json.dumps({"status": "blocked", "errors": ["Hyper-V denied"], "cleanup": "No VM created"}))
            text = report.build_report(root)
            self.assertIn("**INCONCLUSIVE**", text)
            self.assertIn("0/0/0", text)
            self.assertIn("Hyper-V denied", text)
            self.assertNotIn("0.000", text)

    def test_failed_repetition_is_not_a_fast_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "A-1").mkdir()
            (root / "A-1/result.json").write_text('{"success": false}')
            text = report.build_report(root)
            self.assertIn("0/0/0", text)
            self.assertIn("A-1: missing successful", text)

    def test_sql_only_validation_is_not_counted_as_benchmark_repetitions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "preparation").mkdir()
            (root / "execution.json").write_text('{"status":"complete","mode":"ValidateSql","errors":[],"cleanup":"removed"}')
            (root / "preparation/result.json").write_text(json.dumps({
                "success": True, "image_digest": "validated-digest", "readiness": {"select_1": 1}}))
            text = report.build_report(root)
            self.assertIn("0/0/0", text)
            self.assertIn("**INCONCLUSIVE**", text)
            self.assertIn("SQL-only initialization: SELECT 1 and database placement checks passed", text)
            self.assertIn("validated-digest", text)

    def test_complete_comparison_uses_medians_and_weighted_io(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "execution.json").write_text('{"status":"complete","errors":[],"cleanup":"removed"}')
            for scenario, seconds in (("A", 100), ("B", 70), ("C", 90)):
                for repetition in (1, 2, 3):
                    path = root / f"{scenario}-{repetition}"
                    (path / "asap").mkdir(parents=True)
                    wait = {name: {"wait_time_ms": 1} for name in measure.WAITS}
                    focused = {"seconds": 5, "database_init_seconds": 1, "writes_seconds": 3, "drop_seconds": 1,
                               "waits": wait, "files": [{"num_of_writes": 10, "io_stall_write_ms": 20}]}
                    value = {"success": True, "image_digest": "same-digest", "label": path.name, "device": "/dev/sdb",
                             "sql_start_seconds": 4, "sql": focused, "first_use_sql": focused,
                             "tests": {"seconds": seconds + repetition - 2, "waits": wait,
                                       "individual": [{"test": name, "outcome": "Passed", "seconds": seconds / 3} for name in measure.TESTS],
                                       "fixtures": [{"operation": "dacpac_deploy_including_database_creation", "duration_seconds": 3}]}}
                    host = {"success": True, "scenario": scenario, "resetSeconds": 2, "bootReadySeconds": 10,
                            "layout": [{"slot": 1, "bytes": 100}], "endLayout": [{"slot": 1, "bytes": 200}]}
                    (path / "result.json").write_text(json.dumps(value))
                    (path / "host.json").write_text(json.dumps(host))
                    samples = {"kind": "host", "interval_seconds": 1, "disks": {"sdb": {"write_operations": 10, "write_time_ms": 20}}}
                    (path / "asap/samples.jsonl").write_text(json.dumps(samples) + "\n")
            text = report.build_report(root)
            self.assertIn("**PROCEED to focused full-VM validation**", text)
            self.assertIn("3/3/3", text)
            self.assertIn("| Migration tests (s) | 100.000 | 70.000 | 90.000 |", text)
            self.assertIn("| Guest benchmark disk write await (ms/op) | 2.000 | 2.000 | 2.000 |", text)
            self.assertIn("66.7%", text)
            self.assertIn("-30.000", text)
            for path in root.glob('B-*/result.json'):
                value = json.loads(path.read_text())
                value['tests']['seconds'] += 28
                path.write_text(json.dumps(value))
            text = report.build_report(root)
            self.assertIn("**DO NOT PROCEED", text)
            self.assertNotIn("Checkpoint loses", text)
            self.assertIn("below the materiality threshold", text)


if __name__ == "__main__":
    unittest.main()
