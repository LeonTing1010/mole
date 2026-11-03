package cmd

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// patchScriptAPICall replaces scriptAPICall with one that uses a plain
// http.Client against the given server, restoring the original on cleanup.
// serverAPIWithin's bound-physical-interface client cannot reach a loopback
// httptest server, so every one of these tests patches around it.
func patchScriptAPICall(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := scriptAPICall
	scriptAPICall = func(budget time.Duration, method, path string, body any) ([]byte, error) {
		return callAPI(&http.Client{Timeout: budget}, srv.URL, "Bearer test-key", method, path, body)
	}
	t.Cleanup(func() { scriptAPICall = orig })
}

// TestCreateStartupScriptRetriesAfter5xx verifies the verify-before-retry
// path: a POST that gets 502, followed by a GET that confirms the script was
// actually created, returns the existing id instead of erroring out.
func TestCreateStartupScriptRetriesAfter5xx(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		switch r.Method + " " + r.URL.Path {
		case "POST /startup-scripts":
			if n == 1 {
				// First POST: Vultr gateway 502, but the backend actually
				// processed it.
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`We are currently conducting some software upgrades.`))
				return
			}
			t.Errorf("unexpected POST round %d — GET should have found the script", n)
			w.WriteHeader(http.StatusBadRequest)

		case "GET /startup-scripts/scr-abc123":
			fmt.Fprintf(w, `{"startup_script":{"script":%q}}`, base64.StdEncoding.EncodeToString([]byte("#!/bin/bash\necho hi")))
		case "GET /startup-scripts":
			// The verify call that follows the 502: script exists.
			_, _ = w.Write([]byte(`{"startup_scripts":[{"id":"scr-abc123","name":"test-node"}]}`))

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	id, err := createStartupScript("test-node", "#!/bin/bash\necho hi")
	if err != nil {
		t.Fatalf("expected verify-before-retry to recover, got %v", err)
	}
	if id != "scr-abc123" {
		t.Errorf("got script id %q, want scr-abc123", id)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("expected at least 2 hits (1 POST + 1 GET), got %d", got)
	}
}

// TestCreateStartupScriptRetriesWhenVerifySaysMissing verifies that a 5xx
// followed by a GET that shows nothing still retries the POST.
func TestCreateStartupScriptRetriesWhenVerifySaysMissing(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		switch r.Method + " " + r.URL.Path {
		case "POST /startup-scripts":
			if n == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			// Round 2 POST succeeds.
			_, _ = w.Write([]byte(`{"startup_script":{"id":"scr-def456"}}`))

		case "GET /startup-scripts":
			// Nothing there yet.
			_, _ = w.Write([]byte(`{"startup_scripts":[]}`))

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	id, err := createStartupScript("test-node", "#!/bin/bash\necho hi")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if id != "scr-def456" {
		t.Errorf("got script id %q, want scr-def456", id)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("expected 3 hits (POST + GET + POST), got %d", got)
	}
}

// TestCreateStartupScriptDoesNotRetry4xx verifies that a 4xx (Vultr actually
// received and rejected the request) is NOT retried — it's a final answer.
func TestCreateStartupScriptDoesNotRetry4xx(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"script too long"}`))
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	_, err := createStartupScript("test-node", "#!/bin/bash\necho hi")
	if err == nil {
		t.Fatal("expected 4xx to return an error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("4xx must not retry, got %d hits", got)
	}
}

// TestCreateStartupScriptSuccess verifies the happy path still works unchanged.
func TestCreateStartupScriptSuccess(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("unexpected %s — success path should not call GET", r.Method)
		}
		_, _ = w.Write([]byte(`{"startup_script":{"id":"scr-success"}}`))
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	id, err := createStartupScript("test-node", "#!/bin/bash\necho hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "scr-success" {
		t.Errorf("got %q, want scr-success", id)
	}
}

// TestCreateStartupScriptGivesUpAfterAllRounds verifies that after maxRounds
// of 5xx + "nothing found" cycles, we return an error and stop retrying.
func TestCreateStartupScriptGivesUpAfterAllRounds(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		switch r.Method {
		case "POST":
			w.WriteHeader(http.StatusBadGateway)
		case "GET":
			_, _ = w.Write([]byte(`{"startup_scripts":[]}`))
		}
		_ = n
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	_, err := createStartupScript("test-node", "#!/bin/bash\necho hi")
	if err == nil {
		t.Fatal("expected error after exhausting all rounds")
	}
	// Each round is one POST + one GET, so 3 rounds = 6 hits.
	if got := atomic.LoadInt32(&hits); got != 6 {
		t.Errorf("expected 6 hits (3 rounds × (POST + GET)), got %d", got)
	}
}

// findStartupScriptByName tests.
func TestFindStartupScriptByNameFinds(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("expected per_page=100, got %q", r.URL.Query().Get("per_page"))
		}
		_, _ = w.Write([]byte(`{"startup_scripts":[{"id":"scr-a","name":"other"},{"id":"scr-target","name":"mine"}]}`))
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	id, found, err := findStartupScriptByName("mine")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected to find 'mine'")
	}
	if id != "scr-target" {
		t.Errorf("got %q, want scr-target", id)
	}
}

func TestFindStartupScriptByNameMissing(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"startup_scripts":[{"id":"scr-a","name":"other"}]}`))
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)

	id, found, err := findStartupScriptByName("nobody")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("should not have found anything")
	}
	if id != "" {
		t.Errorf("got unexpected id %q", id)
	}
}
