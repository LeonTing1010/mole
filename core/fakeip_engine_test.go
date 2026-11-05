package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Real-engine acceptance: retain a cached non-SNI application address across
// repeated SIGKILL, including a mature cache and a restart with no DNS activity.
func TestFakeIPRecoveryWithRealEngine(t *testing.T) {
	engine, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed")
	}
	out, err := exec.Command(engine, "version").Output()
	if err != nil || validateEngineVersion(string(out)) != nil {
		t.Fatalf("unqualified integration engine: %s (%v)", out, err)
	}
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err == nil && string(b) == "PING" {
					c.Write([]byte("PONG"))
				}
			}()
		}
	}()
	port := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	for _, scenario := range []string{"fresh", "mature", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			socks, dns := port(), port()
			cache := filepath.Join(dir, "cache.db")
			cfgPath := filepath.Join(dir, "config.json")
			cfg := map[string]any{"log": map[string]any{"level": "error"}, "dns": map[string]any{"servers": []any{map[string]any{"type": "fakeip", "tag": "fake", "inet4_range": "198.18.0.0/15"}, map[string]any{"type": "hosts", "tag": "hosts", "predefined": map[string]any{"a.example": []string{"127.0.0.1"}, "b.example": []string{"127.0.0.1"}, "c.example": []string{"127.0.0.1"}}}}, "rules": []any{map[string]any{"query_type": []string{"A"}, "server": "fake"}}, "final": "hosts"}, "inbounds": []any{map[string]any{"type": "mixed", "tag": "app", "listen": "127.0.0.1", "listen_port": socks}, map[string]any{"type": "direct", "tag": "dns-in", "listen": "127.0.0.1", "listen_port": dns}}, "outbounds": []any{map[string]any{"type": "direct", "tag": "direct", "domain_resolver": map[string]any{"server": "hosts"}}}, "route": map[string]any{"rules": []any{map[string]any{"action": "sniff"}, map[string]any{"inbound": []string{"dns-in"}, "action": "hijack-dns"}}, "final": "direct"}, "experimental": map[string]any{"cache_file": map[string]any{"enabled": true, "store_fakeip": true, "path": cache}}}
			// Match config.Save: create the shared cache privately before engine launch.
			if err := os.WriteFile(cache, nil, 0600); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(cfg)
			os.WriteFile(cfgPath, data, 0600)
			var p *exec.Cmd
			stop := func(hard bool) {
				if p == nil {
					return
				}
				if hard {
					p.Process.Kill()
				} else {
					p.Process.Signal(os.Interrupt)
				}
				done := make(chan struct{})
				go func() { p.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					p.Process.Kill()
					<-done
				}
				p = nil
			}
			defer stop(true)
			launch := func() {
				if err := recoverFakeIPCache(cfgPath); err != nil {
					t.Fatal(err)
				}
				p = exec.Command(engine, "run", "-c", cfgPath)
				if err := p.Start(); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 100; i++ {
					c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socks)), 20*time.Millisecond)
					if err == nil {
						c.Close()
						ready, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						err := waitDNSReady(ready, net.JoinHostPort("127.0.0.1", strconv.Itoa(dns)), []string{"a.example"}, make(chan struct{}))
						cancel()
						if err != nil {
							t.Fatalf("real engine DNS readiness: %v", err)
						}
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatal("engine not listening")
			}
			query := func(name string) net.IP {
				q := []byte{1, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
				start := 0
				for i := 0; i <= len(name); i++ {
					if i == len(name) || name[i] == '.' {
						q = append(q, byte(i-start))
						q = append(q, name[start:i]...)
						start = i + 1
					}
				}
				q = append(q, 0, 0, 1, 0, 1)
				c, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dns)))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				c.Write(q)
				b := make([]byte, 512)
				n, err := c.Read(b)
				if err != nil || n < 4 {
					t.Fatalf("DNS: %v", err)
				}
				return net.IP(append([]byte(nil), b[n-4:n]...))
			}
			application := func(ip net.IP) {
				t.Helper()
				c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socks)), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				c.Write([]byte{5, 1, 0})
				b := make([]byte, 2)
				if _, err := io.ReadFull(c, b); err != nil {
					t.Fatal(err)
				}
				req := append([]byte{5, 1, 0, 1}, ip.To4()...)
				req = binary.BigEndian.AppendUint16(req, uint16(origin.Addr().(*net.TCPAddr).Port))
				c.Write(req)
				reply := make([]byte, 10)
				if _, err := io.ReadFull(c, reply); err != nil || reply[1] != 0 {
					t.Fatalf("cached %s failed: status=%v err=%v", ip, reply, err)
				}
				c.Write([]byte("PING"))
				b = make([]byte, 4)
				if _, err := io.ReadFull(c, b); err != nil || string(b) != "PONG" {
					t.Fatalf("cached application payload %q: %v", b, err)
				}
			}
			launch()
			first := query("a.example")
			application(first)
			if scenario == "mature" {
				stop(false)
				launch()
				application(first)
			}
			if scenario == "stale" {
				time.Sleep(10200 * time.Millisecond)
			}
			second := query("b.example")
			application(second)
			for i := 0; i < 3; i++ {
				stop(true)
				launch()
				application(first)
				application(second)
			}
			third := query("c.example")
			if third.Equal(first) || third.Equal(second) {
				t.Fatal("recovered allocator reused an existing application address")
			}
			application(third)
			stop(true)
			launch()
			application(first)
			application(second)
			application(third)
			stop(false)
			st, _ := os.Stat(cache)
			if st.Mode().Perm() != 0600 {
				t.Fatalf("cache mode: %o", st.Mode().Perm())
			}
		})
	}
}
