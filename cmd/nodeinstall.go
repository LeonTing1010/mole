package cmd

import nodeassets "github.com/LeonTing1010/mole/deploy"

// Fixed release artifacts, verified against the publishers' release SHA-256
// digests on 2026-11-02. Updating versions requires updating these digests.
var nodeInstallFunctions = nodeassets.InstallFunctions

const nodeBootPrelude = `
umask 077
mkdir -p /var/log/mole
exec > >(tee -a /var/log/mole/boot.log) 2>&1
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+240))
echo "mole required installation started $(date -Is)"
`
