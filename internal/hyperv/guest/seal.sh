#!/usr/bin/env bash
set -euo pipefail
python3 /opt/clc/service-control.py stop image-seal || true
systemctl disable clc-runner.service
systemctl stop docker.service docker.socket containerd.service
systemctl stop clc-trace-bus.service clc-trace-kernel.service clc-trace-state.service || true
# Do not clone builder process identities or trace history into future runners.
rm -f /var/log/clc-diagnostics/{bus,kernel,state}.log /var/log/clc-diagnostics/{bus,kernel,state}.log.1
# Only image-build smoke workloads have run here. Fresh daemon state prevents
# cloned Docker engine/network identities; normal systemd startup recreates it.
rm -rf /var/lib/docker /var/lib/containerd
rm -f /etc/docker/key.json
# Refuse to freeze registration or GitHub credential material.
for file in .runner .credentials .credentials_rsaparams .clc/jit .clc/jit.pending; do
  if test -e "/home/runner/$file"; then echo 'Refusing to seal registered runner image' >&2; exit 1; fi
done
rm -rf /home/runner/.ssh /home/runner/.clc/* /home/runner/_diag/* /home/runner/_work /root/.ssh /root/.npm
rm -f /etc/ssh/ssh_host_* /etc/netplan/50-cloud-init.yaml
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
cloud-init clean --logs --machine-id --seed
rm -f /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
rm -f /var/lib/systemd/random-seed
sync
echo CLC_IMAGE_SEALED
systemctl poweroff --no-block
