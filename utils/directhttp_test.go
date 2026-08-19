package utils

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// The cache exists for one call: the destroy at a closing lid, which gets ~30
// seconds and often gets them on a link that has already gone dark. On
// 2026-08-09 the lookup for api.vultr.com timed out after 5s and the whole
// "direct" management path was blind before it opened a socket. Remembering
// where the host lived last time turns that into a dial.
func TestDirectIPCacheRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := CachedDirectIPs("api.vultr.com"); got != nil {
		t.Fatalf("empty cache returned %v, want nil", got)
	}

	cacheDirectIPs("api.vultr.com", []string{"1.2.3.4", "5.6.7.8"})
	got := CachedDirectIPs("api.vultr.com")
	if len(got) != 2 || got[0] != "1.2.3.4" || got[1] != "5.6.7.8" {
		t.Fatalf("CachedDirectIPs = %v, want [1.2.3.4 5.6.7.8]", got)
	}

	// A second host must not evict the first — the file holds a map, not one
	// entry, and the management host has to survive whatever else resolves.
	cacheDirectIPs("example.com", []string{"9.9.9.9"})
	if got := CachedDirectIPs("api.vultr.com"); len(got) != 2 {
		t.Errorf("caching a second host clobbered the first: %v", got)
	}
	if got := CachedDirectIPs("example.com"); len(got) != 1 || got[0] != "9.9.9.9" {
		t.Errorf("CachedDirectIPs(example.com) = %v, want [9.9.9.9]", got)
	}

	// A later answer replaces the earlier one; the point is the LAST address
	// that worked, not every address that ever did.
	cacheDirectIPs("api.vultr.com", []string{"10.0.0.1"})
	if got := CachedDirectIPs("api.vultr.com"); len(got) != 1 || got[0] != "10.0.0.1" {
		t.Errorf("CachedDirectIPs after refresh = %v, want [10.0.0.1]", got)
	}
}

// A corrupt or truncated cache must read as "nothing cached", never panic. It
// is written on a laptop that gets its power cut by a closing lid.
func TestDirectIPCacheSurvivesGarbage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(APIIPsPath(), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := CachedDirectIPs("api.vultr.com"); got != nil {
		t.Errorf("garbage cache returned %v, want nil", got)
	}
	// And a subsequent write must repair it rather than append to it.
	cacheDirectIPs("api.vultr.com", []string{"1.2.3.4"})
	if got := CachedDirectIPs("api.vultr.com"); len(got) != 1 {
		t.Errorf("cache did not recover from garbage: %v", got)
	}
}

// ResolveDirectCached is what the PF exemption and the bound client call when
// they have somewhere to be. A live answer wins; a dead resolver falls back to
// the last one that worked and says so.
func TestResolveDirectCachedFallsBackToTheCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cacheDirectIPs("api.vultr.com", []string{"1.2.3.4"})

	// A name that cannot resolve, with a context already past its deadline so
	// the test does not wait on a real lookup.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	ips, live, err := ResolveDirectCached(ctx, "api.vultr.com")
	if live {
		t.Error("reported a live answer from a resolver that could not run")
	}
	if err == nil {
		t.Error("swallowed the resolution failure; callers that care cannot tell")
	}
	if len(ips) != 1 || ips[0] != "1.2.3.4" {
		t.Fatalf("ResolveDirectCached = %v, want the cached [1.2.3.4]", ips)
	}

	// With nothing cached there is nothing to fall back to, and the caller must
	// get the error rather than an empty list that reads as success.
	ips, live, err = ResolveDirectCached(ctx, "never-resolved.invalid")
	if err == nil || live || len(ips) != 0 {
		t.Errorf("ResolveDirectCached(uncached) = (%v, %v, %v), want (nil, false, error)", ips, live, err)
	}
}

// bindControl must never fail the dial itself. It scopes the socket when it
// can and steps aside when it cannot — a machine mid Wi-Fi flap has no physical
// interface to name, and that is exactly the state this code runs in.
//
// Note what binding costs: a socket pinned to en0 cannot reach 127.0.0.1, which
// is correct and is why nothing local may be dialed through these clients.
// Everything they talk to (223.5.5.5, api.vultr.com) is off-machine.
func TestBindControlNeverFailsTheDial(t *testing.T) {
	var got error
	control := bindControl()
	// A UDP "dial" binds and connects without sending anything, so this
	// exercises the Control callback without depending on the network being up.
	conn, err := (&net.Dialer{
		Timeout: time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			got = control(network, address, c)
			return got
		},
	}).Dial("udp4", "223.5.5.5:53")
	if got != nil {
		t.Fatalf("bindControl returned %v; it must degrade to an unbound dial, never refuse one", got)
	}
	if err != nil {
		t.Skipf("no route to dial at all (%v) — the Control contract still held", err)
	}
	conn.Close()
}
