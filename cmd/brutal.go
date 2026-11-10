package cmd

import (
	"fmt"

	"github.com/LeonTing1010/mole/core"
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
var brutalKeeperPy = `#!/usr/bin/env python3
# mole-brutal-keeper: see cmd/brutal.go in mole. Loopback only.
import http.server, ipaddress, os, socketserver, subprocess, threading, time, urllib.parse

# The two env overrides exist so the keeper can be run and tested off the node.
# Unset — which is how systemd starts it — they are exactly the old constants.
RATE_FILE = os.environ.get("MOLE_BRUTAL_RATE_FILE", "/etc/mole-brutal-rate")
GAIN_FILE = os.environ.get("MOLE_BRUTAL_GAIN_FILE", "/etc/mole-brutal-gain")
PROC_RULES = "/proc/net/tcp_brutal/rules"
PORT = int(os.environ.get("MOLE_BRUTAL_PORT", ` + fmt.Sprint(core.BrutalKeeperPort) + `))
LISTEN = "127.0.0.1"
LEG_PORT = 443
BLOCK = os.urandom(1 << 20)  # 1 MiB of incompressible bytes, repeated for /bulk
DEFAULT_GAIN = 15   # 1.5x window — Brutal README sweet spot, vs kernel default 20 (2.0x)
PROC_HANDLE = open(PROC_RULES, "w") if os.path.exists(PROC_RULES) else None

def read_rate():
    try:
        return max(1, int(open(RATE_FILE).read().strip()))
    except Exception:
        return 100

def write_rate(n):
    tmp = RATE_FILE + ".tmp"
    with open(tmp, "w") as f:
        f.write("%d\n" % n)
    os.replace(tmp, RATE_FILE)

def read_gain():
    try:
        g = int(open(GAIN_FILE).read().strip())
        return max(5, min(80, g))  # Brutal kernel bounds
    except Exception:
        return DEFAULT_GAIN

def write_gain(n):
    g = max(5, min(80, int(n)))
    tmp = GAIN_FILE + ".tmp"
    with open(tmp, "w") as f:
        f.write("%d\n" % g)
    os.replace(tmp, GAIN_FILE)
    return g

def peers():
    """Addresses currently established on the leg port, as the node sees them.

    Never raises: this is called from a request handler, and an exception here
    would close the connection on the client, which reads as "the keeper did
    not answer" — indistinguishable from a dead tunnel, and one of the few
    failures that would leave the client with no reason to report.
    """
    try:
        out = subprocess.run(["ss", "-Htn", "state", "established", "( sport = :%d )" % LEG_PORT],
                             capture_output=True, text=True).stdout
    except Exception:
        return set()
    ips = set()
    for line in out.splitlines():
        cols = line.split()
        if len(cols) < 4:
            continue
        host, sep, port = cols[3].rpartition(":")
        if not sep or not port.isdecimal() or not 0 < int(port) <= 65535:
            continue
        try:
            address = ipaddress.ip_address(host.removeprefix("[").removesuffix("]"))
            address = address.ipv4_mapped if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped else address
            if address.is_unspecified or address.is_multicast or "%" in str(address):
                continue
            ips.add(str(address))
        except ValueError:
            continue
    return ips

applied = {}  # ip -> rate last applied
lock = threading.Lock()

def module_ok():
    try:
        with open("/proc/modules") as f:
            if any(line.startswith("brutal ") for line in f):
                return True, ""
    except Exception as e:
        return False, "cannot read /proc/modules: %s" % e
    return False, "kernel module 'brutal' is not loaded"

def apply(force=False):
    """Install the pacing rule for every current peer via /proc/net/tcp_brutal/rules.

    Writes directly instead of shelling out to brutcalctl so we can set
    cwnd_gain alongside rate — brutcalctl's 'add' syntax takes only Mbps
    while the /proc writer accepts 'rate=<bytes/s> gain=<tenths>'. This also
    eliminates a subprocess fork per peer per sweep.
    """
    rate = read_rate()
    gain = read_gain()
    ok, detail = module_ok()
    ips = peers()
    if not ok:
        return 0, len(ips), detail
    rate_bytes = int(rate) * 125000  # 1 Mbps = 125,000 bytes/s
    n = 0
    with lock:
        for ip in ips:
            # Rate equality is not kernel-state evidence: a route/module
            # reset can remove pacing for new sessions while old sockets
            # remain brutal. Reconcile the idempotent rule every sweep.
            try:
                prefix = str(ipaddress.ip_network(ip))
                rule = "add %s rate=%d gain=%d\n" % (prefix, rate_bytes, gain)
                with open(PROC_RULES, "w") as f:
                    f.write(rule)
                applied[ip] = rate
                n += 1
            except OSError as e:
                applied.pop(ip, None)
                detail = "/proc write failed: %s" % str(e)[:200]
    if n == 0 and not detail:
        detail = "no peer established on TCP/%d" % LEG_PORT
    return n, len(ips), detail

def loop():
    while True:
        try:
            apply()
        except Exception:
            pass
        time.sleep(3)



class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/rate":
            try:
                n = max(1, min(10000, int(q.get("mbps", ["0"])[0])))
            except ValueError:
                n = 0
            if n:
                write_rate(n)
            # Allow callers to set gain together with rate so a single request
            # configures both knobs — saves one round-trip and one apply().
            try:
                g = max(5, min(80, int(q.get("gain", ["0"])[0])))
                if g:
                    write_gain(g)
            except ValueError:
                pass
            # Applied synchronously, not in a thread: the caller is asking
            # whether the rate is in effect, and an answer sent before the
            # work is done cannot say.
            got, npeers, detail = apply(True)
            body = "rate=%d\ngain=%d\napplied=%d\npeers=%d\n" % (read_rate(), read_gain(), got, npeers)
            if detail:
                body += "detail=%s\n" % detail
            self.reply(body, 200 if got else 503)
        elif u.path == "/gain":
            # Dynamic gain adjustment — cwnd_gain in tenths (5=0.5x…80=8.0x).
            # Default 15 (1.5x) is Brutal README sweet spot vs kernel default 20.
            try:
                g = max(5, min(80, int(q.get("g", [str(read_gain())])[0])))
            except ValueError:
                g = read_gain()
            write_gain(g)
            got, npeers, detail = apply(True)
            body = "gain=%d\napplied=%d\npeers=%d\n" % (read_gain(), got, npeers)
            if detail:
                body += "detail=%s\n" % detail
            self.reply(body, 200)
        elif u.path == "/bulk":
            try:
                n = max(1, min(64 << 20, int(q.get("bytes", ["2000000"])[0])))
            except ValueError:
                n = 2000000
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(n))
            self.end_headers()
            left = n
            while left > 0:
                chunk = BLOCK if left >= len(BLOCK) else BLOCK[:left]
                self.wfile.write(chunk)
                left -= len(chunk)
        elif u.path == "/reachable":
            # This request arrived through an authenticated tunnel. It proves
            # transport delivery, independently of the optional kernel module.
            self.reply("rate=%d\napplied=0\npeers=0\nreachable=1\n" % read_rate())
        elif u.path == "/tuning":
            try:
                result = subprocess.run(["python3", "/usr/local/lib/mole-network-state.py"], capture_output=True, text=True, timeout=12)
                if result.returncode == 0:
                    self.reply(result.stdout)
                else:
                    self.reply('{"status":"unobserved"}', 503)
            except (OSError, subprocess.TimeoutExpired):
                self.reply('{"status":"unobserved"}', 503)
        elif u.path == "/health":
            ok, detail = module_ok()
            ips = peers()
            rules = ""
            try:
                rules = subprocess.run(["brutalctl", "list"], capture_output=True, text=True).stdout
            except FileNotFoundError:
                detail = detail or "brutalctl is not installed"
            body = "rate=%d\napplied=%d\npeers=%d\n" % (read_rate(), len(applied), len(ips))
            if detail:
                body += "detail=%s\n" % detail
            self.reply(body + rules, 200 if ok else 503)
        else:
            self.send_error(404)
    def reply(self, body, status=200):
        b = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

if __name__ == "__main__":
    threading.Thread(target=loop, daemon=True).start()
    Server((LISTEN, PORT), H).serve_forever()
`
