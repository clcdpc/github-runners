"""Isolated tests: no real journal, service control, guest files or privilege changes."""
import gzip
import importlib.util
import io
import os
from pathlib import Path
import stat
import sys
import types
import unittest
import zlib
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parent

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

collect = load("collect", ROOT / "collect.py")
with patch.dict(sys.modules, {"syslog": types.SimpleNamespace(LOG_PID=1, LOG_DAEMON=2, LOG_NOTICE=3, openlog=Mock(), syslog=Mock())}):
    control = load("control", ROOT.parent / "guest/service-control.py")

class Output(io.StringIO):
    def __init__(self):
        super().__init__()
        self.buffer = io.BytesIO()

class CollectorTests(unittest.TestCase):
    def test_full_boot_and_allowlisted_commands(self):
        with patch.object(collect, "command", return_value=False) as run, patch.object(collect, "note"):
            for source in ("boot", "kernel", "services", "auth", "packages", "service-state"):
                self.assertFalse(collect.main(source))
            commands = [call.args[0] for call in run.call_args_list]
            self.assertEqual(commands[0], collect.JOURNAL)
            for command in commands[:5]:
                self.assertIn("--boot=0", command)
                self.assertNotIn("-n", command)
            self.assertNotIn("--property=Environment", commands[-1])
            self.assertNotIn("--property=ExecStart", commands[-1])
            with self.assertRaises(ValueError): collect.main("arbitrary-secret-command")

    def test_command_status_and_no_inherited_environment(self):
        with patch.object(collect.subprocess,"run",return_value=types.SimpleNamespace(returncode=12)) as run, patch.object(collect,"note"):
            self.assertTrue(collect.command(["/usr/bin/journalctl"]))
            self.assertEqual(set(run.call_args.kwargs["env"]), {"PATH","LC_ALL","SYSTEMD_COLORS","SYSTEMD_PAGER"})
            self.assertNotIn("shell",run.call_args.kwargs)

    def test_directory_no_follow_each_component(self):
        # POSIX syscall flags are checked even when this test runs on Windows.
        with patch.object(collect.os,"O_DIRECTORY",0x10000,create=True), patch.object(collect.os,"O_NOFOLLOW",0x20000,create=True), patch.object(collect.os,"open",side_effect=[10,11,12,13]) as opened, patch.object(collect.os,"close") as closed:
            self.assertEqual(collect.open_directory("/home/runner/_diag"),13)
            for call in opened.call_args_list[1:]:
                self.assertTrue(call.args[1] & 0x20000)
                self.assertIn("dir_fd",call.kwargs)
            self.assertEqual([c.args[0] for c in closed.call_args_list],[10,11,12])
        with patch.object(collect.os,"O_DIRECTORY",0x10000,create=True), patch.object(collect.os,"O_NOFOLLOW",0x20000,create=True), patch.object(collect.os,"open",side_effect=[10,OSError("symlink refused")]), patch.object(collect.os,"close") as closed:
            with self.assertRaises(OSError): collect.open_directory("/home")
            closed.assert_called_once_with(10)

    def copy_fixture(self, payload, mode=stat.S_IFREG, links=1, size=None, name="Runner_20261107-200000-utc.log"):
        output=Output()
        with patch.object(collect.os,"O_NOFOLLOW",0x20000,create=True), patch.object(collect.os,"O_NONBLOCK",0x800,create=True), patch.object(collect.os,"open",return_value=9) as opened, patch.object(collect.os,"close"), patch.object(collect.os,"fstat",return_value=types.SimpleNamespace(st_mode=mode,st_nlink=links,st_size=len(payload) if size is None else size)), patch.object(collect.os,"fdopen",return_value=io.BytesIO(payload)), patch.object(collect.sys,"stdout",output):
            failed=collect.copy_file(3,name)
            self.assertTrue(opened.call_args.args[1] & 0x20000)
        return failed,output.buffer.getvalue(),output.getvalue()

    def test_file_safety_and_incomplete_lines(self):
        failed,data,_=self.copy_fixture(b"complete line\npartial-private-value")
        self.assertTrue(failed); self.assertEqual(data,b"complete line\n")
        for kwargs in ({"mode":stat.S_IFIFO},{"links":2},{"size":collect.MAX_FILE_SCAN+1}):
            failed,data,_=self.copy_fixture(b"private value\n",**kwargs)
            self.assertTrue(failed); self.assertEqual(data,b"")
        failed,data,_=self.copy_fixture(b"private-value"*7000+b"\n")
        self.assertTrue(failed); self.assertEqual(data,b"")
        failed,data,_=self.copy_fixture(b"normal first\nnormal last\n")
        self.assertFalse(failed); self.assertEqual(data,b"normal first\nnormal last\n")

    def test_compressed_rotation_and_decompression_limit(self):
        failed,data,_=self.copy_fixture(gzip.compress(b"prior package event\n"),name="history.log.1.gz")
        self.assertFalse(failed); self.assertEqual(data,b"prior package event\n")
        with patch.object(collect,"MAX_FILE_SCAN",256):
            failed,data,_=self.copy_fixture(gzip.compress(b"complete package event\n"*100),name="history.log.1.gz")
            self.assertTrue(failed); self.assertLessEqual(len(data),256)
            self.assertTrue(data.endswith(b"\n"))
        with patch.object(collect.gzip,"GzipFile",side_effect=zlib.error("invalid compressed stream")):
            failed,data,_=self.copy_fixture(b"broken compressed data",name="history.log.1.gz")
            self.assertTrue(failed); self.assertEqual(data,b"")
        with patch.object(collect,"open_directory",return_value=3),patch.object(collect.os,"close"),patch.object(collect,"copy_file",side_effect=[True,False]) as copy:
            self.assertTrue(collect.files("/var/log/apt",["history.log.1.gz","history.log"]))
            self.assertEqual([c.args[1] for c in copy.call_args_list],["history.log.1.gz","history.log"])
        with patch.object(collect,"files",return_value=False) as files,patch.object(collect,"note"):
            for source in ("dpkg-files","apt-files","unattended-files"):
                collect.main(source)
                names=files.call_args.args[1]
                first_current=next(i for i,name in enumerate(names) if not name.endswith((".1",".gz")))
                self.assertTrue(all(not n.endswith((".1",".gz")) for n in names[first_current:]))
            collect.main("apt-files")
            self.assertIn("history.log.1.gz",files.call_args.args[1])

    def test_newest_file_selection_and_bounded_enumeration(self):
        names=["Runner_20261107-200000-utc.log","Runner_20261107-200001-utc.log","Runner_20261107-200002-utc.log","Worker_20261107-200001-utc.log","Runner_../../secret.log",".credentials"]
        class Entries:
            def __enter__(self): return iter(types.SimpleNamespace(name=n) for n in names)
            def __exit__(self,*args): pass
        with patch.object(collect,"open_directory",return_value=3), patch.object(collect.os,"close"), patch.object(collect.os,"scandir",return_value=Entries()), patch.object(collect,"copy_file",return_value=False) as copy, patch.object(collect,"note"):
            self.assertFalse(collect.runner_logs("Runner"))
            self.assertEqual([c.args[1] for c in copy.call_args_list],names[1:3])
            names.extend(["ignored"]*collect.MAX_DIRECTORY_ENTRIES)
            copy.reset_mock()
            self.assertTrue(collect.runner_logs("Runner")); copy.assert_not_called()

