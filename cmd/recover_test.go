package cmd

import (
	"fmt"
	"testing"
)

// A deploy that returns before Vultr has assigned an address leaves an entry
// with an instance id and nothing else. Filling it in is deterministic — one
// source, one correct value — so it happens without being asked, and these are
// the rules it follows.
func TestFillAddressesRecordsOnlyWhatIsMissing(t *testing.T) {
	local := []Server{
		{Name: "pending", InstanceID: "i-1"},
		{Name: "already-known", InstanceID: "i-2", IP: "10.0.0.2"},
		{Name: "imported", IP: "10.0.0.3"}, // no instance id: not ours to reconcile
		{Name: "not-on-vultr", InstanceID: "i-missing"},
		{Name: "no-address-yet", InstanceID: "i-4"},
	}
	remote := []serverInstance{
		{ID: "i-1", MainIP: "203.0.113.1", V6MainIP: "2001:db8::1"},
		{ID: "i-2", MainIP: "203.0.113.99"}, // drift: must NOT be adopted
		{ID: "i-4", MainIP: "0.0.0.0"},      // placeholder, not an address
	}

	if got := fillAddresses(local, remote); got != 1 {
		t.Fatalf("filled %d entries, want 1", got)
	}
	if local[0].IP != "203.0.113.1" || local[0].IPv6 != "2001:db8::1" {
		t.Errorf("pending entry got %q/%q, want the remote address", local[0].IP, local[0].IPv6)
	}
	// A local address that disagrees with the remote one means something
	// happened that this repair cannot interpret; adopting it silently would
	// paper over it, so it is left for `mole server reconcile` to report.
	if local[1].IP != "10.0.0.2" {
		t.Errorf("an entry that already had an address was overwritten with %q", local[1].IP)
	}
	if local[2].IP != "10.0.0.3" {
		t.Errorf("an entry with no instance id was touched: %q", local[2].IP)
	}
	if local[3].IP != "" {
		t.Errorf("an entry with no matching instance got an address: %q", local[3].IP)
	}
	if local[4].IP != "" {
		t.Errorf("0.0.0.0 was recorded as a real address: %q", local[4].IP)
	}
}

func TestFillAddressesIsIdempotent(t *testing.T) {
	local := []Server{{Name: "a", InstanceID: "i-1"}}
	remote := []serverInstance{{ID: "i-1", MainIP: "203.0.113.1"}}

	if got := fillAddresses(local, remote); got != 1 {
		t.Fatalf("first pass filled %d, want 1", got)
	}
	if got := fillAddresses(local, remote); got != 0 {
		t.Errorf("second pass filled %d, want 0 — a repeat run must be a no-op", got)
	}
}

// `mole up` decides whether to repair by classifying the error ActiveServer
// returned. That classification used to match on message text, so rewording a
// message would have turned an automatic repair back into a manual step with
// nothing failing to announce it. Wrapping is the contract; assert it holds
// through the formatting ActiveServer actually applies.
func TestRecoverableStatesSurviveWrapping(t *testing.T) {
	stopped := fmt.Errorf("%w: %q", ErrServerStopped, "nrt-1")
	provisioning := fmt.Errorf("%w: %q has no address yet", ErrServerProvisioning, "nrt-1")

	if !isStoppedError(stopped) {
		t.Error("a wrapped stopped error was not recognised as recoverable")
	}
	if !isProvisioningError(provisioning) {
		t.Error("a wrapped provisioning error was not recognised as recoverable")
	}
	// The two repairs are different actions; confusing them would start a
	// halted instance when the address was merely late, or vice versa.
	if isProvisioningError(stopped) || isStoppedError(provisioning) {
		t.Error("the two recoverable states are not distinguishable")
	}
	// Anything else must stop and report rather than trigger a repair.
	for _, err := range []error{
		fmt.Errorf("no servers deployed"),
		fmt.Errorf("no active server"),
		fmt.Errorf("read local servers: permission denied"),
		nil,
	} {
		if isStoppedError(err) || isProvisioningError(err) {
			t.Errorf("%v was misclassified as automatically recoverable", err)
		}
	}
}

// The address a deploy waits for arrives inside an envelope. Reading it from
// the wrong level does not fail — it just never finds anything, so the deploy
// polls until its five-minute deadline and then reports "no address yet" for an
// instance that has been reachable almost the whole time.
func TestParseInstanceAddressReadsTheEnvelope(t *testing.T) {
	// The real shape of GET /v2/instances/{id}.
	ip, v6 := parseInstanceAddress([]byte(`{"instance":{"id":"i-1","main_ip":"203.0.113.7","v6_main_ip":"2001:db8::7"}}`))
	if ip != "203.0.113.7" || v6 != "2001:db8::7" {
		t.Fatalf("got %q/%q, want the wrapped address", ip, v6)
	}

	// Vultr's placeholder for "not assigned yet" must keep the deploy waiting,
	// not record an address nothing can connect to.
	if ip, _ := parseInstanceAddress([]byte(`{"instance":{"main_ip":"0.0.0.0"}}`)); ip != "" {
		t.Errorf("placeholder 0.0.0.0 was accepted as %q", ip)
	}
	if ip, _ := parseInstanceAddress([]byte(`{"instance":{"main_ip":""}}`)); ip != "" {
		t.Errorf("empty main_ip was accepted as %q", ip)
	}

	// Top-level main_ip is the shape this code used to expect and Vultr has
	// never sent. Accepting it would hide the bug rather than fix it.
	if ip, _ := parseInstanceAddress([]byte(`{"main_ip":"203.0.113.7"}`)); ip != "" {
		t.Errorf("unwrapped main_ip was accepted as %q", ip)
	}

	// Garbage keeps the poll going rather than crashing a deploy that has
	// already created a billable instance.
	if ip, _ := parseInstanceAddress([]byte(`not json`)); ip != "" {
		t.Errorf("unparseable body yielded %q", ip)
	}
}
