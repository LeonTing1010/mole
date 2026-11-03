package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Quality/what-x-what: dual allocation and single-mode rollback affect new flows.
func TestBalanceAllocationCanUseBothAndReturnToThePrimarySelector(t *testing.T) {
	// Why: manual single-leg controls must still override dual routing.
	current := map[string]string{}
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		current[BalanceLaneTag(i)] = ProxySelectorTag
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": current[tag]})
			return
		}
		var request struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		current[tag] = request.Name
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	c := NewClashClient(api.Listener.Addr().String())
	for _, share := range []int{4, 0, 8, -1} {
		if _, err := ApplyBalancedLanes(c, share); err != nil {
			t.Fatal(err)
		}
		hy2 := 0
		for i := 0; i < BalanceLanes; i++ {
			now, err := c.Selection(BalanceLaneTag(i))
			if err != nil {
				t.Fatal(err)
			}
			if now == Hy2ProbeSelectorTag {
				hy2++
			}
			if share == -1 && now != ProxySelectorTag {
				t.Fatal("manual mode still bypasses the main selector")
			}
		}
		if share >= 0 && hy2 != share {
			t.Fatalf("share=%d routed %d UDP lanes", share, hy2)
		}
	}
}
