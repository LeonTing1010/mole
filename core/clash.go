package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ClashClient talks to sing-box's Clash-compatible control API.
type ClashClient struct {
	base string
	http *http.Client
}

// NewClashClient builds a client targeting the given external_controller address.
func NewClashClient(addr string) *ClashClient {
	return &ClashClient{
		base: "http://" + addr,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// TestDelay probes a proxy outbound's end-to-end latency to the given URL.
// Returns the measured delay in ms. An error means the proxy failed the probe.
func (c *ClashClient) TestDelay(proxy, testURL string, timeoutMs int) (int, error) {
	u := fmt.Sprintf("%s/proxies/%s/delay?url=%s&timeout=%d",
		c.base, url.PathEscape(proxy), url.QueryEscape(testURL), timeoutMs)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	// Use a dedicated client with idle-connection timeout so sockets don't
	// accumulate across repeated probes.
	client := &http.Client{
		Timeout: time.Duration(timeoutMs+2000) * time.Millisecond,
		Transport: &http.Transport{
			IdleConnTimeout:     5 * time.Second,
			MaxIdleConnsPerHost: 1,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		client.CloseIdleConnections()
	}()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("delay probe %s: %s: %s", proxy, resp.Status, string(msg))
	}
	var out struct {
		Delay int `json:"delay"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Delay, nil
}

// SelectProxy points a selector outbound at one of its members. This is how the
// bandwidth scheduler switches between the peak/off-peak hy2 outbounds without
// restarting sing-box: the TUN and existing connections are untouched, and new
// connections route through the newly selected member.
func (c *ClashClient) SelectProxy(selector, member string) error {
	body, _ := json.Marshal(map[string]string{"name": member})
	u := fmt.Sprintf("%s/proxies/%s", c.base, url.PathEscape(selector))
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("select %s→%s: %s: %s", selector, member, resp.Status, string(msg))
	}
	return nil
}

// Ping returns nil once the Clash API is reachable (used to wait for sing-box startup).
func (c *ClashClient) Ping() error {
	resp, err := c.http.Get(c.base + "/version")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("clash api: %s", resp.Status)
	}
	return nil
}

// Selection returns the member a selector currently points at. This is the only
// way the supervisor can tell what is *actually* live: `mole ceiling` drives the
// same selector out of band, so the scheduler's own record of what it last set
// is a statement of intent, not a fact about the tunnel.
func (c *ClashClient) Selection(selector string) (string, error) {
	resp, err := c.http.Get(fmt.Sprintf("%s/proxies/%s", c.base, url.PathEscape(selector)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("selection %s: %s: %s", selector, resp.Status, string(msg))
	}
	var out struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Now == "" {
		return "", fmt.Errorf("selection %s: no current member in response", selector)
	}
	return out.Now, nil
}

// Connection is one live connection as sing-box reports it, reduced to the
// three fields the direct-exemption learner needs.
type Connection struct {
	// Chains is the outbound stack, innermost first: ["direct"] for a direct
	// dial, ["proxy-reality", "proxy"] when the "proxy" selector resolved to
	// its REALITY member. Chains[0] is therefore the outbound that actually
	// opened the socket, which is the only one that decides whether the packets
	// leave through a physical interface.
	Chains []string
	// Host is the sniffed destination name — the SNI for TLS. Empty for a dial
	// straight to an address, which is exactly the case the learner refuses to
	// act on: there is no name to check against the rules.
	Host string
	// DestIP is populated for direct dials. Connections routed into the tunnel
	// carry a domain rather than an address and leave this empty.
	DestIP string
}

// Connections lists the live connections. Used by the direct-exemption learner
// to see what sing-box's "direct" outbound is dialing — including the dials PF
// is dropping, which still appear here with zero bytes moved, and which is what
// makes the learner able to fix a block it can only observe after the fact.
func (c *ClashClient) Connections() ([]Connection, error) {
	resp, err := c.http.Get(c.base + "/connections")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("connections: %s: %s", resp.Status, string(msg))
	}
	var out struct {
		Connections []struct {
			Chains   []string `json:"chains"`
			Metadata struct {
				Host          string `json:"host"`
				DestinationIP string `json:"destinationIP"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	conns := make([]Connection, 0, len(out.Connections))
	for _, c := range out.Connections {
		conns = append(conns, Connection{
			Chains: c.Chains,
			Host:   c.Metadata.Host,
			DestIP: c.Metadata.DestinationIP,
		})
	}
	return conns, nil
}