class ControlTests(unittest.TestCase):
    def test_request_result_correlation_and_no_secret_arguments(self):
        with patch.object(control.os,"getuid",return_value=1001,create=True), patch.object(control.os,"geteuid",return_value=0,create=True), patch.object(control,"emit") as emit, patch.object(control.subprocess,"run",return_value=types.SimpleNamespace(returncode=23)) as run:
            self.assertEqual(control.main(["stop","image-seal"]),23)
            run.assert_called_once_with(["/usr/bin/systemctl","stop","clc-runner.service"])
            messages=[c.args[0] for c in emit.call_args_list]
            self.assertIn("event=request",messages[0]);self.assertIn("event=result",messages[1])
            self.assertEqual(messages[0].split("request=")[1].split()[0],messages[1].split("request=")[1].split()[0])
            self.assertIn("uid=1001 euid=0",messages[0]);self.assertIn("exit=23",messages[1])
            first_id=messages[0].split("request=")[1].split()[0]
            self.assertEqual(control.main(["start","clone-boot","--no-block"]),23)
            self.assertNotEqual(first_id,emit.call_args_list[-1].args[0].split("request=")[1].split()[0])
            run.reset_mock();emit.reset_mock()
            with patch.object(control.sys,"stderr",new_callable=io.StringIO) as errors:
                self.assertEqual(control.main(["stop","image-seal","token=private"]),2)
                self.assertNotIn("private",errors.getvalue())
            run.assert_not_called();emit.assert_not_called()

    def test_logging_failure_does_not_change_action(self):
        with patch.object(control.os,"getuid",return_value=0,create=True),patch.object(control.os,"geteuid",return_value=0,create=True),patch.object(control.syslog,"openlog",side_effect=OSError("unavailable")),patch.object(control.subprocess,"run",return_value=types.SimpleNamespace(returncode=0)) as run:
            self.assertEqual(control.main(["stop","image-seal"]),0)
            run.assert_called_once()

if __name__=="__main__": unittest.main()
