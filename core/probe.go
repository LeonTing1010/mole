package core

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// ipBoundIf is darwin's IP_BOUND_IF (netinet/in.h). Setting it scopes the
// socket to one interface, bypassing the routing table — the same mechanism
// sing-box's auto_detect_interface uses to keep its own packets out of the
// TUN it created.
const ipBoundIf = 25

// ProbeHy2UDP checks whether the hy2/QUIC server at addr is reachable over
// UDP, from OUTSIDE the tunnel.
//
// Two properties are load-bearing; both were violated by the previous
// implementation (diagnosed 2026-07-03, the "走了直连" root-cause session):
//
//  1. The socket is bound to the physical default interface (IP_BOUND_IF).
//     A plain dial gets captured by sing-box's TUN, and — the VPS IP having
//     no direct route rule — rides the hy2 tunnel to the VPS and back to
//     itself. Such a probe measures "can I write into my own tunnel" and
//     stays green through a total path blackout.
//
//  2. The payload is a QUIC long-header packet with an unsupported (GREASE)
//     version, padded to 1200 bytes. RFC 9000 obliges the server to answer
//     with a Version Negotiation packet, so a healthy server RESPONDS
//     (~35 bytes, no crypto, no state). Silence is therefore a signal —
//     path dark — instead of being indistinguishable from health, which is
//     what the old junk-byte probe suffered from (a healthy server dropped
//     the junk silently, so timeout had to be treated as OK and a blackhole
//     could never be seen).
//
// Verdicts: response → ProbeAlive (+real RTT); "connection refused" (kernel's
// surfacing of ICMP port unreachable) → ProbeRefused; deadline → ProbeSilent;
// anything preventing the probe from running → ProbeError.
func ProbeHy2UDP(addr string, timeout time.Duration) (time.Duration, utils.ProbeVerdict, error) {
	ifi, err := defaultPhysicalInterface()
	if err != nil {
		return 0, utils.ProbeError, fmt.Errorf("no physical interface for direct probe: %w", err)
	}
	d := net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if cerr := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIf, ifi.Index)
			}); cerr != nil {
				return cerr
			}
			return serr
		},
	}
	conn, err := d.Dial("udp4", addr)
	if err != nil {
		return 0, utils.ProbeError, fmt.Errorf("dial via %s: %w", ifi.Name, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, utils.ProbeError, err
	}
	start := time.Now()
	if _, err := conn.Write(vnElicitPacket()); err != nil {
		if isRefused(err) {
			return 0, utils.ProbeRefused, fmt.Errorf("write: %w", err)
		}
		return 0, utils.ProbeError, fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, 256)
	_, err = conn.Read(buf)
	rtt := time.Since(start)
	switch {
	case err == nil:
		// Any datagram back from the server (the socket is connected, so only
		// this peer can be the source) proves path + process. We don't insist
		// on strict VN framing — being liberal here can't create a false 🟢
		// about the thing we measure.
		return rtt, utils.ProbeAlive, nil
	case isRefused(err):
		return 0, utils.ProbeRefused, fmt.Errorf("read: %w", err)
	default:
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return 0, utils.ProbeSilent, fmt.Errorf("no response within %s", timeout)
		}
		return 0, utils.ProbeError, fmt.Errorf("read: %w", err)
	}
}

// vnElicitPacket builds a QUIC long-header packet carrying a GREASE version
// (0x1a2a3a4a) with random 8-byte connection IDs, padded to the 1200-byte
// minimum so the server won't discard it. Per RFC 9000 §6 the server must
// answer a version it doesn't support with a Version Negotiation packet.
func vnElicitPacket() []byte {
	pkt := make([]byte, 1200)
	pkt[0] = 0xC0 // long header + fixed bit
	binary.BigEndian.PutUint32(pkt[1:5], 0x1a2a3a4a)
	pkt[5] = 8
	_, _ = rand.Read(pkt[6:14])
	pkt[14] = 8
	_, _ = rand.Read(pkt[15:23])
	return pkt
}

// isRefused reports whether err is the kernel surfacing an ICMP
// port-unreachable on a connected UDP socket.
func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		(err != nil && strings.Contains(err.Error(), "connection refused"))
}

// defaultPhysicalInterface picks the interface real traffic leaves through —
// up, non-loopback, carrying a global unicast IPv4, and not one of macOS's
// virtual/utility interfaces. Prefers en0 (the Mac's primary interface) when
// it qualifies. The TUN (utunN) is explicitly excluded: the whole point is to
// probe from outside it.
func defaultPhysicalInterface() (*net.Interface, error) {
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
			return &ifi, nil
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
