package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
)

// A separate, unprivileged engine pins each local forwarder to the NEW node.
// Using the active daemon here could accidentally verify the previous server.
type protocolProbe struct {
	server Server
	ports  map[string]int
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
	dir    string
	log    *os.File
}

func newProtocolProbe(parent context.Context, srv Server) (*protocolProbe, error) {
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		return nil, fmt.Errorf("protocol verification requires sing-box: %w", err)
	}
	ifi, err := utils.DefaultPhysicalInterface()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "mole-verify-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	p := &protocolProbe{server: srv, ports: map[string]int{}, cancel: cancel, done: make(chan error, 1), dir: dir}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	var inbounds, outbounds, rules []any
	h, r := wantsLegs(srv)
	for _, leg := range []string{"hy2", "reality"} {
		if (leg == "hy2" && !h) || (leg == "reality" && !r) {
			continue
		}
		uri := srv.URI()
		if leg == "reality" && !srv.isReality() {
			uri = srv.StandbyURI()
		}
		out, err := config.ParseOutbound(uri)
		if err != nil {
			return nil, fmt.Errorf("invalid %s identity", leg)
		}
		out.Tag = "verify-" + leg
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		p.ports[leg] = port
		outbounds = append(outbounds, out)
		inbounds = append(inbounds, map[string]any{"type": "direct", "tag": leg, "listen": "127.0.0.1", "listen_port": port, "override_address": "cloudflare.com", "override_port": 443})
		rules = append(rules, map[string]any{"inbound": []string{leg}, "action": "route", "outbound": out.Tag})
	}
	data, err := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "inbounds": inbounds, "outbounds": outbounds, "route": map[string]any{"rules": rules, "default_interface": ifi.Name}})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	p.log, err = os.OpenFile(filepath.Join(dir, "engine.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	p.cmd = exec.CommandContext(ctx, bin, "run", "-c", path)
	p.cmd.Stdout = p.log
	p.cmd.Stderr = p.log
	if err = p.cmd.Start(); err != nil {
		p.cmd = nil
		return nil, err
	}
	go func() { p.done <- p.cmd.Wait() }()
	// Only allow the helper to start; this is not node readiness evidence.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		p.cmd = nil
		return nil, fmt.Errorf("verification engine exited before probing")
	case <-time.After(200 * time.Millisecond):
	}
	ok = true
	return p, nil
}

func (p *protocolProbe) Close() {
	p.cancel()
	if p.cmd != nil {
		<-p.done
		p.cmd = nil
	}
	if p.log != nil {
		p.log.Close()
	}
	os.RemoveAll(p.dir)
}

func (p *protocolProbe) Probe(parent context.Context, leg string) legReport {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	addr := net.JoinHostPort(serverHost(p.server), strconv.Itoa(int(p.server.Port)))
	var v utils.ProbeVerdict
	var err error
	if leg == "hy2" {
		_, v, err = core.ProbeHy2UDPContext(ctx, addr, p.server.ObfsPassword, nodeProbeTimeout)
	} else {
		_, v, err = core.ProbeTCPContext(ctx, p.server.realityAddr(), nodeProbeTimeout)
	}
	if v != utils.ProbeAlive {
		return legReport{Verdict: v, Err: err, Detail: describeProbeError(err)}
	}
	start := time.Now()
	transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.ports[leg])))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://cloudflare.com/cdn-cgi/trace", nil)
	resp, err := client.Do(req)
	if err != nil {
		return legReport{Verdict: utils.ProbeError, Err: fmt.Errorf("%s authenticated HTTPS request failed", leg), Detail: "authenticated HTTPS request failed"}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || resp.StatusCode != 200 || len(body) == 0 || len(body) > 65536 {
		return legReport{Verdict: utils.ProbeError, Err: fmt.Errorf("invalid HTTPS response (HTTP %d)", resp.StatusCode), Detail: "invalid HTTPS payload"}
	}
	return legReport{Verdict: utils.ProbeAlive, RTT: time.Since(start), Detail: "authenticated HTTPS payload received"}
}

func describeProbeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
