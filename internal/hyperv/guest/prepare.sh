#!/usr/bin/env bash
# Build-only harness. Installed packages/configuration live in provision.sh.
set +x
set -Eeuo pipefail
umask 077
test "$(id -u)" -eq 0
# cloud-init does not create a login environment. Package managers run as root;
# verification below runs as runner with its own passwd-derived home.
export HOME=/root DEBIAN_FRONTEND=noninteractive
exec 3>&1
exec > >(tee /var/log/clc-image-build.log) 2>&1
build_logger_pid=$!
build_failure() {
  local result=$1 command=$2 source=$3 line=$4
  trap - ERR EXIT
  set +ex
  printf 'CLC_PROVISIONING_FAILED exit=%s script=%s line=%s command=%s\n' "$result" "$source" "$line" "$command" | tee /var/log/clc-image-failure
  mkdir -p /home/runner/.clc
  touch /home/runner/.clc/failed
  exec 1>&3 2>&3
  wait "$build_logger_pid"
  exit "$result"
}
build_exit() {
  if test "$1" -ne 0; then build_failure "$@"; fi
}
trap 'build_failure "$?" "$BASH_COMMAND" "${BASH_SOURCE[0]}" "$LINENO"' ERR
# nounset errors do not invoke ERR. EXIT still records their nonzero status.
trap 'build_exit "$?" "$BASH_COMMAND" "${BASH_SOURCE[0]}" "$LINENO"' EXIT
# Trace only credential-free installation commands. Verification disables tracing
# and redacts its entire transcript, including ephemeral SQL credentials.
PS4='+[PrepareImage ${BASH_SOURCE}:${LINENO}] '
# Preserve the installation's normal file modes; only diagnostic logs are private.
umask 022
set -x
source /opt/clc/provision.sh
set +x
echo '[PrepareImage verification] Verifying installed tools and Docker as runner'
runuser -u runner -- /opt/clc/verify.sh smoke < /dev/null
exec 1>&3 2>&3
wait "$build_logger_pid"
touch /home/runner/.clc/build-ready
trap - ERR EXIT
