package core

import (
	"bytes"
	"encoding/binary"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// deobfuscateReference mirrors hysteria2's salamander Deobfuscate as published
// in apernet/hysteria (extras/obfs/salamander.go): the packet is
// [8-byte salt][payload XOR repeating BLAKE2b-256(PSK || salt)]. Written out
// separately here so the test states the wire contract independently of the
// implementation it checks.
func deobfuscateReference(in []byte, psk string) []byte {
	if len(in) <= salamanderSaltLen {
		return nil
	}
	key := blake2b.Sum256(append([]byte(psk), in[:salamanderSaltLen]...))
	out := make([]byte, len(in)-salamanderSaltLen)
	for i, c := range in[salamanderSaltLen:] {
		out[i] = c ^ key[i%len(key)]
	}
	return out
}

// The server deobfuscates before QUIC ever sees the bytes. If our wrapping
// doesn't invert exactly, the packet becomes noise, is dropped unanswered, and
// the probe reports a dark path through a healthy tunnel.
func TestSalamanderRoundTripsToTheOriginalPacket(t *testing.T) {
	const psk = "deadbeefcafebabe"
	pkt := vnElicitPacket()

	wire := salamanderObfuscate(pkt, psk)
	if len(wire) != len(pkt)+salamanderSaltLen {
		t.Fatalf("wire length = %d, want payload+%d", len(wire), salamanderSaltLen)
	}
	got := deobfuscateReference(wire, psk)
	if !bytes.Equal(got, pkt) {
		t.Fatal("server-side deobfuscation does not recover the elicitation packet")
	}
	// And what it recovers must still be a valid version-negotiation trigger.
	if got[0]&0x80 == 0 {
		t.Error("recovered packet lost its long-header bit")
	}
	if v := binary.BigEndian.Uint32(got[1:5]); v != 0x1a2a3a4a {
		t.Errorf("recovered GREASE version = %#x, want 0x1a2a3a4a", v)
	}
}

// A wrong key must not accidentally decode. This is what makes the probe's
// silence meaningful: only a server sharing the key can answer.
func TestSalamanderWrongKeyDoesNotDecode(t *testing.T) {
	pkt := vnElicitPacket()
	wire := salamanderObfuscate(pkt, "the-right-key")
	if bytes.Equal(deobfuscateReference(wire, "the-wrong-key"), pkt) {
		t.Fatal("a mismatched key still recovered the packet")
	}
}

// Salt is per-packet, so two probes must not be byte-identical on the wire —
// a fixed prefix would be a trivial fingerprint of exactly the kind this whole
// change exists to remove.
func TestSalamanderSaltVariesPerPacket(t *testing.T) {
	pkt := vnElicitPacket()
	a := salamanderObfuscate(pkt, "psk")
	b := salamanderObfuscate(pkt, "psk")
	if bytes.Equal(a[:salamanderSaltLen], b[:salamanderSaltLen]) {
		t.Error("two packets shared a salt")
	}
	if bytes.Equal(a, b) {
		t.Error("two obfuscations of the same packet are identical on the wire")
	}
}

// Servers without obfs must keep receiving the bare packet — wrapping it would
// break every existing deployment, including the one currently in production.
func TestSalamanderNoPasswordIsPassthrough(t *testing.T) {
	pkt := vnElicitPacket()
	if got := salamanderObfuscate(pkt, ""); !bytes.Equal(got, pkt) {
		t.Error("an unobfuscated server would receive a wrapped packet")
	}
}
