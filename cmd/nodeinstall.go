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

// Boot and repair retain fd 9 for the entire node mutation, including identity
// inspection. Downloads in the two boot legs can still proceed independently.
const nodeInstallLock = `
exec 9>/run/mole-install.lock
flock -w 300 9 || { echo "another mole installer is still running" >&2; exit 124; }
`

// Recovery access is installed before this bounded prerequisite stage. Its
// budget is separate from the protocol installers' shared absolute deadline.
var nodeBootPrereq = nodeInstallFunctions + `
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+${MOLE_PREREQ_BUDGET:-120}))
mole_wait_dpkg
mole_configure_pending
mole_run apt-get -qq update || exit $?
mole_run apt-get -qqy -o DPkg::Lock::Timeout=60 install psmisc curl ca-certificates python3 || exit $?
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+300))
`
