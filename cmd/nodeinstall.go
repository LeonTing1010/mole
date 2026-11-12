package cmd

import nodeassets "github.com/LeonTing1010/mole/deploy"

// Fixed release artifacts, verified against the publishers' release SHA-256
// digests on 2026-11-02. Updating versions requires updating these digests.
var nodeInstallFunctions = nodeassets.InstallFunctions

const nodeBootPrelude = `
umask 077
mkdir -p /var/log/mole
exec > >(tee -a /var/log/mole/boot.log) 2>&1
echo "mole required installation started $(date -Is)"
`

// nodeBootPrereq runs before either leg forks. On a fresh Ubuntu image the
// first minutes belong to apt-daily/unattended-upgrades, which hold the dpkg
// frontend lock — and the cloud image may not ship psmisc (fuser) or even
// curl, which mole_wait_dpkg and mole_download depend on. Without this stage
// the legs race the lock (or fail to probe it) and ship half-installed; the
// dpkg --configure -a also recovers any package a killed attempt left
// half-configured. The install deadline is set HERE, after the prerequisites,
// so apt-daily's lock-hold does not eat into the legs' own budget.
const nodeBootPrereq = `
mole_boot_prereq() {
  local lockfiles="/var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock"
  while :; do
    if command -v fuser >/dev/null 2>&1; then
      fuser $lockfiles >/dev/null 2>&1 || break
    else
      dpkg --audit >/dev/null 2>&1 && break
    fi
    sleep 3
  done
  export DEBIAN_FRONTEND=noninteractive
  dpkg --configure -a >/dev/null 2>&1 || true
  apt-get -qq update >/dev/null 2>&1 || true
  apt-get -qqy -o DPkg::Lock::Timeout=120 install psmisc curl ca-certificates >/dev/null 2>&1 || true
}
mole_boot_prereq
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+300))
`
