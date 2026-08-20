package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
)

// serverIdentity is the per-server secret material that makes the tunnel both
// verifiable and unremarkable on the wire. It is minted on this machine at
// deploy time rather than on the VPS, because the client has to keep the
// certificate in order to pin it, and a cert generated on the far side can
// never be read back over a channel we trust.
type serverIdentity struct {
	CertPEM      string // leaf certificate, pinned by the client
	KeyPEM       string // matching private key, installed on the VPS only
	SNI          string // SAN on the cert; the name the client asks for
	ObfsPassword string // salamander key, must match the server exactly
	// SSHPubKey, when found, is installed as root's only credential so the
	// node can turn off password authentication. Vultr publishes a generated
	// root password through its own API for a window after provisioning, so
	// "nobody knows the password" is not a property this system has: anyone
	// holding the account API key holds root on every node, and from there the
	// tunnel password, the obfs key and the TLS private key. A key-only node
	// closes that path and stops presenting a brute-forceable root login to
	// the internet at the same time.
	SSHPubKey string
}

// newServerIdentity mints a fresh identity for one server.
//
// The certificate is self-signed on purpose. The alternative — a real domain
// plus an ACME certificate — trades one exposure for another: it would publish
// this host in the public Certificate Transparency log, which is a worse deal
// than pinning, and it adds a renewal that can silently expire the tunnel.
// Pinning gives full MITM resistance with nothing to buy, renew, or announce.
//
// The SAN is random and unbranded. Impersonating a well-known site is what the
// old provisioning did (`/CN=bing.com`), and it backfires twice over: the name
// contradicts the destination's ASN, and an active prober that completes a
// handshake receives a self-signed certificate for a site that demonstrably
// does not issue its own — a conclusive proxy signature. A name nobody has
// heard of makes no claim that can be checked and therefore none that can fail.
//
// A SAN is mandatory, not decorative: Go dropped Common-Name fallback in 1.15,
// so a CN-only certificate — exactly what the old script produced — fails
// verification outright once pinning is on. The old setup only worked because
// it verified nothing at all.
func newServerIdentity() (*serverIdentity, error) {
	sni, err := randomHostname()
	if err != nil {
		return nil, err
	}
	obfs, err := randomHexString(16)
	if err != nil {
		return nil, err
	}

	pub, err := localSSHPubKey()
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	// Backdated an hour so a VPS whose clock has not yet synced at first boot
	// doesn't reject its own certificate as not-yet-valid. Long-lived because
	// there is no renewal path to a pinned cert: expiry would take the tunnel
	// down with no way to fix it from the wrong side of the outage.
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: sni},
		DNSNames:              []string{sni},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	return &serverIdentity{
		CertPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		SNI:          sni,
		ObfsPassword: obfs,
		SSHPubKey:    pub,
	}, nil
}

// localSSHPubKey returns this machine's SSH public key, preferring ed25519.
// Returns "" (not an error) when there is none: the node then keeps password
// authentication, because provisioning a box you cannot log into at all is a
// worse outcome than one with a strong generated password. The caller reports
// which of the two happened.
func localSSHPubKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		b, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err != nil {
			continue
		}
		if k := strings.TrimSpace(string(b)); strings.HasPrefix(k, "ssh-") || strings.HasPrefix(k, "ecdsa-") {
			return k, nil
		}
	}
	return "", nil
}

// randomHostname returns an unbranded name for the certificate's SAN. Under
// salamander obfuscation this never appears on the wire at all — it exists to
// satisfy verification, not to convince anyone of anything.
func randomHostname() (string, error) {
	label, err := randomHexString(6)
	if err != nil {
		return "", err
	}
	return "node-" + label + ".net", nil
}

func randomHexString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ── REALITY identity ───────────────────────────────────────────

// realityIdentity is the per-server secret material for a VLESS+REALITY node.
//
// REALITY needs no certificate of its own: the server borrows a real one, live,
// from a third-party site it forwards the client's ClientHello to. That removes
// everything the hysteria2 leg has to carry — no self-signed leaf, no pin, no
// SAN that has to match — and replaces it with an X25519 keypair plus a short
// id, which together let the server recognise its own clients inside what is
// otherwise an ordinary TLS 1.3 handshake to Dest.
type realityIdentity struct {
	UUID       string // VLESS user id
	PrivateKey string // base64url X25519 scalar, installed on the VPS only
	PublicKey  string // base64url X25519 point, carried by the client
	ShortID    string // 8-byte hex, must be one of the server's short_id list
	Dest       string // the site whose TLS handshake the server steals
	SSHPubKey  string // as in serverIdentity: key-only root, or "" when absent
}

// defaultRealityDest is the borrowed handshake target.
//
// The choice is load-bearing and silently fatal when wrong. REALITY completes
// the TLS 1.3 handshake against Dest and needs to derive the same keys the
// target negotiated — so a target that picks a hybrid post-quantum group
// (X25519MLKEM768), which modern browsers offer and an increasing number of
// large sites now select, leaves the server unable to finish. It shows up as
// `handshake did not complete successfully` on the server and a bare EOF on the
// client, with the REALITY authentication itself reported as fine: the failure
// looks exactly like a broken protocol implementation rather than a bad
// hostname. www.microsoft.com is one such target, and cost this project an
// afternoon; www.apple.com was measured working from an icn node.
//
// Requirements for any replacement: TLS 1.3, HTTP/2, a certificate chain it
// serves itself (no redirect to another host), no PQ hybrid group, and traffic
// to it from the VPS's region that looks unremarkable.
const defaultRealityDest = "www.apple.com"

// newRealityIdentity mints the material for one REALITY node. Like
// newServerIdentity, the secret is generated here rather than on the VPS: the
// client needs the public half to connect at all, and material generated on the
// far side could only be read back over the very channel it is meant to secure.
func newRealityIdentity(dest string) (*realityIdentity, error) {
	if dest == "" {
		dest = defaultRealityDest
	}
	uuid, err := randomUUID()
	if err != nil {
		return nil, err
	}
	// Eight bytes is REALITY's maximum short id. Shorter ones are legal and
	// carry less entropy for no gain, since the id is never visible on the wire.
	shortID, err := randomHexString(8)
	if err != nil {
		return nil, err
	}
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		return nil, fmt.Errorf("read random: %w", err)
	}
	// Clamp the scalar before it is ever encoded. Go's curve25519 clamps
	// internally, so the public key below comes out the same either way and
	// the two halves look perfectly consistent from here — but the private
	// half is what gets installed on the VPS, and REALITY's server side uses
	// it as given. Ship an unclamped scalar and the server derives a shared
	// secret the client never agreed to: every handshake is rejected, and
	// rejection in REALITY means silently relaying the client to the borrowed
	// site. The client then reports the borrowed site's certificate failing
	// its pin, which reads as a wrong ?sni= rather than a bad key, and the
	// server says only "processed invalid connection". This cost an evening.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("derive reality public key: %w", err)
	}
	sshPub, err := localSSHPubKey()
	if err != nil {
		return nil, err
	}
	return &realityIdentity{
		UUID:       uuid,
		PrivateKey: base64.RawURLEncoding.EncodeToString(priv),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
		ShortID:    shortID,
		Dest:       dest,
		SSHPubKey:  sshPub,
	}, nil
}

// randomUUID returns a RFC 4122 version-4 UUID. Hand-rolled to keep the
// dependency list where it is — mole ships with cobra and x/crypto and nothing
// else, and a UUID is sixteen random bytes with six of them masked.
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
