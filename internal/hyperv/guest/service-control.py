#!/usr/bin/env python3
"""Attribute repository-owned requests only; never intercept systemctl or D-Bus.

Logging is best effort and does not change service-control behavior. No argv,
environment, arbitrary reason text or process command line is logged. This is
not attribution for independent admin/package-manager/dependency requests.
"""
import os
import subprocess
import sys
import syslog
import time
import uuid

UNIT = "clc-runner.service"
REQUESTS = {
    ("start", "clone-boot", "--no-block"): ["start", "--no-block", UNIT],
    ("stop", "image-seal"): ["stop", UNIT],
}


def emit(message):
    try:
        syslog.openlog("clc-service-control", syslog.LOG_PID, syslog.LOG_DAEMON)
        syslog.syslog(syslog.LOG_NOTICE, message)
    except (OSError, ValueError):
        pass


def main(arguments):
    command = REQUESTS.get(tuple(arguments))
    if command is None:
        # Do not echo untrusted input, including secret-shaped extra arguments.
        print("Unsupported service-control request", file=sys.stderr)
        return 2
    action, source = arguments[:2]
    request = uuid.uuid4().hex
    fields = ("request=" + request + " unit=" + UNIT + " action=" + action +
              " source=" + source + " pid=" + str(os.getpid()) +
              " ppid=" + str(os.getppid()) + " uid=" + str(os.getuid()) +
              " euid=" + str(os.geteuid()))
    started = time.monotonic()
    emit("event=request " + fields)
    try:
        result = subprocess.run(["/usr/bin/systemctl"] + command).returncode
    except OSError:
        result = 127
    emit("event=result " + fields + " exit=" + str(result) +
         " elapsed_ms=" + str(int((time.monotonic() - started) * 1000)))
    return result


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
