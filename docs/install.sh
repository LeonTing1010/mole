#!/usr/bin/env bash
set -euo pipefail
task_script=$(mktemp)
trap 'rm -f "$task_script"' EXIT
curl -fsSL --connect-timeout 10 --max-time 60 "https://raw.githubusercontent.com/LeonTing1010/mole/${MOLE_REVISION:-master}/scripts/mole-server.sh" -o "$task_script"
bash "$task_script" "$@"
