package cmd

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Safety / what: unknown creation outcomes must not trigger blind retries.
func TestStartupVerificationFailureStopsCreation(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)
	_, err := createStartupScript("audit", "new identity")
	if err == nil || posts != 1 || strings.Contains(err.Error(), "verified no duplicate") {
		t.Fatalf("unknown outcome must stop: posts=%d err=%v", posts, err)
	}
}

// Safety / audit: the provider's script listing is paginated.
func TestStartupLookupTraversesPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "next" {
			fmt.Fprint(w, `{"startup_scripts":[{"id":"recent","name":"wanted"}]}`)
		} else {
			fmt.Fprint(w, `{"startup_scripts":[],"meta":{"links":{"next":"/v2/startup-scripts?cursor=next"}}}`)
		}
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)
	id, found, err := findStartupScriptByName("wanted")
	if err != nil || !found || id != "recent" {
		t.Fatalf("second-page resource lost: id=%s found=%v err=%v", id, found, err)
	}
}

// Safety / what: an old script with the same name carries a different identity.
func TestStartupNameMatchCannotAdoptDifferentIdentity(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			posts++
			w.WriteHeader(502)
		case r.URL.Path == "/startup-scripts/old":
			fmt.Fprintf(w, `{"startup_script":{"script":%q}}`, base64.StdEncoding.EncodeToString([]byte("old identity")))
		default:
			fmt.Fprint(w, `{"startup_scripts":[{"id":"old","name":"audit"}]}`)
		}
	}))
	defer srv.Close()
	patchScriptAPICall(t, srv)
	id, err := createStartupScript("audit", "new identity")
	if err == nil || id != "" || posts != 1 {
		t.Fatalf("reused old identity: id=%s posts=%d err=%v", id, posts, err)
	}
}
