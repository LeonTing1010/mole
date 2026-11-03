package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Gateway-echo cadence. Five seconds fills the LinkWindow in a minute, and one
// echo every five seconds is far below the 2 pps at which a home router's ICMP
// rate limiter starts inventing loss (measured 2026-08-22: `ping -i 0.2` alone
// produced 50% loss on a clean link).
const (
	linkProbeEvery   = 5 * time.Second
	linkProbeTimeout = 1500 * time.Millisecond
)

// runLinkProbe echoes the default gateway on a timer and folds the result into
// the local-link verdict. See utils/link.go for what the verdict is for.
//
// The gateway is re-read on every uplink change (the link watcher pokes this
// through the same channel it uses for re-dials), and the window is emptied
// when it changes: echoes to the old network say nothing about the new one.
func (s *Supervisor) runLinkProbe(ctx context.Context) {
	var gw, ifname string
	refresh := func() {
		g, i, err := utils.DefaultGateway()
		if err != nil {
			g, i = "", ""
		}
		if g != gw {
			s.stateMu.Lock()
			s.state.ResetLink()
			s.state.LinkGateway = g
			s.stateMu.Unlock()
			if g != "" {
				log.Printf("link probe: echoing gateway %s via %s", g, i)
			}
		}
		gw, ifname = g, i
	}
	refresh()
	t := time.NewTicker(linkProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
		if gw == "" {
			refresh()
			if gw == "" {
				continue
			}
		}
		rtt, err := echoGateway(gw, ifname, linkProbeTimeout)
		if err != nil && !isEchoTimeout(err) {
			// A socket-level failure (interface gone, address changed) says
			// nothing about the hop; find out where we are now and try again
			// next beat.
			refresh()
			continue
		}
		s.stateMu.Lock()
		wasBad := s.state.LinkBad
		s.state.RecordLink(int(rtt/time.Millisecond), err == nil)
		nowBad := s.state.LinkBad
		loss, med := s.state.LinkLoss(), s.state.LinkMedianMs()
		s.noteHealthLocked()
		_ = utils.WriteState(s.stateSnapshotLocked())
		s.stateMu.Unlock()
		switch {
		case nowBad && !wasBad:
			log.Printf("link: LOCAL LINK BAD — gateway %s lost %d/%d echoes, median %dms; this is the Wi-Fi, not the path, and every leg or ceiling decision is frozen until it clears",
				gw, loss, utils.LinkWindow, med)
			go s.logWiFiPHY()
		case wasBad && !nowBad:
			log.Printf("link: local link clean again — gateway %s lost %d/%d, median %dms", gw, loss, utils.LinkWindow, med)
		}
	}
}

// linkBad reports the latched local-link verdict.
func (s *Supervisor) linkBad() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state.LinkBad
}

// echoGateway sends one ICMP echo to gw from a socket bound to ifname and
// waits for the reply. Returns the RTT, or a timeout error when nothing came
// back within the budget.
//
// A datagram ICMP socket (SOCK_DGRAM/IPPROTO_ICMP) is used first: darwin allows
// it unprivileged and it needs no IP header handling on send. The daemon runs
// as root, so a raw socket is the fallback rather than the rule.
func echoGateway(gw, ifname string, timeout time.Duration) (time.Duration, error) {
	dst := net.ParseIP(gw).To4()
	if dst == nil {
		return 0, fmt.Errorf("gateway %q is not IPv4", gw)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
		if err != nil {
			return 0, fmt.Errorf("icmp socket: %w", err)
		}
	}
	defer syscall.Close(fd)
	if ifname != "" {
		if ifi, ierr := net.InterfaceByName(ifname); ierr == nil {
			_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, ipBoundIf, ifi.Index)
		}
	}
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	id := uint16(time.Now().UnixNano() & 0xffff)
	seq := uint16(time.Now().UnixNano() >> 16 & 0xffff)
	pkt := make([]byte, 8+16)
	pkt[0] = 8 // echo request
	binary.BigEndian.PutUint16(pkt[4:6], id)
	binary.BigEndian.PutUint16(pkt[6:8], seq)
	binary.BigEndian.PutUint64(pkt[8:16], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint16(pkt[2:4], icmpChecksum(pkt))

	sa := &syscall.SockaddrInet4{}
	copy(sa.Addr[:], dst)
	start := time.Now()
	if err := syscall.Sendto(fd, pkt, 0, sa); err != nil {
		// "No route to host" on the send is the ARP for the gateway failing:
		// the radio is associated but the first hop is not answering. That is
		// a lost echo, not a broken socket — seen 2026-09-05 on four
		// consecutive sends during an otherwise ordinary minute.
		if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
			return 0, errEchoTimeout
		}
		return 0, fmt.Errorf("icmp send: %w", err)
	}
	buf := make([]byte, 1500)
	deadline := start.Add(timeout)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return 0, errEchoTimeout
			}
			return 0, fmt.Errorf("icmp recv: %w", err)
		}
		b := buf[:n]
		// darwin hands the IPv4 header back on both socket types.
		if len(b) >= 20 && b[0]>>4 == 4 {
			ihl := int(b[0]&0x0f) * 4
			if len(b) < ihl+8 {
				continue
			}
			b = b[ihl:]
		}
		if len(b) >= 8 && b[0] == 0 && binary.BigEndian.Uint16(b[6:8]) == seq {
			// The kernel rewrites the identifier on datagram ICMP sockets, so
			// only the sequence is matched.
			return time.Since(start), nil
		}
		if time.Now().After(deadline) {
			return 0, errEchoTimeout
		}
	}
}

var errEchoTimeout = errors.New("no echo reply within budget")

func isEchoTimeout(err error) bool { return errors.Is(err, errEchoTimeout) }

func icmpChecksum(b []byte) uint16 {
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

// logWiFiPHY records the radio's own view once per bad stretch: channel,
// signal, noise and the current transmit rate. This is the evidence that
// separates "my Wi-Fi is bad" from "a neighbour is on my channel" — on
// 2026-08-27 the answer was a -67dBm neighbour on the same channel as our
// -71dBm link, and on 2026-09-01 a PHY rate flapping 51<->97 Mbps. It is
// sampled here rather than on a timer because system_profiler costs a second
// of CPU, which is nothing once an episode and everything every five seconds.
func (s *Supervisor) logWiFiPHY() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/system_profiler", "SPAirPortDataType").Output()
	if err != nil {
		return
	}
	var keep []string
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		for _, k := range []string{"Channel:", "Signal / Noise:", "Transmit Rate:", "PHY Mode:"} {
			if strings.HasPrefix(t, k) {
				keep = append(keep, t)
				break
			}
		}
		if len(keep) >= 4 {
			break
		}
	}
	if len(keep) == 0 {
		return
	}
	phy := strings.Join(keep, "; ")
	log.Printf("link: radio says %s", phy)
	s.stateMu.Lock()
	s.state.LinkPHY = phy
	s.stateMu.Unlock()
}
