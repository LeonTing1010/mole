package core

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Quality/what-x-what. Why: complete fast downloads must reach the governor;
// a 6 MiB sample takes under 100 ms above approximately 503 Mbps.
func TestBulkAcceptsCompleteSub100MillisecondSamples(t *testing.T) {
	const size = 64 << 10
	client := &http.Client{Transport: bulkSpeedTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", size)))}, nil
	})}
	rate, err := bulkConcurrentWithClient(client, "http://sample.invalid", 8, size)
	if err != nil || rate <= 0 {
		t.Fatalf("complete fast download discarded: rate=%v err=%v", rate, err)
	}
}

type bulkSpeedTransport func(*http.Request) (*http.Response, error)

func (f bulkSpeedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Safety/what. Why: accepting fast samples must not restore the old false-throughput bug.
func TestFastBulkStillRejectsFailedOrIncompletePayloads(t *testing.T) {
	const size = 1024
	for _, mode := range []string{"http-error", "short", "long", "read-error", "dial-error"} {
		t.Run(mode, func(t *testing.T) {
			client := &http.Client{Transport: bulkSpeedTransport(func(*http.Request) (*http.Response, error) {
				if mode == "dial-error" {
					return nil, fmt.Errorf("dial failed")
				}
				status, length := 200, size
				if mode == "http-error" {
					status = 503
				}
				if mode == "short" {
					length--
				}
				if mode == "long" {
					length++
				}
				var reader io.Reader = strings.NewReader(strings.Repeat("x", length))
				if mode == "read-error" {
					reader = bulkBrokenReader{}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(reader)}, nil
			})}
			if rate, err := bulkConcurrentWithClient(client, "http://sample.invalid", 8, size); err == nil || rate != 0 {
				t.Fatalf("%s qualified: rate=%v err=%v", mode, rate, err)
			}
		})
	}
}

type bulkBrokenReader struct{}

func (bulkBrokenReader) Read([]byte) (int, error) { return 0, fmt.Errorf("body interrupted") }
