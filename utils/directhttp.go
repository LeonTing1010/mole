package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ipBoundIf is darwin's IP_BOUND_IF (netinet/in.h). Setting it scopes the
// socket to one interface, bypassing the routing table — the same mechanism
// sing-box's auto_detect_interface uses to keep its own packets out of the
// TUN it created.
const ipBoundIf = 25

// bindControl returns a net.Dialer Control func that scopes the socket to the
// physical default interface. It resolves the interface per dial rather than
// once at construction because the answer changes under us — Wi-Fi drops and
// re-associates, and the daemon outlives several of those.
//
// A nil Control (no physical interface, or the lookup failed) is the honest
// fallback: the dial then follows the routing table, which is worse but is
// still better than refusing to dial at all. Callers that need the guarantee
// check DefaultPhysicalInterface themselves.
func bindControl() func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifi, err := DefaultPhysicalInterface()
		if err != nil {
			return nil
		}
		var serr error
		if cerr := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIf, ifi.Index)
		}); cerr != nil {
			return cerr
		}
		return serr
	}
}

// directResolver is a DNS resolver that queries 223.5.5.5 (Alibaba DNS)
// directly, bypassing the system DNS which is taken over to the TUN gateway
// (172.19.0.1) when mole is running.
//
// The socket is bound to the physical interface, and that binding is the whole
// point. Pointing the query at 223.5.5.5 only decides WHO answers; without
// IP_BOUND_IF the packet still leaves through whatever the routing table says,
// which while mole is up is the TUN. sing-box then forwards it out its "direct"
// outbound — fine when sing-box is healthy, and useless in the one situation
// this resolver exists for. On 2026-08-09 the lid closed into a dead uplink and
// this lookup timed out after 5s, so the "direct" management path was blind
// before it ever opened a socket, and a quarter of the destroy's 20s budget was
// gone. Bound, the query is exactly as alive as the physical link is.
// directDNSTimeout bounds one query to 223.5.5.5.
//
// It is a single UDP exchange to a nameserver one hop off the local link: it
// answers in milliseconds or it is not going to. Five seconds used to be the
// budget, and on 2026-08-14 that was most of the destroy: attempts are 6s, so
// a dead resolver left roughly one second to dial the cached address that
// exists precisely for that case, and the last two of the lid close's three
// attempts never got to try it at all.
const directDNSTimeout = 1500 * time.Millisecond

var directResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: directDNSTimeout, Control: bindControl()}
		// Use the caller's network type (udp/tcp) so DNS-over-TCP
		// fallback works for large responses. Always connect to the
		// direct DNS server regardless of what address Go's resolver
		// read from the (taken-over) system DNS config.
		return d.DialContext(ctx, network, "223.5.5.5:53")
	},
}

// newDirectHTTPClient returns an HTTP client that resolves domains via
// directResolver instead of the system DNS. Use this for requests that MUST
// bypass the proxy regardless of VPN state — e.g. fetching boot rule-sets at
// cold start, before sing-box exists.
//
// "Direct" here means the NAME is resolved outside the tunnel, not the
// connection. Once the TUN owns the default route, the socket this dials still
// leaves through it. Callers that need the packets themselves to stay off the
// tunnel want NewBoundHTTPClient.
func newDirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:  timeout,
				Resolver: directResolver,
			}).DialContext,
			IdleConnTimeout:       10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}
}

// NewBoundHTTPClient returns an HTTP client whose sockets are pinned to the
// physical default interface (IP_BOUND_IF) and whose names resolve through
// directResolver — so both the lookup and the packets stay outside the tunnel.
//
// This is what the management plane needs. A plain http.Client reaches
// api.vultr.com through whatever the routing table says, and while mole is up
// that is the TUN: the name resolves to a fake IP and the request rides the
// very hy2 tunnel it may be trying to tear down. On 2026-08-05 that cost a
// destroy — the lid closed during a UDP blackout, the DELETE went into the
// dead tunnel, and the server billed all day. Bound, the same call is a
// ~0.9s round trip that does not care whether the tunnel is alive.
//
// The PF kill switch blocks foreign egress on physical interfaces, so a bound
// socket also needs the destination passed there; see AllowDirectHost.
func NewBoundHTTPClient(timeout time.Duration) (*http.Client, error) {
	// Checked up front, and not reused for the binding itself: an error here
	// means there is no physical path at all right now, which the caller wants
	// to know before it spends any of its budget. bindControl re-reads the
	// interface per dial, so a Wi-Fi flap between construction and use is
	// followed rather than baked in.
	if _, err := DefaultPhysicalInterface(); err != nil {
		return nil, err
	}
	d := &net.Dialer{
		Timeout:  timeout,
		Resolver: directResolver,
		Control:  bindControl(),
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Force IPv4: the kill switch blocks inet6 outright on physical
			// interfaces, so an AAAA that wins happy-eyeballs would be dropped
			// and the call would spend its whole budget timing out.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, serr := net.SplitHostPort(addr)
				if serr != nil || net.ParseIP(host) != nil {
					return d.DialContext(ctx, "tcp4", addr)
				}
				// Resolution and dialing are split so the first cannot eat the
				// budget of the second. Name resolution is the part of this
				// path most likely to be dead while the link itself works — a
				// DNS server gone quiet, a query lost in a stalled Wi-Fi
				// association — and letting the Dialer do it inline meant a
				// 6-second attempt could spend 5 of them waiting for 223.5.5.5
				// before the cached address got its turn. Bounded here, the
				// fallback gets the rest of the window instead of the crumbs.
				//
				// The last address that worked is a good guess: dialing it
				// changes only where the packets go, since the Transport takes
				// SNI and the Host header from the URL, not from here.
				rctx, cancel := context.WithTimeout(ctx, directDNSTimeout)
				live, rerr := ResolveDirect(rctx, host)
				cancel()

				firstErr := rerr
				seen := make(map[string]bool, len(live)+2)
				for _, ip := range append(live, CachedDirectIPs(host)...) {
					if seen[ip] {
						continue
					}
					seen[ip] = true
					conn, derr := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip, port))
					if derr == nil {
						return conn, nil
					}
					if firstErr == nil {
						firstErr = derr
					}
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("no address for %s", host)
				}
				return nil, firstErr
			},
			IdleConnTimeout:       10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
	}, nil
}

