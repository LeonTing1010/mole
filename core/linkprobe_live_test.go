package core

import (
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Echoes the real default gateway. Skipped when there is none; fails when the
// socket mechanics are wrong (that is what it is for — the identifier rewrite
// and the IPv4 header on darwin datagram ICMP sockets are things a unit test
// cannot know).
func TestEchoGatewayLive(t *testing.T) {
	gw, ifn, err := utils.DefaultGateway()
	if err != nil {
		t.Skip("no default gateway:", err)
	}
	ok := 0
	var worst time.Duration
	for i := 0; i < 4; i++ {
		rtt, err := echoGateway(gw, ifn, 1500*time.Millisecond)
		if err == nil {
			ok++
			if rtt > worst {
				worst = rtt
			}
		} else {
			t.Logf("echo %d: %v", i, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if ok == 0 {
		t.Fatalf("no echo reply from %s via %s in 4 tries", gw, ifn)
	}
	t.Logf("gateway %s via %s: %d/4 answered, worst %s", gw, ifn, ok, worst)
}
