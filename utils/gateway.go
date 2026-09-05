package utils

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultGateway returns the IPv4 default gateway and the interface it sits
// behind, as `route -n get default` reports them.
//
// Read from the routing table rather than assumed: the home router is
// 192.168.1.1, a phone hotspot is 192.168.43.1, and the 2026-08-17 session
// spent its first measurements pinging the wrong one.
func DefaultGateway() (gateway, ifname string, err error) {
	cmd := exec.Command("/sbin/route", "-n", "get", "default")
	out, err := runWithin(cmd, 3*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("route get default: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "gateway":
			gateway = strings.TrimSpace(v)
		case "interface":
			ifname = strings.TrimSpace(v)
		}
	}
	if gateway == "" {
		return "", "", fmt.Errorf("no default gateway")
	}
	return gateway, ifname, nil
}

// runWithin runs cmd and kills it past the budget.
func runWithin(cmd *exec.Cmd, budget time.Duration) ([]byte, error) {
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return out, err
	case <-time.After(budget):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return nil, fmt.Errorf("%s: timed out after %s", cmd.Path, budget)
	}
}
