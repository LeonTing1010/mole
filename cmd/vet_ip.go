package cmd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// errAddressIntercepted marks a freshly assigned address that this network
// answers for locally — the address is unusable no matter how healthy the
// instance behind it is, because the answerer shadows the real host.
var errAddressIntercepted = errors.New("assigned address is intercepted on this network")

// vetRTTBound is the largest round trip that can still be a local forgery.
// Shanghai→Seoul cannot go below ~25ms (light alone wants 15 each way), and
// the forger measured on 2026-11-11 answered in 0.1–0.7ms. Same-city users
// are protected by fabricationNearby: a network that does not fabricate never
// reaches this comparison.
const vetRTTBound = 5 * time.Millisecond

// vetProbes is how many echoes vetAssignedIP sends, one per second.
const vetProbes = 15

// vetMinReplies keeps a booting host from being condemned on a handful of
// answers from the local "no route yet" helper: fewer replies than this and
// the verdict is silence, and silence is handled by the normal boot wait.
const vetMinReplies = 10

// vetAssignedIP decides whether a just-assigned address is worth waiting for.
//
// The forger gives itself away only by being impossibly fast, and only when
// this network fabricates at all — which fabricationNearby establishes first
// by probing two almost-certainly-unassigned neighbours of the target: a live
// neighbour answers at real international RTT, a fabricated one answers
// locally. When nothing nearby fabricates, a sub-5ms answer from the target
// is genuine (the operator is in the datacenter's city) and vetting passes.
func vetAssignedIP(ctx context.Context, ip string) (bool, string) {
	fab, detail := fabricationNearby(ip)
	if !fab {
		return false, detail
	}
	var replies []time.Duration
	for i := 0; i < vetProbes; i++ {
		if ctx.Err() != nil {
			return false, "vetting interrupted"
		}
		if rtt, err := icmpEchoRTT(ip, 800*time.Millisecond); err == nil {
			replies = append(replies, rtt)
		}
		select {
		case <-ctx.Done():
			return false, "vetting interrupted"
		case <-time.After(time.Second):
		}
	}
	return interceptionVerdict(replies)
}

// interceptionVerdict is the pure decision over the collected round trips,
// separated from the prober so the policy is testable without a network.
func interceptionVerdict(replies []time.Duration) (bool, string) {
	if len(replies) < vetMinReplies {
		return false, fmt.Sprintf("only %d/%d echoes answered — inconclusive, proceeding", len(replies), vetProbes)
	}
	slowest := time.Duration(0)
	for _, r := range replies {
		if r > slowest {
			slowest = r
		}
	}
	if slowest < vetRTTBound {
		return true, fmt.Sprintf("%d/%d echoes answered locally (slowest %s) — physically impossible for the region, the address is shadowed", len(replies), vetProbes, slowest.Round(time.Millisecond))
	}
	return false, fmt.Sprintf("echoes take %s at worst — real distance, address looks clean", slowest.Round(time.Millisecond))
}

// fabricationNearby probes two same-/24 neighbours of ip, offset far enough to
// be unassigned on any realistic allocation. An answer faster than vetRTTBound
// can only come from something local impersonating the address.
func fabricationNearby(ip string) (bool, string) {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return false, "not an IPv4 address; vetting skipped"
	}
	for _, delta := range []int{97, 199} {
		nb := make(net.IP, 4)
		copy(nb, v4)
		nb[3] = byte((int(v4[3]) + delta) % 256)
		if rtt, err := icmpEchoRTT(nb.String(), 1500*time.Millisecond); err == nil && rtt < vetRTTBound {
			return true, fmt.Sprintf("neighbour %s answered in %s — this network fabricates foreign addresses", nb, rtt.Round(time.Millisecond))
		}
	}
	return false, "no local fabrication detected near the assigned address"
}

// icmpEchoRTT sends one ICMP echo over a datagram socket — darwin allows that
// unprivileged — and waits for the reply. Mirrors core.echoGateway minus the
// interface binding: here the whole point is the route the OS picks.
func icmpEchoRTT(ip string, timeout time.Duration) (time.Duration, error) {
	dst := net.ParseIP(ip).To4()
	if dst == nil {
		return 0, fmt.Errorf("%q is not IPv4", ip)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return 0, fmt.Errorf("icmp socket: %w", err)
	}
	defer syscall.Close(fd)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	seq := uint16(time.Now().UnixNano() >> 16 & 0xffff)
	pkt := make([]byte, 8+16)
	pkt[0] = 8
	binary.BigEndian.PutUint16(pkt[4:6], 0x4d4f) // 'MO'; the kernel rewrites it
	binary.BigEndian.PutUint16(pkt[6:8], seq)
	binary.BigEndian.PutUint64(pkt[8:16], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint16(pkt[2:4], icmpChecksumVet(pkt))

	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], dst)
	start := time.Now()
	if err := syscall.Sendto(fd, pkt, 0, sa); err != nil {
		return 0, fmt.Errorf("icmp send: %w", err)
	}
	buf := make([]byte, 1500)
	deadline := start.Add(timeout)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return 0, fmt.Errorf("icmp recv: %w", err)
		}
		b := buf[:n]
		if len(b) >= 20 && b[0]>>4 == 4 {
			ihl := int(b[0]&0x0f) * 4
			if len(b) < ihl+8 {
				continue
			}
			b = b[ihl:]
		}
		if len(b) >= 8 && b[0] == 0 && binary.BigEndian.Uint16(b[6:8]) == seq {
			return time.Since(start), nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no echo reply within budget")
		}
	}
}

func icmpChecksumVet(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// tunnelDaemonRunning reports whether the supervisor is up. Deploy and verify
// must refuse when it is: probes to any node but the active one route into the
// tunnel and hairpin through the active node, which on 2026-11-11 cost two
// freshly created instances that were both healthy.
func tunnelDaemonRunning() bool {
	st, err := utils.ReadState()
	if err != nil || st == nil || st.PID <= 0 {
		return false
	}
	return processAlive(st.PID)
}

// processAlive works for the root-owned daemon from an unprivileged shell:
// signal 0 answers EPERM for a live process owned by someone else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
