package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
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
