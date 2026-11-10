#!/usr/bin/env bash
# mole-server container entrypoint
set -euo pipefail

META_ISSUE="REALITY_PRIV REALITY_SHORT USER_UUID HY2_PASSWORD"
missing=""
for var in $META_ISSUE; do
  [ -n "${!var:-}" ] || missing="$missing $var"
done
if [ -n "$missing" ]; then
  echo "mole-server: missing required env vars:$missing" >&2
  exit 1
fi

MOLE_ETC=/etc/mole
mkdir -p "$MOLE_ETC" /var/log/mole

# Generate self-signed cert if not present
if [ ! -f "$MOLE_ETC/server.crt" ]; then
  apt-get -qqy install openssl >/dev/null 2>&1 || true
  openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
    -keyout "$MOLE_ETC/server.key" -out "$MOLE_ETC/server.crt" \
    -subj "/CN=www.microsoft.com" 2>/dev/null || true
fi

# Write sing-box config
cat > /etc/sing-box/config.json << SBEOF
{
  "log": {"level": "warn"},
  "dns": {
    "servers": [
      {"type": "udp", "server": "223.5.5.5", "tag": "dns-direct"},
      {"type": "udp", "server": "8.8.8.8", "tag": "dns-remote"}
    ],
    "final": "dns-remote",
    "strategy": "ipv4_only"
  },
  "inbounds": [
    {
      "type": "vless",
      "tag": "reality-in",
      "listen": "0.0.0.0", "listen_port": 443,
      "users": [{"uuid": "${USER_UUID}", "flow": "xtls-rprx-vision"}],
      "tls": {
        "enabled": true, "protocol": "reality",
        "certificate_path": "$MOLE_ETC/server.crt",
        "key_path": "$MOLE_ETC/server.key",
        "reality": {
          "handshake": {
            "server_name": "www.microsoft.com",
            "server": "www.microsoft.com",
            "server_port": 443, "transport": "tcp"
          },
          "private_key": "${REALITY_PRIV}",
          "short_ids": ["${REALITY_SHORT}"]
        }
      }
    }
  ],
  "outbounds": [
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"}
  ],
  "route": {
    "rules": [
      {"protocol": "quic", "action": "reject"},
      {"domain_suffix": ["cn", "com.cn", "net.cn", "edu.cn", "中国"], "outbound": "direct"},
      {"ip_cidr": ["1.1.1.1/32", "223.5.5.5/32", "192.168.0.0/16", "10.0.0.0/8", "127.0.0.0/8"], "outbound": "direct"},
      {"ip_cidr": ["0.0.0.0/0"], "outbound": "direct"}
    ],
    "final": "direct", "auto_detect_interface": true
  }
}
SBEOF

# Write hy2 config
cat > /etc/hysteria/config.yaml << HY2EOF
listen: :443
tls:
  cert: $MOLE_ETC/server.crt
  key: $MOLE_ETC/server.key
auth:
  type: password
  password: "${HY2_PASSWORD}"
masquerade:
  type: proxy
  proxy:
    url: https://www.microsoft.com
    rewriteHost: true
HY2EOF

# Start Brutal keeper first (loopback-only)
/usr/bin/python3 /usr/local/bin/mole-brutal-keeper &
PID_KEEPER=$!

# Start sing-box
sing-box run -c /etc/sing-box/config.json &
PID_SB=$!

# Start hysteria2
/usr/local/bin/hysteria2 server --config /etc/hysteria/config.yaml &
PID_HY2=$!

echo "mole-server: all services started (keeper=$PID_KEEPER, sing-box=$PID_SB, hy2=$PID_HY2)"

# Wait for any child to exit, then kill siblings and exit
wait -n
RC=$?
echo "mole-server: a child exited with $RC, shutting down"
kill $PID_KEEPER $PID_SB $PID_HY2 2>/dev/null || true
wait
exit $RC
