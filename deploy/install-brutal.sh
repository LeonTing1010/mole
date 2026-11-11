#!/usr/bin/env bash
set -euo pipefail
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+300))
source /usr/local/lib/mole-install-functions.sh
mkdir -p /var/log/mole
installer= archive=
trap 'rc=$?; rm -f "$installer" "$archive"; if [ "$rc" = 0 ]; then echo ready; else echo failed; fi > /var/log/mole/brutal.status' EXIT
if command -v brutalctl >/dev/null 2>&1 && lsmod | grep -q '^brutal'; then exit 0; fi
mole_run apt-get -qq update
mole_run apt-get -qq -o DPkg::Lock::Timeout=30 install -y dkms "linux-headers-$(uname -r)" curl
installer=$(mktemp)
archive=$(mktemp --suffix=.tar.gz)
mole_download https://raw.githubusercontent.com/HyNetworks/tcp-brutal/d2397ff8bca04a29fd2de01cf7d2d4b825224de8/scripts/install_dkms.sh 399eebb8df5e14f4ca1e11468b292f27585f633aa56f08d0c20e9eb1d5f35880 "$installer"
mole_download https://github.com/HyNetworks/tcp-brutal/releases/download/v2.0.1/tcp-brutal.dkms.tar.gz 64a2778f308f7684cb5a137d578bc071e373960a3b9b79f9de015b84b29a7299 "$archive"
mole_run bash "$installer" --local "$archive"
rm -f "$installer" "$archive"
modprobe brutal
command -v brutalctl >/dev/null
lsmod | grep -q '^brutal'
