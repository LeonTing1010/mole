#!/usr/bin/env bash
set -euo pipefail
umask 077
ip="${MOLE_SERVER_IP:-}"
if [[ -z "$ip" ]]; then
  ip=$(curl -fsSL --connect-timeout 5 --max-time 10 https://api.ipify.org)
fi
python3 /usr/local/bin/mole-configure --ip "$ip"
sing-box check -c /etc/sing-box/config.json
pids=()
shutdown() {
  trap - TERM INT
  if ((${#pids[@]})); then
    kill "${pids[@]}" 2>/dev/null || true
    # Bound shutdown even when a child ignores TERM.
    (sleep 5; kill -KILL "${pids[@]}" 2>/dev/null || true) >/dev/null 2>&1 &
    local watchdog=$!
    wait "${pids[@]}" 2>/dev/null || true
    kill "$watchdog" 2>/dev/null || true
    wait "$watchdog" 2>/dev/null || true
  fi
}
trap 'exit 143' TERM
trap 'exit 130' INT
trap shutdown EXIT
python3 /usr/local/bin/mole-brutal-keeper &
pids+=("$!")
sing-box run -c /etc/sing-box/config.json &
pids+=("$!")
/usr/local/bin/hysteria server --config /etc/hysteria/config.yaml &
pids+=("$!")
rc=0
wait -n "${pids[@]}" || rc=$?
exit "$rc"
