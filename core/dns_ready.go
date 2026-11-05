package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// Only full configs with a DNS inbound have a DNS readiness contract;
// isolated protocol helpers need neither TUN nor system DNS changes.
func engineDNSAddress(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var cfg struct {
		Inbounds []struct {
			Tag    string `json:"tag"`
			Listen string `json:"listen"`
			Port   uint16 `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	for _, in := range cfg.Inbounds {
		if in.Tag == "dns-in" {
			return net.JoinHostPort(in.Listen, fmt.Sprint(in.Port)), nil
		}
	}
	return "", nil
}

func waitDNSReady(ctx context.Context, addr string, hosts []string, exited <-chan struct{}) error {
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	var last error
	for {
		select {
		case <-exited:
			return fmt.Errorf("engine exited before DNS readiness")
		case <-ctx.Done():
			return fmt.Errorf("DNS readiness: %w (last query: %v)", ctx.Err(), last)
		default:
		}
		ready := true
		for _, host := range hosts {
			probe, cancel := context.WithTimeout(ctx, time.Second)
			ips, err := resolver.LookupIP(probe, "ip4", host)
			cancel()
			if err != nil || len(ips) == 0 {
				last = err
				ready = false
				break
			}
		}
		if ready {
			select {
			case <-exited:
				return fmt.Errorf("engine exited during DNS readiness")
			default:
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-exited:
		case <-time.After(100 * time.Millisecond):
		}
	}
}
