package core

import (
	"bytes"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

var nodeDialANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type nodeDialSnapshot struct {
	sequence uint64
	last     time.Time
}

// Observe only this engine's actual node dial errors, never an origin rejection.
// Passive sightings can accelerate measurement; they are not failed probes.
type nodeDialSensor struct {
	mu          sync.Mutex
	target      string
	tail        []byte
	dropped     bool
	observation nodeDialSnapshot
}

func (s *nodeDialSensor) configure(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = ""
	if host, port, err := net.SplitHostPort(addr); err == nil && net.ParseIP(host) != nil {
		s.target = net.JoinHostPort(host, port)
	}
	s.tail, s.dropped, s.observation = nil, false, nodeDialSnapshot{}
}

func (s *nodeDialSensor) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(s.tail)+len(part) > 4096 {
			s.tail = nil
			s.dropped = true
		}
		if !s.dropped {
			s.tail = append(s.tail, part...)
		}
		if i < 0 {
			break
		}
		if !s.dropped && s.target != "" {
			line := string(nodeDialANSI.ReplaceAll(s.tail, nil))
			if strings.Contains(line, "open connection to ") && strings.Contains(line, "using outbound/vless["+RealityLegTag+"]:") && strings.Contains(line, "dial tcp "+s.target+": ") && (strings.HasSuffix(line, "i/o timeout") || strings.HasSuffix(line, "connect: connection refused")) {
				s.observation.sequence++
				s.observation.last = time.Now()
			}
		}
		s.tail = s.tail[:0]
		s.dropped = false
		p = p[i+1:]
	}
	return n, nil
}

func (s *nodeDialSensor) snapshot() nodeDialSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observation
}

var singboxNodeDials nodeDialSensor
