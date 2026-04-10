#!/usr/bin/env bash
#
# mole-server.sh — One-line proxy server setup
# Usage: curl -fsSL https://raw.githubusercontent.com/LeonTing1010/mole/master/scripts/mole-server.sh | bash
#
# Deploys sing-box with VLESS + Reality on any Linux VPS.
# Outputs a URI you can paste into: mole add <uri>
#
set -euo pipefail

# ── Config ──────────────────────────────────────────────────────────
SINGBOX_VERSION="1.13.4"
PORT="${MOLE_PORT:-443}"
SNI="${MOLE_SNI:-www.microsoft.com}"
INSTALL_DIR="/opt/mole-server"
CONFIG_FILE="$INSTALL_DIR/config.json"

# ── Colors ──────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${GREEN}[✓]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
err()   { echo -e "${RED}[✗]${NC} $*" >&2; }
step()  { echo -e "${CYAN}[→]${NC} $*"; }

# ── Preflight ───────────────────────────────────────────────────────
check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        err "Please run as root: sudo bash <(curl -fsSL ...)"
        exit 1
    fi
}

detect_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        armv7l)        echo "armv7" ;;
        *)             err "Unsupported architecture: $arch"; exit 1 ;;
    esac
}

detect_os() {
    local os
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$os" in
        linux)  echo "linux" ;;
        *)      err "This script is for Linux VPS only (got: $os)"; exit 1 ;;
    esac
}

