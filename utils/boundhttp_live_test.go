package utils

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBoundClientReachesTheAPI is the end-to-end check that the kill switch and
// the bound client agree — the thing no unit test can assert, because it is a
// property of the running host's PF anchor.
//
// Expected with mole up and the anchor armed:
//
//	ResolveDirect     → a real Vultr address, NOT 198.18.0.x (the fake-IP pool)
//	bound → baidu     → 200, proving IP_BOUND_IF really put the socket on the
//	                    physical interface (a CN address the anchor passes)
//	bound → Vultr API → 200, proving the exemption is loaded
//
// A blocked Vultr API here means the anchor predates the exemption: re-arm with
// `mole down && mole up`.
//
// Opt-in (MOLE_NET_TEST=1) because it talks to the real network and depends on
// host state, neither of which belongs in the default suite.
func TestBoundClientReachesTheAPI(t *testing.T) {
	if os.Getenv("MOLE_NET_TEST") == "" {
		t.Skip("set MOLE_NET_TEST=1 to run the live path check")
	}
	ips, err := ResolveDirect(context.Background(), APIHost)
	t.Logf("ResolveDirect(%s) = %v (err=%v)", APIHost, ips, err)

	for _, url := range []string{"https://www.baidu.com", "https://" + APIHost + "/v2/regions"} {
		c, err := NewBoundHTTPClient(4 * time.Second)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		start := time.Now()
		resp, err := c.Get(url)
		if err != nil {
			t.Logf("%-45s BLOCKED/failed in %v: %v", url, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		resp.Body.Close()
		t.Logf("%-45s http=%d in %v", url, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	}
}

// TestDeadDNSStillReachesTheCachedAddress exercises the case the destroy at a
// closing lid actually meets: the link is up but name resolution is gone.
//
// It fakes that by asking for a name that cannot resolve while seeding the
// cache with an address that answers. A dial that gets far enough to fail on
// the certificate proves the fallback ran — TLS only happens after connect.
// The failure it guards against is the opposite shape: a DNS timeout that eats
// the whole attempt, which is what a 5-second resolver did to two of the three
// attempts a lid close had on 2026-08-14.
//
// Opt-in for the same reason as the test above: real network, real host state.
func TestDeadDNSStillReachesTheCachedAddress(t *testing.T) {
	if os.Getenv("MOLE_NET_TEST") == "" {
		t.Skip("set MOLE_NET_TEST=1 to run the live path check")
	}
	ips, err := ResolveDirect(context.Background(), APIHost)
	if err != nil {
		t.Skipf("cannot resolve %s to seed the cache: %v", APIHost, err)
	}
	// Seed into a throwaway cache. The bind and the anchor are host state and
	// stay real; the cache is a file under $HOME, and a test has no business
	// writing a fake host into the live daemon's copy of it.
	t.Setenv("HOME", t.TempDir())

	// A name nothing can answer for, pointed at an address that can.
	const dead = "not-a-real-host.invalid"
	cacheDirectIPs(dead, ips)

	c, err := NewBoundHTTPClient(4 * time.Second)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	start := time.Now()
	resp, err := c.Get("https://" + dead + "/v2/regions")
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("unexpected success against %s — the cert cannot match that name", dead)
	}
	t.Logf("dead-DNS dial failed in %v: %v", elapsed.Round(time.Millisecond), err)

	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") {
		t.Errorf("error is %v, want a TLS/certificate failure — anything else means the cached address was never dialed", err)
	}
	if elapsed > directDNSTimeout+2*time.Second {
		t.Errorf("took %v; the dead lookup was supposed to be bounded at %v and leave the rest of the window to the dial",
			elapsed.Round(time.Millisecond), directDNSTimeout)
	}
}
