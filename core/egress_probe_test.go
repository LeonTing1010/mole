package core

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Quality/what: external status and payload contracts survive multi-origin retries.
func TestPerformanceEgressValidatesStatusAndCompleteBody(t *testing.T) {
	for _, tc := range []struct {
		status, expected int
		body             string
		required, ok     bool
	}{
		{200, 200, "trace", true, true}, {200, 200, "", true, false}, {204, 204, "", false, true},
		{200, 204, "", false, false}, {204, 204, "unexpected", false, false},
	} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		err := validateEgressResponse(resp, LegEgressTarget{Status: tc.expected, RequireBody: tc.required})
		if (err == nil) != tc.ok {
			t.Fatalf("status=%d body=%q: %v", tc.status, tc.body, err)
		}
	}
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(brokenEgressReader{})}
	if err := validateEgressResponse(resp, LegEgressTarget{Status: 200, RequireBody: true}); err == nil {
		t.Fatal("incomplete external body qualified")
	}
}

type brokenEgressReader struct{}

func (brokenEgressReader) Read([]byte) (int, error) { return 0, fmt.Errorf("truncated response") }
