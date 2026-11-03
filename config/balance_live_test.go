package config

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/core"
)

// Quality/audit (2026-10-07): real engine routing and selector lifetime semantics.
func TestRealSingboxDualFlowsRemainBoundAcrossWeightChanges(t *testing.T) {
	// Why: JSON shape alone cannot prove simultaneous use or connection continuity.
	sb, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box unavailable")
	}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	backend := func(name string) *httptest.Server {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hold" {
				fmt.Fprintln(w, name+"-start")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				fmt.Fprintln(w, name+"-end")
			} else {
				fmt.Fprintln(w, name)
			}
		}))
		t.Cleanup(origin.Close)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				t.Error("upstream expected CONNECT")
				w.WriteHeader(400)
				return
			}
			peer, err := net.Dial("tcp", origin.Listener.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer peer.Close()
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
			rw.Flush()
			go func() { io.Copy(peer, rw); peer.Close() }()
			io.Copy(conn, peer)
		}))
		t.Cleanup(proxy.Close)
		return proxy
	}
	reality, hy2 := backend("tcp"), backend("udp")
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return port
	}
	portOf := func(s *httptest.Server) int { return s.Listener.Addr().(*net.TCPAddr).Port }
	ingress, controller := freePort(), freePort()
	cfg := &SingboxConfig{
		Log:      LogConfig{Level: "error"},
		Inbounds: []InboundConfig{{Type: "http", Tag: "test-in", Listen: "127.0.0.1", ListenPort: ingress}},
		Outbounds: append([]OutboundConfig{
			{Type: "selector", Tag: core.ProxySelectorTag, Outbounds: []string{core.RealityLegTag, core.Hy2ProbeSelectorTag}, Default: core.RealityLegTag},
			{Type: "http", Tag: core.RealityLegTag, Server: "127.0.0.1", ServerPort: portOf(reality)},
			{Type: "http", Tag: core.Hy2ProbeSelectorTag, Server: "127.0.0.1", ServerPort: portOf(hy2)},
		}, flowSelectors()...),
		// A sniffed domain has no destination IP, so CIDR routes miss it. The
		// final route must still pass through a flow selector, as it does on TUN.
		Route:        RouteConfig{Rules: flowRoutes([]RouteRule{{IPCIDR: []string{"0.0.0.0/0"}, Outbound: core.ProxySelectorTag}}), Final: core.ProxySelectorTag, AutoDetectInterface: true},
		Experimental: &ExperimentalConfig{ClashAPI: &ClashAPIConfig{ExternalController: fmt.Sprintf("127.0.0.1:%d", controller)}},
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	engine := exec.CommandContext(ctx, sb, "run", "-c", path)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { engine.Process.Kill(); engine.Wait() }()
	clash := core.NewClashClient(fmt.Sprintf("127.0.0.1:%d", controller))
	deadline := time.Now().Add(3 * time.Second)
	for clash.Ping() != nil {
		if time.Now().After(deadline) {
			t.Fatal("engine control API did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", ingress))
	clients := []*http.Client{}
	defer func() {
		for _, c := range clients {
			c.CloseIdleConnections()
		}
	}()
	request := func(base int, path string) *http.Response {
		// Each retry advances one whole bucket cycle, preserving the chosen lane.
		port := 0
		for candidate := base; candidate < 65000; candidate += 256 {
			l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(candidate))
			if err != nil {
				continue
			}
			l.Close()
			port = candidate
			break
		}
		if port == 0 {
			t.Fatal("no free source port in the requested bucket")
		}
		dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: dialer.DialContext, DisableKeepAlives: true}}
		clients = append(clients, client)
		resp, err := client.Get("http://example.invalid" + path)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	tcp := request(49152, "/hold")
	defer tcp.Body.Close()
	tcpReader := bufio.NewReader(tcp.Body)
	if line, _ := tcpReader.ReadString('\n'); line != "tcp-start\n" {
		t.Fatalf("TCP bucket: %q", line)
	}
	if _, err := core.ApplyBalancedLanes(clash, 4); err != nil {
		t.Fatal(err)
	}
	udp := request(49184, "/hold")
	defer udp.Body.Close()
	udpReader := bufio.NewReader(udp.Body)
	if line, _ := udpReader.ReadString('\n'); line != "udp-start\n" {
		t.Fatalf("UDP bucket: %q", line)
	}
	// Both responses remain open while the weight changes to all UDP.
	if _, err := core.ApplyBalancedLanes(clash, core.BalanceLanes); err != nil {
		t.Fatal(err)
	}
	fresh := request(49408, "/fresh")
	body, err := io.ReadAll(fresh.Body)
	fresh.Body.Close()
	if err != nil || strings.TrimSpace(string(body)) != "udp" {
		t.Fatalf("new connection ignored weight: %q %v", body, err)
	}
	unblock()
	for name, reader := range map[string]*bufio.Reader{"tcp": tcpReader, "udp": udpReader} {
		body, err := io.ReadAll(reader)
		if err != nil || strings.TrimSpace(string(body)) != name+"-end" {
			t.Fatalf("existing %s connection moved or broke: %q %v", name, body, err)
		}
	}
}
