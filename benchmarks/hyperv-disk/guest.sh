#!/usr/bin/env bash
set -euo pipefail
umask 077
cd /home/runner/clc-disk-benchmark
action=${1:?prepare, validate or run}
case "$action" in prepare|validate|run) ;; *) exit 2 ;; esac
mountpoint=/mnt/clc-disk-benchmark

# This VM has precisely one whole 40-GiB disk, separate from the 100-GiB OS.
mapfile -t candidates < <(lsblk -bdnpo NAME,SIZE,TYPE | awk '$2==42949672960 && $3=="disk" {print $1}')
test "${#candidates[@]}" -eq 1
device=${candidates[0]}
test "$(lsblk -nrpo MOUNTPOINTS "$device" | tr -d '[:space:]')" = ''
sudo mkdir -p "$mountpoint"
if test "$action" = prepare || test "$action" = validate; then
  test -z "$(sudo wipefs --no-act --noheadings "$device")"
  sudo mkfs.ext4 -q -L CLC_SQL_BENCH -m 0 -E nodiscard,lazy_itable_init=0,lazy_journal_init=0 "$device"
else
  test "$(sudo blkid -s LABEL -o value "$device")" = CLC_SQL_BENCH
fi
sudo mount -o noatime,nodiscard "$device" "$mountpoint"
test "$(findmnt -n -o SOURCE --target "$mountpoint")" = "$device"
test "$(findmnt -n -o MAJ:MIN --target "$mountpoint")" != "$(findmnt -n -o MAJ:MIN --target /)"
test "$(sudo blockdev --getsize64 "$device")" -eq 42949672960
sudo mkdir -p "$mountpoint/mssql"
sudo chown 10001:0 "$mountpoint/mssql"

if test "$action" = prepare || test "$action" = validate; then
  sudo systemctl mask --now clc-runner.service
  sudo systemctl disable --now fstrim.timer
  test ! -f /home/runner/.runner
  test ! -f /home/runner/.credentials
  test "$(. /etc/os-release; echo "$VERSION_ID")" = 24.04
  test "$(nproc)" -eq 4
  if test "$action" = prepare; then
    mkdir source
    tar -xf source.tar -C source
    # Use the SDK requested by the immutable source, installing only if absent.
    sdk=$(python3 -c 'import json; print(json.load(open("source/global.json"))["sdk"]["version"])')
    if ! dotnet --list-sdks | grep -q "^$sdk "; then
      curl -fsSL --retry 3 https://dot.net/v1/dotnet-install.sh -o dotnet-install.sh
      sha256sum dotnet-install.sh > dotnet-install.sha256
      bash dotnet-install.sh --version "$sdk" --install-dir /home/runner/.dotnet
    fi
    export PATH="/home/runner/.dotnet:$PATH"
    (cd source; dotnet build tests/Asap.Tests/Asap.Tests.csproj --configuration Release) > build.log 2>&1
  fi
  # Resolve a mutable tag once. Every subsequent run uses this immutable digest.
  docker pull mcr.microsoft.com/mssql/server:2022-latest > docker-pull.log 2>&1
  docker image inspect mcr.microsoft.com/mssql/server:2022-latest --format '{{index .RepoDigests 0}}' > image-digest
  printf 'Bench%sAa1!\n' "$(openssl rand -hex 24)" > password
fi

export PATH="/home/runner/.dotnet:$PATH"
python3 measure.py "$action" "${2:-preparation}" "$device"
sync
sudo umount "$mountpoint"
