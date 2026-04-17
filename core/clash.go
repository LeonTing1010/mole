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

// SwitchSelector flips a selector outbound to the named option.
// Equivalent to: PUT /proxies/{name} {"name": option}.
func (c *ClashClient) SwitchSelector(name, option string) error {
	body, _ := json.Marshal(map[string]string{"name": option})
	req, err := http.NewRequest(http.MethodPut, c.base+"/proxies/"+url.PathEscape(name), bytes.NewReader(body))
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
		return fmt.Errorf("switch selector %s→%s: %s: %s", name, option, resp.Status, string(msg))
	}
	return nil
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
	client := &http.Client{Timeout: time.Duration(timeoutMs+2000) * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
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
