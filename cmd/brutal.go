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
if ! command -v brutalctl >/dev/null 2>&1 || ! lsmod | grep -q '^brutal'; then
  for i in $(seq 1 90); do
    fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock >/dev/null 2>&1 || break
    sleep 2
  done
  (apt-get -qq update && apt-get -qq install -y dkms "linux-headers-$(uname -r)" curl) >/tmp/brutal-apt.log 2>&1 || echo "brutal: apt failed (see /tmp/brutal-apt.log)" >&2
  (bash <(curl -fsSL https://tcp.hy2.sh/)) >/tmp/brutal-install.log 2>&1 || echo "brutal: module install failed (see /tmp/brutal-install.log); REALITY runs BBR" >&2
  modprobe brutal 2>/dev/null || true
fi
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
`, defaultMbps, brutalKeeperPy)
}

// brutalKeeperPy is the keeper. Standard library only: the node is a fresh
// Ubuntu with python3 and nothing else.
var brutalKeeperPy = `#!/usr/bin/env python3
# mole-brutal-keeper: see cmd/brutal.go in mole. Loopback only.
import http.server, os, re, socketserver, subprocess, threading, time, urllib.parse

# The two env overrides exist so the keeper can be run and tested off the node.
# Unset — which is how systemd starts it — they are exactly the old constants.
RATE_FILE = os.environ.get("MOLE_BRUTAL_RATE_FILE", "/etc/mole-brutal-rate")
PORT = int(os.environ.get("MOLE_BRUTAL_PORT", ` + fmt.Sprint(core.BrutalKeeperPort) + `))
LISTEN = "127.0.0.1"
LEG_PORT = 443
BLOCK = os.urandom(1 << 20)  # 1 MiB of incompressible bytes, repeated for /bulk

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
        m = re.search(r"(\d+\.\d+\.\d+\.\d+)\]?:\d+$", cols[3])
        if m:
            ips.add(m.group(1))
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
    """Install the pacing rule for every current peer.

    Returns (applied, peers, detail), where applied counts rules this call could
    account for, and it is what the client is told: a rate that was received
    and not installed is a rate that is not in effect, and the endpoint used
    to answer 200 to that case exactly as it did to success. That made the
    client's confirmation unfalsifiable, which is how REALITY ran unpaced for
    three days while the log said it was paced at 100 Mbps.
    """
    rate = read_rate()
    ok, detail = module_ok()
    ips = peers()
    if not ok:
        return 0, len(ips), detail
    n = 0
    with lock:
        for ip in ips:
            if not force and applied.get(ip) == rate:
                n += 1
                continue
            r = subprocess.run(["brutalctl", "add", ip + "/32", str(rate)], capture_output=True, text=True)
            if r.returncode == 0:
                applied[ip] = rate
                n += 1
            else:
                applied.pop(ip, None)
                detail = (r.stderr or r.stdout or "brutalctl exited %d" % r.returncode).strip().replace("\n", " ")[:200]
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
            # Applied synchronously, not in a thread: the caller is asking
            # whether the rate is in effect, and an answer sent before the
            # work is done cannot say.
            got, npeers, detail = apply(True)
            body = "rate=%d\napplied=%d\npeers=%d\n" % (read_rate(), got, npeers)
            if detail:
                body += "detail=%s\n" % detail
            self.reply(body, 200 if got else 503)
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
