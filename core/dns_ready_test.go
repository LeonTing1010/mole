package core

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// Quality/what: a living process or open socket is not DNS readiness.
func TestDNSReadinessRequiresAnAnswer(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-answer", true: "answer"}[answer], func(t *testing.T) {
			c, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			go func() {
				b := make([]byte, 1024)
				for {
					n, a, err := c.ReadFrom(b)
					if err != nil {
						return
					}
					r := append([]byte(nil), b[:n]...)
					r[2] = 0x81
					r[3] = 0x80
					// Go's resolver may include EDNS; retain only the DNS question.
					end := 12
					for end < len(r) && r[end] != 0 {
						end += int(r[end]) + 1
					}
					end += 5
					if end > len(r) {
						continue
					}
					r = r[:end]
					binary.BigEndian.PutUint16(r[10:12], 0)
					if answer {
						binary.BigEndian.PutUint16(r[6:8], 1)
						r = append(r, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 127, 0, 0, 1)
					}
					c.WriteTo(r, a)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
			defer cancel()
			err = waitDNSReady(ctx, c.LocalAddr().String(), []string{"ready.example"}, make(chan struct{}))
			if (err == nil) != answer {
				t.Fatalf("answer=%t err=%v", answer, err)
			}
		})
	}
}

// Safety/what-x-what: readiness stops when its engine has exited.
func TestDNSReadinessStopsOnEngineExit(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	if err := waitDNSReady(context.Background(), "127.0.0.1:1", []string{"ready.example"}, exited); err == nil {
		t.Fatal("dead engine ready")
	}
}
