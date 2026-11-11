#!/usr/bin/env python3
# mole-brutal-keeper: see cmd/brutal.go in mole. Loopback only.
import http.server, ipaddress, os, socketserver, subprocess, threading, time, urllib.parse

# The two env overrides exist so the keeper can be run and tested off the node.
# Unset — which is how systemd starts it — they are exactly the old constants.
RATE_FILE = os.environ.get("MOLE_BRUTAL_RATE_FILE", "/etc/mole-brutal-rate")
GAIN_FILE = os.environ.get("MOLE_BRUTAL_GAIN_FILE", "/etc/mole-brutal-gain")
PROC_RULES = "/proc/net/tcp_brutal/rules"
PORT = int(os.environ.get("MOLE_BRUTAL_PORT", 8098))
LISTEN = "127.0.0.1"
LEG_PORT = 443
BLOCK = os.urandom(1 << 20)  # 1 MiB of incompressible bytes, repeated for /bulk
DEFAULT_MBPS = max(1, int(os.environ.get("MOLE_DEFAULT_MBPS", "100")))
DEFAULT_GAIN = max(5, min(80, int(os.environ.get("MOLE_DEFAULT_GAIN", "20"))))  # 2.0x window (Brutal INIT_CWND_GAIN). 2026-11-10 A/B on good IPs showed
                    # gain=20 + rate=600 (473 Mbps non-peak) beats gain=15 + rate=600 (213 Mbps).
                    # The /gain HTTP endpoint lets operators tune this per-IP without re-deploying;
                    # gain=15 may still be optimal on lossy IPs where too many in-flight packets
                    # cause loss/retransmit collapse.
PROC_HANDLE = open(PROC_RULES, "w") if os.path.exists(PROC_RULES) else None

def read_rate():
    try:
        return max(1, int(open(RATE_FILE).read().strip()))
    except Exception:
        return DEFAULT_MBPS

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

    A rule alone paces nothing: the module engages only when a route marks
    the destination for the brutal congestion control, which brutcalctl used
    to install alongside the rule and the direct /proc write does not. So
    each sweep also reconciles the proto-233 route — without it the rule
    lists but carries zero bytes (brutalctl list shows ROUTE=no, SENT=0.0).
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
        ensure_routes(ips)
    if n == 0 and not detail:
        detail = "no peer established on TCP/%d" % LEG_PORT
    return n, len(ips), detail

def ensure_routes(ips):
    """Reconcile the proto-233 routes that mark each peer for the brutal CC.

    Never raises: routing failure must not take the rate endpoint down with
    it, and the next sweep retries. The default gateway is re-read every
    sweep because the node can in principle be re-addressed.
    """
    if not ips:
        return
    try:
        have = set()
        out = subprocess.run(["ip", "route", "show", "proto", "233"],
                             capture_output=True, text=True).stdout
        for line in out.splitlines():
            cols = line.split()
            if cols:
                have.add(cols[0])
        dflt = subprocess.run(["ip", "route", "show", "default"],
                              capture_output=True, text=True).stdout.split()
        gw = dflt[dflt.index("via") + 1]
        dev = dflt[dflt.index("dev") + 1]
    except Exception:
        return
    for ip in ips:
        try:
            prefix = str(ipaddress.ip_network(ip))
        except ValueError:
            continue
        if prefix in have:
            continue
        try:
            subprocess.run(["ip", "route", "replace", prefix, "via", gw, "dev", dev,
                            "congctl", "lock", "brutal", "proto", "233"],
                           capture_output=True)
        except Exception:
            pass

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
                g = max(5, min(80, int(q.get("gain", [str(read_gain())])[0])))
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
