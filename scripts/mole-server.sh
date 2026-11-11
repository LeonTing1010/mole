#!/usr/bin/env bash
# Manual root installation on Debian/Ubuntu. All assets come from one revision.
set -euo pipefail
[[ $(id -u) == 0 ]] || { echo 'Run this installer as root.' >&2; exit 1; }
revision="${MOLE_REVISION:-master}"
[[ "$revision" =~ ^[A-Za-z0-9._/-]+$ ]] || { echo 'Invalid revision' >&2; exit 1; }
base="https://raw.githubusercontent.com/LeonTing1010/mole/$revision/deploy"
task_dir=$(mktemp -d)
trap 'rm -rf "$task_dir"' EXIT
for asset in configure.py brutal-keeper.py install-functions.sh install-brutal.sh bootstrap.sh; do
  curl -fsSL --connect-timeout 10 --max-time 60 "$base/$asset" -o "$task_dir/$asset"
done
install -d /usr/local/bin /usr/local/lib
install -m 755 "$task_dir/configure.py" /usr/local/bin/mole-configure
install -m 755 "$task_dir/brutal-keeper.py" /usr/local/bin/mole-brutal-keeper
install -m 644 "$task_dir/install-functions.sh" /usr/local/lib/mole-install-functions.sh
install -m 755 "$task_dir/install-brutal.sh" /usr/local/bin/mole-brutal-install
install -m 755 "$task_dir/bootstrap.sh" /usr/local/bin/mole-bootstrap
/usr/local/bin/mole-bootstrap "$@"
