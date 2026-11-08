#!/usr/bin/env python3
"""Read-only guest collector. Output is sanitized and byte-bounded by the host.

No environment, process command lines, systemctl Environment/ExecStart, workspace
files, runner credentials or raw D-Bus/audit payloads are requested. Raw output
never goes to a temporary file. Each invocation is bounded by remote timeout and
an independent host deadline. Source failures return nonzero with partial data.
"""
import gzip
import os
import re
import stat
import subprocess
import sys
import zlib

JOURNAL = ["/usr/bin/journalctl", "--boot=0", "--utc", "--no-pager", "--output=short-iso-precise"]
SERVICES = ["clc-runner.service", "cloud-final.service", "docker.service", "containerd.service", "clc-trace-bus.service", "clc-trace-kernel.service", "clc-trace-state.service", "systemd-oomd.service"]
PACKAGE_UNITS = ["apt-daily.service", "apt-daily-upgrade.service", "unattended-upgrades.service"]
PROPERTIES = ["Id", "ActiveState", "SubState", "Result", "MainPID", "ExecMainPID", "ExecMainCode", "ExecMainStatus", "NRestarts", "InvocationID", "ActiveEnterTimestamp", "InactiveEnterTimestamp", "StateChangeTimestamp", "Requires", "Wants", "BindsTo", "PartOf", "TriggeredBy", "Before", "After"]
MAX_FILE_SCAN = 32 << 20
MAX_DIRECTORY_ENTRIES = 1024


def note(message):
    print("CLC_DIAGNOSTICS " + message, flush=True)


def command(args):
    # Fixed argv, no shell, no pager, no inherited secret-bearing environment.
    result = subprocess.run(args, env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C", "SYSTEMD_COLORS": "0", "SYSTEMD_PAGER": ""}, stderr=subprocess.STDOUT)
    if result.returncode:
        note("source command failed exit=" + str(result.returncode))
    return result.returncode != 0


def open_directory(path):
    # O_NOFOLLOW on every component, not only the final filename. Never traverse
    # a symlinked _diag, /var/log/apt directory, device or guest workspace.
    descriptor = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for component in path.strip("/").split("/"):
            next_descriptor = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = next_descriptor
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def copy_file(directory, name):
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    except FileNotFoundError:
        note("optional file absent: " + name)
        return False
    except OSError:
        note("file unavailable or unsafe: " + name)
        return True
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            note("non-regular or multiply-linked file refused: " + name)
            return True
        note("file " + name)
        # Omit oversized files instead of cutting raw bytes through a secret.
        # Read from byte zero so the host sees every multiline opening context.
        if info.st_size > MAX_FILE_SCAN:
            note("oversized file omitted; file scan limit reached")
            return True
        remaining = MAX_FILE_SCAN if name.endswith(".gz") else info.st_size
        with os.fdopen(fd, "rb", closefd=False) as raw:
            stream = gzip.GzipFile(fileobj=raw) if name.endswith(".gz") else raw
            while remaining:
                line = stream.readline(min(65537, remaining))
                if not line:
                    break
                remaining -= len(line)
                if not line.endswith(b"\n") or len(line) > 65536:
                    # A live writer may be midway through a secret. Do not flush
                    # a raw partial line or continue past an unknown opener.
                    note("incomplete or oversized line; remainder of file omitted")
                    return True
                sys.stdout.buffer.write(line)
            if name.endswith(".gz") and remaining == 0 and stream.read(1):
                note("decompressed scan limit; remainder of file omitted")
                return True
        sys.stdout.buffer.flush()
        return False
    except (OSError, EOFError, zlib.error):
        note("file read failed; partial evidence retained")
        return True
    finally:
        os.close(fd)


def files(path, names):
    try:
        directory = open_directory(path)
    except OSError:
        note("directory unavailable or unsafe: " + path)
        return True
    try:
        failed = False
        for name in names:
            failed = copy_file(directory, name) or failed
        return failed
    finally:
        os.close(directory)


def runner_logs(kind):
    try:
        directory = open_directory("/home/runner/_diag")
    except OSError:
        note("runner diagnostics directory unavailable or unsafe")
        return True
    try:
        names = []
        with os.scandir(directory) as entries:
            for index, entry in enumerate(entries):
                if index >= MAX_DIRECTORY_ENTRIES:
                    note("directory entry limit reached; file selection incomplete")
                    return True
                if re.fullmatch(kind + r"_[0-9]{8}-[0-9]{6}-utc\.log", entry.name):
                    names.append(entry.name)
        failed = False
        for name in sorted(names)[-2:]:
            failed = copy_file(directory, name) or failed
        if not names:
            note("no matching " + kind + " logs")
        if len(names) > 2:
            note("only newest two " + kind + " files selected")
        return failed
    finally:
        os.close(directory)


def main(source):
    note("source=" + source + " boot=current timestamps=UTC")
    if source in ("trace-state", "trace-bus", "trace-kernel"):
        name = source.removeprefix("trace-")
        return files("/var/log/clc-diagnostics", [name + ".log.1", name + ".log"])
    if source == "service-state":
        return command(["/usr/bin/systemctl", "show", "--no-pager"] + ["--property=" + p for p in PROPERTIES] + SERVICES)
    if source == "services":
        return command(JOURNAL + [arg for unit in SERVICES for arg in ["--unit", unit]])
    if source == "kernel":
        return command(JOURNAL + ["--dmesg"])
    if source == "auth":
        return command(JOURNAL + ["SYSLOG_IDENTIFIER=sshd", "SYSLOG_IDENTIFIER=sshd-session", "SYSLOG_IDENTIFIER=sudo", "SYSLOG_IDENTIFIER=systemd-logind", "SYSLOG_IDENTIFIER=clc-service-control"])
    if source == "packages":
        return command(JOURNAL + [arg for unit in PACKAGE_UNITS for arg in ["--unit", unit]])
    # Oldest first: per-source tail retention must favor current records.
    if source == "dpkg-files":
        return files("/var/log", ["dpkg.log.1", "dpkg.log"])
    if source == "apt-files":
        return files("/var/log/apt", ["term.log.1.gz", "history.log.1.gz", "term.log", "history.log"])
    if source == "unattended-files":
        return files("/var/log/unattended-upgrades", ["unattended-upgrades-dpkg.log.1.gz", "unattended-upgrades.log.1.gz", "unattended-upgrades-dpkg.log.1", "unattended-upgrades.log.1", "unattended-upgrades-dpkg.log", "unattended-upgrades.log"])
    if source in ("runner", "worker"):
        return runner_logs(source.capitalize())
    if source == "boot":
        # No -n limit: scan the current boot, keeping a bounded sanitized tail on
        # the host. Truncation and unavailable journal history are not completeness.
        return command(JOURNAL)
    raise ValueError("unknown diagnostic source")


if __name__ == "__main__":
    try:
        sys.exit(1 if main(sys.argv[1]) else 0)
    except (OSError, ValueError, IndexError):
        note("collector failed; source unavailable")
        sys.exit(1)
