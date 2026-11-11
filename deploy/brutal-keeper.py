#!/usr/bin/env python3
"""TCP Brutal rule keeper — mole server side.

Writes /proc/net/tcp_brutal/rules to set cwnd_gain + rate per-peer.
Loopback-only HTTP on :8098; endpoints:

  GET /rate?mbps=N&gain=M    — set rate and (optionally) gain atomically
  GET /gain?g=N              — set gain only
  GET /bulk?bytes=N          — throughput sample
  GET /health                — module + peer + rate status
"""
import http.server, json, os, subprocess, threading, time, urllib.parse

BRUTAL_DIR = "/proc/net/tcp_brutal"
DEFAULT_MBPS = int(os.environ.get("MOLE_DEFAULT_MBPS", "100"))
DEFAULT_GAIN = int(os.environ.get("MOLE_DEFAULT_GAIN", "20"))
RATE_FILE = "/etc/mole-brutal-rate"
GAIN_FILE = "/etc/mole-brutal-gain"

def _read_first(path, default):
    try:
        with open(path) as f: return f.read().strip() or default
    except OSError: return default

def _brutal_supported(): return os.path.isdir(BRUTAL_DIR)

def _build_rules(mbps, gain):
    rules, peers_found = [], []
    try:
        with open("/proc/net/tcp") as f: lines = f.readlines()[1:]
        peers = set()
        for line in lines:
            parts = line.split()
            if len(parts) < 10: continue
            st, local, remote = parts[3], parts[1], parts[2]
            if st != "01": continue
            try:
                ip_hex, port_hex = remote.split(":")
                ip = ".".join(str(int(ip_hex[i:i+2],16)) for i in (0,2,4,6))
                port = int(port_hex, 16)
                if port == 443: peers.add(ip)
            except (ValueError, IndexError): pass
        for ip in peers:
            rules.append(f"add {ip}/32 rate={mbps*125000} gain={gain}")
            peers_found.append(ip)
    except OSError: pass
    return rules, peers_found

def _ensure_route(ip):
    # A rule alone paces nothing: the module engages only via a route that
    # marks the destination for the brutal CC (brutalctl installs one with
    # 'congctl lock brutal proto 233'; writing /proc directly does not).
    try:
        out = subprocess.run(["ip", "route", "show", "proto", "233"],
                             capture_output=True, text=True).stdout
        if any(line.split() and line.split()[0] == ip + "/32" for line in out.splitlines()):
            return
        dflt = subprocess.run(["ip", "route", "show", "default"],
                              capture_output=True, text=True).stdout.split()
        gw, dev = dflt[dflt.index("via") + 1], dflt[dflt.index("dev") + 1]
        subprocess.run(["ip", "route", "replace", ip + "/32", "via", gw, "dev", dev,
                        "congctl", "lock", "brutal", "proto", "233"], capture_output=True)
    except Exception:
        pass

def _apply_rules(mbps=None, gain=None):
    if not _brutal_supported(): return False
    mbps = mbps if mbps is not None else int(_read_first(RATE_FILE, str(DEFAULT_MBPS)))
    gain = gain if gain is not None else int(_read_first(GAIN_FILE, str(DEFAULT_GAIN)))
    try:
        with open(f"{BRUTAL_DIR}/rules", "w") as f: f.write("flush\n")
        rules, peers = _build_rules(mbps, gain)
        for rule in rules:
            try:
                with open(f"{BRUTAL_DIR}/rules", "w") as f: f.write(rule + "\n")
            except OSError: pass
        for ip in peers:
            _ensure_route(ip)
    except OSError: return False
    return True

class Handler(http.server.BaseHTTPRequestHandler):
    def _reply(self, code, body):
        if isinstance(body, dict): body = json.dumps(body)
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body.encode())
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/rate":
            mbps = int(q.get("mbps", [DEFAULT_MBPS])[0])
            gain = int(q.get("gain", [DEFAULT_GAIN])[0])
            open(RATE_FILE, "w").write(str(mbps))
            if gain > 0: open(GAIN_FILE, "w").write(str(gain))
            ok = _apply_rules(mbps, gain if gain > 0 else None)
            self._reply(200, {"ok": ok, "rate_mbps": mbps, "gain": gain, "module": _brutal_supported()})
        elif u.path == "/gain":
            gain = int(q.get("g", [DEFAULT_GAIN])[0])
            open(GAIN_FILE, "w").write(str(gain))
            _apply_rules(gain=gain)
            self._reply(200, {"ok": True, "gain": gain})
        elif u.path == "/health":
            peers = 0
            try:
                with open("/proc/net/tcp") as f:
                    for line in f.readlines()[1:]:
                        parts = line.split()
                        if len(parts) >= 10 and parts[3] == "01":
                            try:
                                if int(parts[2].split(":")[1],16) == 443: peers += 1
                            except: pass
            except OSError: pass
            self._reply(200, {"brutal_module": _brutal_supported(), "peers_on_443": peers, "rate_mbps": int(_read_first(RATE_FILE, str(DEFAULT_MBPS))), "gain": int(_read_first(GAIN_FILE, str(DEFAULT_GAIN)))})
        elif u.path == "/bulk":
            n = min(int(q.get("bytes", [65536])[0]), 16*1024*1024)
            self._reply(200, os.urandom(n))
        elif u.path == "/status":
            self._reply(200, {"module": _brutal_supported(), "rate": _read_first(RATE_FILE, str(DEFAULT_MBPS)), "gain": _read_first(GAIN_FILE, str(DEFAULT_GAIN))})
        else: self._reply(404, {"error": "not found"})
    def log_message(self, *a, **kw): pass

def _loop():
    _apply_rules()
    while True:
        time.sleep(5)
        _apply_rules()

threading.Thread(target=_loop, daemon=True).start()
server = http.server.HTTPServer(("127.0.0.1", 8098), Handler)
server.serve_forever()
