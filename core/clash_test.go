package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Selection is what lets the scheduler notice that `mole ceiling` moved the
// selector out from under it. Its whole value is being a fact about the tunnel,
// so a response it cannot read must be an error rather than an empty string
// that would silently read as "the selector points nowhere" and trigger a
// re-select every tick.
func TestSelectionReadsTheLiveMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies/proxy" {
			t.Errorf("asked for %s, want /proxies/proxy", r.URL.Path)
		}
		w.Write([]byte(`{"type":"Selector","now":"proxy-bw-30","all":["proxy-bw-12","proxy-bw-30"]}`))
	}))
	defer srv.Close()

	got, err := NewClashClient(srv.Listener.Addr().String()).Selection(ProxySelectorTag)
	if err != nil {
		t.Fatalf("Selection: %v", err)
	}
	if got != "proxy-bw-30" {
		t.Errorf("Selection = %q, want proxy-bw-30", got)
	}
}

func TestSelectionRejectsAnAnswerItCannotUse(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"no now field": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"type":"Selector"}`))
		},
		"not found": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`<html>`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			got, err := NewClashClient(srv.Listener.Addr().String()).Selection(ProxySelectorTag)
			if err == nil {
				t.Errorf("Selection returned %q with no error; the scheduler would treat that as the live member", got)
			}
		})
	}
}
