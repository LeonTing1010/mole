#!/usr/bin/env bash
# Shared first-boot path for cloud-init and the manual installer.
set -euo pipefail
umask 077
exec 9>/run/mole-install.lock
flock -w 300 9 || { echo "another mole installer is still running" >&2; exit 124; }
export DEBIAN_FRONTEND=noninteractive
timeout --kill-after=5 180 apt-get -qq update
timeout --kill-after=5 180 apt-get -qqy -o DPkg::Lock::Timeout=60 install curl ca-certificates openssl iproute2 python3 psmisc
source /usr/local/lib/mole-install-functions.sh
mole_install_singbox
mole_install_hy2
ip="${MOLE_SERVER_IP:-}"
if [[ -z "$ip" ]]; then
  ip=$(curl -fsSL --connect-timeout 5 --max-time 10 https://api.ipify.org)
fi
python3 /usr/local/bin/mole-configure --ip "$ip"
chmod 755 /etc/mole /etc/hysteria /etc/sing-box
chown root:sing-box /etc/sing-box/config.json
chown root:hysteria /etc/hysteria/config.yaml /etc/mole/server.key /etc/mole/server.crt
chmod 640 /etc/mole/server.key /etc/mole/server.crt
sing-box check -c /etc/sing-box/config.json
cat > /etc/systemd/system/mole-brutal-keeper.service <<'UNIT'
[Unit]
Description=mole TCP Brutal keeper
After=network-online.target
[Service]
ExecStart=/usr/bin/python3 /usr/local/bin/mole-brutal-keeper
Restart=on-failure
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
cat > /etc/systemd/system/mole-brutal-install.service <<'UNIT'
[Unit]
Description=mole optional TCP Brutal module
After=network-online.target
[Service]
Type=oneshot
ExecStart=/usr/local/bin/mole-brutal-install
TimeoutStartSec=310
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now mole-brutal-keeper sing-box hysteria-server
systemctl is-active --quiet mole-brutal-keeper sing-box hysteria-server
# Optional kernel support must never block either transport.
if ! systemctl enable mole-brutal-install || ! systemctl start --no-block mole-brutal-install; then
  echo "Optional Brutal installer could not start; transports remain available." >&2
fi
printf 'mole services active; client links: /etc/mole/config.json (root only)\n'
