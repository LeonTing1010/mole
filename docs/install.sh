#!/usr/bin/env bash
# Redirect — actual script lives in scripts/mole-server.sh
set -euo pipefail
exec bash <(curl -fsSL https://raw.githubusercontent.com/LeonTing1010/mole/master/scripts/mole-server.sh) "$@"
