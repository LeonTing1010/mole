package core

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Quality/what: measure an external request on the named leg, with fresh TLS.
func TestLegEgressUsesFixedForwarderAndRejectsEmptyBodies(t *testing.T) {
	// Why: fetching through the traffic selector would measure the wrong leg.
	for _, body := range []string{"fl=test\n", ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "probe.invalid" {
				t.Errorf("original destination lost: %s", r.Host)
			}
			fmt.Fprint(w, body)
		}))
		client := legEgressClient(srv.Listener.Addr().String(), time.Second)
		_, err := (&Supervisor{}).fetchThroughTunnel(client, "http://probe.invalid/cdn-cgi/trace")
		srv.Close()
		if (err == nil) != (body != "") {
			t.Fatalf("body=%q: err=%v", body, err)
		}
	}
}

// Safety/what: external probes must retain certificate verification.
func TestLegEgressVerifiesTLSAndReadsThroughTheLeg(t *testing.T) {
	// Why: an unverified or synthetic response cannot qualify a proxy path.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fl=test\n")
	}))
	defer srv.Close()
	client := legEgressClient(srv.Listener.Addr().String(), time.Second)
	if _, err := (&Supervisor{}).fetchThroughTunnel(client, srv.URL); err == nil {
		t.Fatal("untrusted external certificate accepted")
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	if _, err := (&Supervisor{}).fetchThroughTunnel(client, srv.URL); err != nil {
		t.Fatal(err)
	}
}
