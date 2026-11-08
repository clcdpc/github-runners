#!/usr/bin/env bash
set -euo pipefail
umask 077
cd /home/runner
mkdir -p .clc
fail() { touch .clc/failed; }
trap fail ERR
test "$(id -u)" -ne 0
docker info >/dev/null
touch .clc/guest-ready
while ! test -s .clc/jit; do sleep 1; done
export ACTIONS_RUNNER_INPUT_JITCONFIG
ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat .clc/jit)
rm -f .clc/jit .clc/jit.pending
touch .clc/jit-consumed
# Runner.Listener consumes and clears ACTIONS_RUNNER_INPUT_* before job execution.
# No restart loop: JIT permits exactly one job in this entire VM lifetime.
set +e
./bin/Runner.Listener run > >(tee /home/runner/.clc/listener.log) 2>&1 &
listener=$!
unset ACTIONS_RUNNER_INPUT_JITCONFIG
while kill -0 "$listener" 2>/dev/null; do
  if grep -qF 'Listening for Jobs' .clc/listener.log; then touch .clc/listening; break; fi
  sleep 1
done
wait "$listener"
result=$?
set -e
touch .clc/listener-exited
if test "$result" -ne 0; then touch .clc/failed; fi
exit "$result"
