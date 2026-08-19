package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The whole point of the retry budget is that it fits inside the window a
// closing lid gives us. macOS waits roughly 30 seconds for power-management
// clients and then sleeps regardless — a call that can legally take 60, as this
// one used to, could not have finished even when the network was fine.
func TestAPIBudgetFitsTheSleepWindow(t *testing.T) {
	const sleepWindow = 30 * time.Second
	if apiBudget >= sleepWindow {
		t.Errorf("apiBudget %s does not fit macOS's ~%s sleep window", apiBudget, sleepWindow)
	}
	// And it must buy more than one attempt, or the budget is just a timeout
	// and a single EOF still loses the server.
	if apiBudget < 2*apiAttempt {
		t.Errorf("apiBudget %s affords fewer than two %s attempts", apiBudget, apiAttempt)
	}
}

// DELETE is retried because repeating it can only be answered the same way or
// more truthfully — a 404 on the second try is the honest "already gone".
// POST is not, because a transport error hides whether the server acted, and a
// retried create leaves a second instance billing with no local record of it.
func TestOnlyIdempotentMethodsRetry(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE"} {
		if !idempotent(m) {
			t.Errorf("%s should be retryable", m)
		}
	}
	for _, m := range []string{"POST", "PATCH"} {
		if idempotent(m) {
			t.Errorf("%s must never be retried — it could create a second billing instance", m)
		}
	}
}

// A transport failure must be retried rather than surfaced, which is exactly
// what the lost destroy needed: the first attempt died with EOF against a dead
// tunnel while the endpoint itself was answering fine.
func TestTransportFailureIsRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// Hijack and close without a response: an EOF to the client,
			// indistinguishable from the tunnel dropping the connection.
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	deadline := time.Now().Add(5 * time.Second)
	var err error
	var data []byte
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		data, err = callAPI(&http.Client{Timeout: time.Second}, srv.URL, "Bearer x", "DELETE", "/instances/abc", nil)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("retry never succeeded: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("unexpected body %q", data)
	}
	if hits < 2 {
		t.Fatalf("expected the first attempt to fail and a second to run, got %d hits", hits)
	}
}

// An HTTP status is the server's answer. Repeating the request cannot improve
// it, and burning the budget on a 401 would mean a real destroy — one that
// needs those seconds — never gets them.
func TestHTTPStatusIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	_, err := callAPI(&http.Client{Timeout: time.Second}, srv.URL, "Bearer x", "DELETE", "/instances/abc", nil)
	var se *ServerError
	if !asServerError(err, &se) {
		t.Fatalf("expected a *ServerError carrying the status, got %v", err)
	}
	if se.Status != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", se.Status)
	}
	if hits != 1 {
		t.Errorf("made %d requests, want exactly 1", hits)
	}
}

func asServerError(err error, target **ServerError) bool {
	se, ok := err.(*ServerError)
	if ok {
		*target = se
	}
	return ok
}
