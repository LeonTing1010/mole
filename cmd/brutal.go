package cmd

import (
	"fmt"

	nodeassets "github.com/LeonTing1010/mole/deploy"
)

// TCP Brutal on the REALITY leg.
//
// Measured 2026-09-06 on a CN→KR path dropping 3–6% of packets, paired
// 8 MB samples in the same minutes: REALITY under BBR carried a median 19 Mbps
// with a floor of 3.5; the same leg under Brutal carried 24 with a floor of
// 13; hy2's Brutal carried 31 but failed 27 of 30 new connections. Brutal is
// what makes hy2 fast on a lossy evening, and it is a congestion control, not
// a transport — apernet ships it as a kernel module (tcp-brutal v2) that
// applies per destination to any TCP the host sends. So the fast property
// moves onto the leg that has the stable property, and switching legs turns
// from the normal case into the safety net.
//
// v2 needs nothing from the application: `brutalctl add <client>/32 <mbps>`
// once, and every TCP connection the node opens or accepts toward that
// address is paced at that rate. What it does need is the client's address
// and the rate, both of which change — the address with every Wi-Fi
// re-association, the rate with every ceiling rung — and neither end can
// reach the other out of band (the management API was deleted for good
// reasons; the cloud firewall admits UDP/443 and TCP/443 only). Hence the
// keeper below: a loopback-only service on the node that
//
//   - adds the rule for every peer currently established on TCP/443 — the
//     node's own socket table is the one place the client's address is
//     already known — at the current rate, every few seconds;
//   - accepts the rate IN-BAND: the client fetches
//     http://127.0.0.1:8098/rate?mbps=N through the REALITY leg, the node's
//     sing-box dials its own loopback, and the keeper applies N. Reachable
//     only through the authenticated tunnel, so no new surface;
//   - serves /bulk?bytes=N, a throughput sample that measures exactly the
//     VPS→client segment with no origin in the way and no rate limit
//     (speed.cloudflare.com answers a rate-limited request with one byte and
//     200 OK). For hand-run A/Bs through the tunnel.

// brutalKeeperSnippet installs the module and the keeper, with the given
// default rate. No shebang, no `set -e` of its own: the module install can
// fail on a kernel without headers, and that must not take the REALITY
// server down with it — the leg then simply runs BBR, as it did before.
func brutalKeeperSnippet(defaultMbps int) string {
	return fmt.Sprintf(`
# ---- TCP Brutal for the REALITY leg (see cmd/brutal.go) ----
export DEBIAN_FRONTEND=noninteractive
echo %d > /etc/mole-brutal-rate
cat > /usr/local/bin/mole-brutal-keeper << 'PYEOF'
%s
PYEOF
chmod 755 /usr/local/bin/mole-brutal-keeper
cat > /etc/systemd/system/mole-brutal-keeper.service << 'EOF'
[Unit]
Description=mole: TCP Brutal rule keeper and in-band rate endpoint for the REALITY leg
After=network-online.target

[Service]
ExecStart=/usr/bin/python3 /usr/local/bin/mole-brutal-keeper
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now mole-brutal-keeper >/dev/null 2>&1 || true
systemctl restart mole-brutal-keeper || true
cat > /usr/local/bin/mole-brutal-install << 'BRUTAL_INSTALL'
#!/usr/bin/env bash
export MOLE_INSTALL_DEADLINE=$(($(date +%%s)+300))
%s
mkdir -p /var/log/mole
trap 'rc=$?; if [ "$rc" = 0 ]; then echo ready; else echo failed; fi > /var/log/mole/brutal.status' EXIT
echo installing > /var/log/mole/brutal.status
if command -v brutalctl >/dev/null 2>&1 && lsmod | grep -q '^brutal'; then exit 0; fi
mole_run apt-get -qq update
mole_run apt-get -qq -o DPkg::Lock::Timeout=30 install -y dkms "linux-headers-$(uname -r)" curl
installer=$(mktemp)
archive=$(mktemp --suffix=.tar.gz)
# Pinned source behind tcp.hy2.sh; both script and module archive are verified.
mole_download https://raw.githubusercontent.com/HyNetworks/tcp-brutal/d2397ff8bca04a29fd2de01cf7d2d4b825224de8/scripts/install_dkms.sh 399eebb8df5e14f4ca1e11468b292f27585f633aa56f08d0c20e9eb1d5f35880 "$installer"
mole_download https://github.com/HyNetworks/tcp-brutal/releases/download/v2.0.1/tcp-brutal.dkms.tar.gz 64a2778f308f7684cb5a137d578bc071e373960a3b9b79f9de015b84b29a7299 "$archive"
mole_run bash "$installer" --local "$archive"
rm -f "$installer" "$archive"
modprobe brutal
command -v brutalctl >/dev/null
lsmod | grep -q '^brutal'
BRUTAL_INSTALL
chmod 755 /usr/local/bin/mole-brutal-install
mkdir -p /var/log/mole
cat > /etc/systemd/system/mole-brutal-install.service << 'BRUTAL_UNIT'
[Unit]
Description=mole optional TCP Brutal installation
After=network-online.target sing-box.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/mole-brutal-install
TimeoutStartSec=305
StandardOutput=append:/var/log/mole/brutal-install.log
StandardError=append:/var/log/mole/brutal-install.log
ExecStopPost=/bin/sh -c 'test "$SERVICE_RESULT" = success || echo failed > /var/log/mole/brutal.status'
BRUTAL_UNIT
systemctl daemon-reload
systemctl start --no-block mole-brutal-install
`, defaultMbps, brutalKeeperPy, nodeInstallFunctions)
}

// brutalKeeperPy is the keeper. Standard library only: the node is a fresh
// Ubuntu with python3 and nothing else.
//
// 2026-11-10: switched from brutcalctl CLI to direct /proc/net/tcp_brutal/rules
// writes so we can set cwnd_gain alongside rate. Brutal's INIT_CWND_GAIN=20
// (2.0x window, most aggressive) caused lossy paths to buffer too many
// in-flight packets and collapse throughput — rate=400 actually beat rate=600
// by 45% in A/B. Default gain=15 (1.5x, documented sweet spot) gives the
// congestion window more headroom without overcommitting the path.
var brutalKeeperPy = nodeassets.BrutalKeeper