// ResolveDirect returns host's IPv4 addresses as seen by directResolver.
//
// Needed because the system resolver answers from sing-box's fake-IP pool once
// mole is up: `dig api.vultr.com` returns 198.18.0.x, an address that exists
// only inside the tunnel. Anything that must be reached outside it — a PF pass
// rule, a bound socket — has to ask 223.5.5.5 for the real answer.
func ResolveDirect(ctx context.Context, host string) ([]string, error) {
	addrs, err := directResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s directly: %w", host, err)
	}
	var out []string
	for _, a := range addrs {
		if ip4 := a.IP.To4(); ip4 != nil {
			out = append(out, ip4.String())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("resolve %s directly: no IPv4 address", host)
	}
	cacheDirectIPs(host, out)
	return out, nil
}

// ResolveDirectCached returns host's addresses, preferring a live answer and
// falling back to the last one that worked. live says which it got.
//
// The fallback is for the callers that have somewhere to be. A PF exemption or
// a management DELETE at a closing lid is better served by an address that was
// correct an hour ago than by an error: api.vultr.com does not move often, and
// the alternative on the night this was written was doing nothing at all while
// the server billed until morning. Callers that need to know the difference
// read live; callers that only need somewhere to send packets ignore it.
func ResolveDirectCached(ctx context.Context, host string) (ips []string, live bool, err error) {
	ips, err = ResolveDirect(ctx, host)
	if err == nil {
		return ips, true, nil
	}
	if cached := CachedDirectIPs(host); len(cached) > 0 {
		return cached, false, err
	}
	return nil, false, err
}

// directIPCache is the on-disk record of what each host last resolved to,
// keyed by host. Small, rewritten whole, and read under the same lock — the
// daemon and the CLI both touch it, and it is never big enough for anything
// cleverer to pay off.
type directIPCache map[string]directIPEntry

type directIPEntry struct {
	IPs []string  `json:"ips"`
	At  time.Time `json:"at"`
}

var directIPMu sync.Mutex

// CachedDirectIPs returns the last addresses host resolved to outside the
// tunnel, or nil if it has never resolved. Deliberately unexpiring: a stale
// address that still answers is worth more than no address, and a stale one
// that does not answer costs a single failed dial.
func CachedDirectIPs(host string) []string {
	directIPMu.Lock()
	defer directIPMu.Unlock()
	return readDirectIPCache()[host].IPs
}

func cacheDirectIPs(host string, ips []string) {
	directIPMu.Lock()
	defer directIPMu.Unlock()
	c := readDirectIPCache()
	if c == nil {
		c = directIPCache{}
	}
	c[host] = directIPEntry{IPs: ips, At: time.Now()}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	// Best effort throughout: this is a cache. A write that fails costs the
	// next lookup its fallback, not correctness.
	path := APIIPsPath()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return
	}
	ChownToMoleDirOwner(path)
}

func readDirectIPCache() directIPCache {
	data, err := os.ReadFile(APIIPsPath())
	if err != nil {
		return nil
	}
	var c directIPCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return c
}

// DefaultPhysicalInterface picks the interface real traffic leaves through —
// up, non-loopback, carrying a global unicast IPv4, and not one of macOS's
// virtual/utility interfaces. Prefers en0 (the Mac's primary interface) when
// it qualifies. The TUN (utunN) is explicitly excluded: the whole point of
// every caller is to get outside it.
func DefaultPhysicalInterface() (*net.Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	virtual := []string{"lo", "utun", "gif", "stf", "awdl", "llw", "bridge", "ap", "anpi", "vmenet", "feth"}
	var first *net.Interface
	for i := range ifs {
		ifi := ifs[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		skip := false
		for _, p := range virtual {
			if strings.HasPrefix(ifi.Name, p) {
				skip = true
				break
			}
		}
		if skip || !hasGlobalIPv4(&ifi) {
			continue
		}
		if ifi.Name == "en0" {
			return &ifs[i], nil
		}
		if first == nil {
			first = &ifs[i]
		}
	}
	if first == nil {
		return nil, fmt.Errorf("no up physical interface with an IPv4 address")
	}
	return first, nil
}

func hasGlobalIPv4(ifi *net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			ip4 := ipn.IP.To4()
			if ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return true
			}
		}
	}
	return false
}
