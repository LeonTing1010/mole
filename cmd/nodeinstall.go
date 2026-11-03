package cmd

// Fixed release artifacts, verified against the publishers' release SHA-256
// digests on 2026-11-02. Updating versions requires updating these digests.
const nodeInstallFunctions = `
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
MOLE_INSTALL_DEADLINE=${MOLE_INSTALL_DEADLINE:-$(($(date +%s)+240))}
mole_run() {
  local remaining=$((MOLE_INSTALL_DEADLINE-$(date +%s)))
  test "$remaining" -gt 0 || { echo "required installation deadline exceeded" >&2; return 124; }
  timeout --kill-after=5 "${remaining}s" "$@"
}
mole_download() {
  local url=$1 digest=$2 target=$3 attempt
  for attempt in $(seq 1 "${MOLE_DOWNLOAD_ATTEMPTS:-3}"); do
    if mole_run curl -fSL --connect-timeout 10 --max-time 60 -o "$target" "$url"; then
      if printf '%s  %s\n' "$digest" "$target" | sha256sum -c -; then return 0; fi
    fi
    rm -f "$target"
  done
  echo "verified download failed" >&2
  return 1
}
mole_install_hy2() {
  local arch digest tmp
  case $(uname -m) in
    x86_64) arch=amd64; digest=907ba8c9693edb104b20582681fb7dc15639d5b64a9cbb616a7b539190a86691 ;;
    aarch64) arch=arm64; digest=a68a61a84452ca250ce0368202521965ca9cc9d801a404f1dc9008ac6cf677a7 ;;
    *) echo "unsupported server architecture" >&2; return 1 ;;
  esac
  tmp=$(mktemp)
  mole_download "https://github.com/HyNetworks/hysteria/releases/download/app/v2.13.0/hysteria-linux-$arch" "$digest" "$tmp"
  install -m 755 "$tmp" /usr/local/bin/hysteria
  rm -f "$tmp"
  # The independent installers may both create service accounts. Serialize only
  # account/package mutation, not their downloads; the shared deadline bounds
  # this flock wait as well as the command it protects.
  id hysteria >/dev/null 2>&1 || mole_run flock /run/mole-userdb.lock useradd --system --home-dir /var/lib/hysteria --create-home --shell /usr/sbin/nologin hysteria
  mkdir -p /etc/hysteria
  cat > /etc/systemd/system/hysteria-server.service << 'HY2_UNIT'
[Unit]
Description=Hysteria Server
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart=/usr/local/bin/hysteria server --config /etc/hysteria/config.yaml
WorkingDirectory=/var/lib/hysteria
User=hysteria
Group=hysteria
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
NoNewPrivileges=true
Restart=on-failure
RestartSec=2
[Install]
WantedBy=multi-user.target
HY2_UNIT
}
mole_wait_dpkg() {
  while fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock >/dev/null 2>&1; do
    mole_run sleep 2
  done
}
mole_install_singbox() {
  local arch digest tmp attempt
  case $(uname -m) in
    x86_64) arch=amd64; digest=4e9b8809aed0e3ac78ad14fa3a0af533402425d736f0cc9dc75b1c03d381b614 ;;
    aarch64) arch=arm64; digest=8f52525d84ff0f4fd2cec4bb12ed330590fc741ea5250fe7cfc94ae1f1069ac2 ;;
    *) echo "unsupported server architecture" >&2; return 1 ;;
  esac
  tmp=$(mktemp --suffix=.deb)
  mole_download "https://github.com/SagerNet/sing-box/releases/download/v1.14.4/sing-box_1.14.4_linux_$arch.deb" "$digest" "$tmp"
  for attempt in 1 2 3; do
    mole_wait_dpkg
    if mole_run flock /run/mole-userdb.lock dpkg --force-confold -i "$tmp"; then
      if test "$(dpkg-query -W -f='${Status}' sing-box)" = 'install ok installed' && id sing-box >/dev/null 2>&1 && test -s /usr/lib/systemd/system/sing-box.service; then
        rm -f "$tmp"
        return 0
      fi
    fi
    mole_run sleep 5
  done
  rm -f "$tmp"
  echo "sing-box package configuration incomplete" >&2
  return 1
}
`

const nodeBootPrelude = `
umask 077
mkdir -p /var/log/mole
exec > >(tee -a /var/log/mole/boot.log) 2>&1
export MOLE_INSTALL_DEADLINE=$(($(date +%s)+240))
echo "mole required installation started $(date -Is)"
`