get_public_ip() {
    # Allow manual override
    if [ -n "${MOLE_IP:-}" ]; then
        echo "$MOLE_IP"
        return
    fi
    # Try online services first
    local ip
    ip=$(curl -4 -s --connect-timeout 5 https://api.ipify.org 2>/dev/null) \
    || ip=$(curl -4 -s --connect-timeout 5 https://ifconfig.me 2>/dev/null) \
    || ip=$(curl -4 -s --connect-timeout 5 https://icanhazip.com 2>/dev/null) \
    || ip=""
    # Fallback: first non-loopback, non-docker IPv4 address
    if [ -z "$ip" ]; then
        ip=$(ip -4 addr show scope global | grep -v docker | grep -oP 'inet \K[0-9.]+' | head -1 2>/dev/null) || ip=""
    fi
    if [ -z "$ip" ]; then
        err "Cannot detect public IP. Set it manually: MOLE_IP=x.x.x.x"
        exit 1
    fi
    echo "$ip"
}

# ── Install sing-box ────────────────────────────────────────────────
install_singbox() {
    local arch os tarball url extract_dir
    arch=$(detect_arch)
    os=$(detect_os)
    tarball="sing-box-${SINGBOX_VERSION}-${os}-${arch}.tar.gz"
    url="https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/${tarball}"
    extract_dir="sing-box-${SINGBOX_VERSION}-${os}-${arch}"

    mkdir -p "$INSTALL_DIR"

    if [ -f "$INSTALL_DIR/sing-box" ]; then
        local current_ver
        current_ver=$("$INSTALL_DIR/sing-box" version 2>/dev/null | head -1 | awk '{print $3}' || echo "unknown")
        if [ "$current_ver" = "$SINGBOX_VERSION" ]; then
            info "sing-box $SINGBOX_VERSION already installed"
            return 0
        fi
        warn "Upgrading sing-box from $current_ver to $SINGBOX_VERSION"
    fi

    step "Downloading sing-box $SINGBOX_VERSION ($arch)..."
    curl -fsSL -o "/tmp/$tarball" "$url"

    step "Extracting..."
    tar xzf "/tmp/$tarball" -C /tmp
    mv "/tmp/$extract_dir/sing-box" "$INSTALL_DIR/sing-box"
    chmod +x "$INSTALL_DIR/sing-box"

    # Cleanup
    rm -rf "/tmp/$tarball" "/tmp/$extract_dir"

    info "sing-box installed to $INSTALL_DIR/sing-box"
}

# ── Generate keys ───────────────────────────────────────────────────
generate_reality_keypair() {
    local output
    output=$("$INSTALL_DIR/sing-box" generate reality-keypair)
    PRIVATE_KEY=$(echo "$output" | grep "PrivateKey" | awk '{print $2}')
    PUBLIC_KEY=$(echo "$output" | grep "PublicKey" | awk '{print $2}')
}

generate_uuid() {
    UUID=$("$INSTALL_DIR/sing-box" generate uuid)
}

generate_short_id() {
    SHORT_ID=$(openssl rand -hex 4)
}

# ── Generate config ─────────────────────────────────────────────────
generate_config() {
    cat > "$CONFIG_FILE" <<CONF
{
  "log": { "level": "warn", "timestamp": true },
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-in",
      "listen": "::",
      "listen_port": $PORT,
      "users": [
        {
          "uuid": "$UUID",
          "flow": "xtls-rprx-vision"
        }
      ],
      "tls": {
        "enabled": true,
        "server_name": "$SNI",
        "reality": {
          "enabled": true,
          "handshake": {
            "server": "$SNI",
            "server_port": 443
          },
          "private_key": "$PRIVATE_KEY",
          "short_id": ["$SHORT_ID"]
        }
      }
    }
  ],
  "outbounds": [
    { "type": "direct", "tag": "direct" }
  ]
}
CONF
    info "Config written to $CONFIG_FILE"
}

# ── Systemd service ─────────────────────────────────────────────────
install_service() {
    cat > /etc/systemd/system/mole-server.service <<EOF
[Unit]
Description=Mole Server (sing-box)
After=network.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/sing-box run -c $CONFIG_FILE
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable mole-server >/dev/null 2>&1
    systemctl restart mole-server

    # Wait and check
    sleep 2
    if systemctl is-active --quiet mole-server; then
        info "mole-server service started"
    else
        err "Service failed to start. Check: journalctl -u mole-server"
        exit 1
    fi
}

# ── Firewall ────────────────────────────────────────────────────────
open_firewall() {
    if command -v ufw >/dev/null 2>&1; then
        ufw allow "$PORT/tcp" >/dev/null 2>&1 && info "UFW: port $PORT opened"
    elif command -v firewall-cmd >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null 2>&1
        firewall-cmd --reload >/dev/null 2>&1
        info "firewalld: port $PORT opened"
    fi
    # If no firewall tool found, skip silently (most VPS have no firewall by default)
}

# ── Output URI ──────────────────────────────────────────────────────
print_uri() {
    local ip="$1"
    local uri="vless://${UUID}@${ip}:${PORT}?encryption=none&security=reality&sni=${SNI}&fp=chrome&pbk=${PUBLIC_KEY}&sid=${SHORT_ID}&type=tcp&flow=xtls-rprx-vision#mole-${ip}"

    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo ""
    info "Your proxy server is ready!"
    echo ""
    echo -e "  ${CYAN}URI (copy this):${NC}"
    echo ""
    echo "  $uri"
    echo ""
    echo -e "  ${CYAN}Connect with Mole:${NC}"
    echo ""
    echo "  mole add '$uri'"
    echo "  mole up"
    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo ""
    echo -e "  ${YELLOW}Server info:${NC}"
    echo "    IP:          $ip"
    echo "    Port:        $PORT"
    echo "    Protocol:    VLESS + Reality"
    echo "    SNI:         $SNI"
    echo "    UUID:        $UUID"
    echo "    Public Key:  $PUBLIC_KEY"
    echo "    Short ID:    $SHORT_ID"
    echo ""
    echo "  Manage:  systemctl {start|stop|restart|status} mole-server"
    echo "  Logs:    journalctl -u mole-server -f"
    echo "  Config:  $CONFIG_FILE"
    echo ""

    # Save URI to file for easy retrieval
    echo "$uri" > "$INSTALL_DIR/client.uri"
    info "URI also saved to $INSTALL_DIR/client.uri"
}

# ── Main ────────────────────────────────────────────────────────────
main() {
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║   Mole Server — One-line Setup       ║"
    echo "  ║   VLESS + Reality + sing-box          ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""

    check_root

    step "Detecting environment..."
    local ip
    ip=$(get_public_ip)
    info "Public IP: $ip"
    info "Architecture: $(detect_arch)"

    step "Installing sing-box..."
    install_singbox

    step "Generating credentials..."
    generate_uuid
    generate_reality_keypair
    generate_short_id
    info "UUID: $UUID"

    step "Writing config..."
    generate_config

    step "Opening firewall port $PORT..."
    open_firewall

    step "Starting service..."
    install_service

    print_uri "$ip"
}

main "$@"
