package core

import (
	"fmt"
	"github.com/LeonTing1010/mole/utils"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Quality/what-x-what. Why: failed downloads must never increase the declared rate.
func TestBulkReportsOnlyCompleteSuccessfulPayloads(t *testing.T) {
	for _, mode := range []string{"status", "short", "truncated", "valid"} {
		t.Run(mode, func(t *testing.T) {
			const size = 8192
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(120 * time.Millisecond)
				if mode == "status" {
					w.WriteHeader(503)
					fmt.Fprint(w, "unavailable")
					return
				}
				n := size
				if mode == "short" {
					n = 3
				}
				if mode == "truncated" {
					w.Header().Set("Content-Length", fmt.Sprint(size))
					n = 3
				}
				w.Write(make([]byte, n))
			}))
			defer s.Close()
			rate, err := BulkConcurrent(s.URL, 2, size)
			if mode == "valid" {
				if err != nil || rate <= 0 {
					t.Fatalf("rate=%v err=%v", rate, err)
				}
			} else if err == nil {
				t.Fatalf("failed payload reported %.1f Mbps", rate)
			}
		})
	}
}

// Safety/what. Why: stale state must never kill an unrelated process after PID reuse.
func TestOrphanCleanupDoesNotSignalReusedPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := exec.Command("sleep", "30")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { p.Process.Kill(); p.Wait() }()
	if err := utils.WriteState(&utils.State{SingboxPID: p.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	KillOrphanedSingboxes(utils.SingboxConfigPath())
	// Signal 0 can succeed for an unreaped exited child; ps exposes the zombie.
	out, _ := exec.Command("ps", "-p", fmt.Sprint(p.Process.Pid), "-o", "stat=").Output()
	if len(out) == 0 || out[0] == 'Z' {
		t.Fatal("cleanup terminated an unrelated process")
	}
}

// Safety/what. Why: an explicitly configured ceiling is a hard capacity bound.
func TestGovernorHonorsConfiguredCeilingAtOrBelowSeed(t *testing.T) {
	for _, peak := range []int{1, 40, 100, 200} {
		s := &Supervisor{schedule: BandwidthSchedule{PeakDown: peak}}
		g := NewRateGovernor(100, 3, s.governorCeiling(100))
		g.Observe(0.01)
		if g.Rate() > peak {
			t.Errorf("initial declaration %d exceeds ceiling %d", g.Rate(), peak)
		}
		if got := s.governorCeiling(100); got != peak {
			t.Errorf("peak=%d ceiling=%d", peak, got)
		}
	}
}

// Safety/what. Why: a backup config with the same prefix belongs to a different instance.
func TestProcessScanDoesNotConfuseConfigPathPrefixes(t *testing.T) {
	dir := t.TempDir()
	ps := filepath.Join(dir, "ps")
	script := "#!/bin/sh\nprintf '%s\\n' '12345 1 /usr/bin/sing-box run -c /tmp/mole-config.json.bak' '12346 1 /usr/bin/sing-box run -c /tmp/mole-config.json'\n"
	if err := os.WriteFile(ps, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got := scanSingboxByPS("/tmp/mole-config.json", true)
	if len(got) != 1 || got[0] != 12346 {
		t.Fatalf("wrong config selected: %v", got)
	}
}
