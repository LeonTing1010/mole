package utils

import (
	"context"
	"net"
	"net/http"
	"time"
)

// directResolver is a DNS resolver that queries 223.5.5.5 (Alibaba DNS)
// directly, bypassing the system DNS which is taken over to the TUN gateway
// (172.19.0.1) when mole is running. The route config sends 223.5.5.5/32
// through the "direct" outbound, so DNS queries don't go through the proxy.
var directResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
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
