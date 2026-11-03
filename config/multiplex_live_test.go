package config

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Quality/audit (2026-10-20): verify Vision + smux in the installed engine,
// including authenticated TLS and actual reuse across fresh application flows.
func TestRealSingboxVisionMultiplexReusesAuthenticatedConnections(t *testing.T) {
	// Why: a JSON option passing check does not prove body delivery or reuse.
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box unavailable")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "payload-through-vision")
	}))
	defer origin.Close()
	identity := httptest.NewTLSServer(nil)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.TLS.Certificates[0].Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(identity.TLS.Certificates[0].PrivateKey)
	identity.Close()
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	nodePort, ingress := freePort(), freePort()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	var sessions atomic.Int32
	go func() {
		for {
			conn, err := front.Accept()
			if err != nil {
				return
			}
			sessions.Add(1)
			go func(conn net.Conn) {
				defer conn.Close()
				peer, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort))
				if err != nil {
					return
				}
				defer peer.Close()
				go func() { io.Copy(peer, conn); peer.Close() }()
				io.Copy(conn, peer)
			}(conn)
		}
	}()
	const uuid = "00000000-0000-4000-8000-000000000000"
	outbound, err := parseServerURL(fmt.Sprintf("vless://%s@%s?security=tls&sni=example.com&flow=xtls-rprx-vision&mux=smux", uuid, front.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	outbound.TLS.Certificate = strings.Split(strings.TrimSpace(string(cert)), "\n")
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, data := range map[string][]byte{certPath: cert, keyPath: key} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"log": map[string]any{"level": "error"},
		"inbounds": []any{
			map[string]any{"type": "direct", "tag": "app", "listen": "127.0.0.1", "listen_port": ingress,
				"override_address": "127.0.0.1", "override_port": origin.Listener.Addr().(*net.TCPAddr).Port},
			map[string]any{"type": "vless", "tag": "node", "listen": "127.0.0.1", "listen_port": nodePort,
				"users":     []any{map[string]any{"uuid": uuid, "flow": "xtls-rprx-vision"}},
				"multiplex": map[string]any{"enabled": true},
				"tls":       map[string]any{"enabled": true, "certificate_path": certPath, "key_path": keyPath}},
		},
		"outbounds": []any{outbound, map[string]any{"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"rules": []any{map[string]any{"inbound": []string{"app"}, "outbound": outbound.Tag}}, "final": "direct"},
	}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	engine := exec.CommandContext(ctx, bin, "run", "-c", path)
	logFile, err := os.Create(filepath.Join(dir, "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	engine.Stdout, engine.Stderr = logFile, logFile
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { engine.Process.Kill(); engine.Wait() }()
	addr := fmt.Sprintf("127.0.0.1:%d", ingress)
	for until := time.Now().Add(3 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(until) {
			t.Fatal("engine did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 8; i++ {
		resp, err := client.Get("http://" + addr)
		if err != nil {
			logs, _ := os.ReadFile(logFile.Name())
			t.Fatalf("fresh flow %d: %v\n%s", i, err, logs)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(body) != "payload-through-vision" {
			t.Fatalf("flow %d: status=%d body=%q err=%v", i, resp.StatusCode, body, err)
		}
	}
	if got := sessions.Load(); got < 1 || got > 2 {
		t.Fatalf("8 sequential fresh flows opened %d authenticated sessions, want 1..2", got)
	}
}
