#!/usr/bin/env bash
set -euo pipefail
mkdir -p /home/runner/.clc /opt/clc /opt/hostedtoolcache
apt-get update
apt-get -y upgrade
apt-get install -y --no-install-recommends openssh-server sudo git curl wget jq unzip tar zip xz-utils \
  build-essential pkg-config python3 python3-pip python3-venv python3-dbus python3-gi bpftrace ca-certificates openssl gnupg \
  libicu74 libssl3t64 zlib1g libkrb5-3 liblttng-ust1t64 libunwind8 \
  linux-virtual linux-cloud-tools-virtual dotnet-sdk-10.0

# Signed official Docker apt feed, normal systemd Docker daemon inside Ubuntu.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL --retry 5 https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
fingerprint=$(gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc | awk -F: '$1=="fpr" {print $10; exit}')
test "$fingerprint" = 9DC858229FC7DD38854AE2D88D81803C0EBFCD88
chmod a+r /etc/apt/keyrings/docker.asc
echo 'deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable' > /etc/apt/sources.list.d/docker.list
curl -fsSL --retry 5 https://packages.microsoft.com/config/ubuntu/24.04/packages-microsoft-prod.deb -o /tmp/microsoft.deb
dpkg -i /tmp/microsoft.deb
rm /tmp/microsoft.deb
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin powershell
usermod -aG docker runner
systemctl enable --now docker ssh

# Official Node distribution. setup-node can subsequently select another version.
node_version=24.22.2
node_sha256=48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75
node_archive="node-v${node_version}-linux-x64.tar.xz"
curl -fsSL --retry 5 "https://nodejs.org/dist/v${node_version}/${node_archive}" -o "/tmp/${node_archive}"
echo "$node_sha256  /tmp/${node_archive}" | sha256sum --check --strict
tar -xJf "/tmp/${node_archive}" --strip-components=1 -C /usr/local
rm "/tmp/${node_archive}"

runner_version=2.338.2
runner_sha256=baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40
curl -fsSL --retry 5 "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-x64-${runner_version}.tar.gz" -o /tmp/runner.tar.gz
echo "$runner_sha256  /tmp/runner.tar.gz" | sha256sum --check --strict
tar -xzf /tmp/runner.tar.gz -C /home/runner
rm /tmp/runner.tar.gz
/home/runner/bin/installdependencies.sh
npx --yes playwright@1.62.1 install-deps chromium
mkdir -p /home/runner/.clc /opt/hostedtoolcache /usr/share/dotnet
chown -R runner:runner /home/runner /opt/hostedtoolcache /usr/share/dotnet
chmod 700 /home/runner/.clc
printf 'runner ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-clc-runner
chmod 440 /etc/sudoers.d/90-clc-runner
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/10-clc.conf
printf 'datasource_list: [ NoCloud ]\n' > /etc/cloud/cloud.cfg.d/90-clc-datasource.cfg
systemctl daemon-reload
systemctl disable clc-runner.service
# Independent observers start in future clones. They are optional Wants, never
# a hard prerequisite for executing a job and never alter update/restart policy.
systemctl enable clc-trace-bus.service clc-trace-kernel.service clc-trace-state.service
dpkg-query -W > /var/log/clc-image-packages.txt
