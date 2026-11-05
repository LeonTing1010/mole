package utils

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Safety/what: never replace host DNS without a durable original value.
func TestDNSTakeoverRequiresReadableAndWritableBackup(t *testing.T) {
	for _, mode := range []string{"read-fails", "write-fails", "corrupt-existing"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup.json")
			changes := 0
			m := dnsSettings{path: path, list: func() ([]string, error) { return []string{"Wi-Fi"}, nil }, get: func(string) ([]string, error) {
				if mode == "read-fails" {
					return nil, errors.New("read failed")
				}
				return []string{"1.2.3.4"}, nil
			}, set: func(string, []string) error { changes++; return nil }}
			if mode == "write-fails" {
				m.path = filepath.Join(path, "missing", "backup.json")
			}
			if mode == "corrupt-existing" {
				os.WriteFile(path, []byte("broken"), 0600)
			}
			if err := m.takeOver(); err == nil || changes != 0 {
				t.Fatalf("err=%v changes=%d", err, changes)
			}
		})
	}
}

// Safety/what: partial recovery preserves only the services still needing retry.
func TestDNSRestoreRetainsFailedServicesAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	os.WriteFile(path, []byte(`{"Wi-Fi":["1.2.3.4"],"Ethernet":[]}`), 0600)
	fail := true
	calls := map[string]int{}
	m := dnsSettings{path: path, set: func(s string, v []string) error {
		calls[s]++
		if s == "Wi-Fi" && fail {
			return errors.New("unavailable")
		}
		if s == "Ethernet" && (len(v) != 1 || v[0] != "Empty") {
			t.Fatal(v)
		}
		return nil
	}}
	if err := m.restore(); err == nil {
		t.Fatal("partial restore hidden")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pending map[string][]string
	json.Unmarshal(data, &pending)
	if len(pending) != 1 || pending["Wi-Fi"][0] != "1.2.3.4" {
		t.Fatal(string(data))
	}
	fail = false
	if err := m.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if calls["Ethernet"] != 1 || calls["Wi-Fi"] != 2 {
		t.Fatal(calls)
	}
}

// Safety/what: preserve crash recovery values and add newly connected services.
func TestDNSTakeoverPreservesExistingBackupAndReportsSetFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	os.WriteFile(path, []byte(`{"Wi-Fi":["1.2.3.4"]}`), 0600)
	m := dnsSettings{path: path, list: func() ([]string, error) { return []string{"Wi-Fi", "Ethernet"}, nil }, get: func(s string) ([]string, error) { return []string{tunGatewayDNS, "9.9.9.9"}, nil }, set: func(string, []string) error { return errors.New("denied") }}
	if err := m.takeOver(); err == nil {
		t.Fatal("set failure hidden")
	}
	data, _ := os.ReadFile(path)
	var b map[string][]string
	json.Unmarshal(data, &b)
	if b["Wi-Fi"][0] != "1.2.3.4" || len(b["Ethernet"]) != 1 || b["Ethernet"][0] != "9.9.9.9" {
		t.Fatal(string(data))
	}
}

// Safety/what: a second cleanup cannot erase already restored custom DNS.
func TestDNSWithoutBackupOnlyRecoversTunResolvers(t *testing.T) {
	changed := map[string][]string{}
	m := dnsSettings{path: filepath.Join(t.TempDir(), "absent.json"), list: func() ([]string, error) { return []string{"Wi-Fi", "Ethernet"}, nil }, get: func(s string) ([]string, error) {
		if s == "Wi-Fi" {
			return []string{"1.2.3.4"}, nil
		}
		return []string{tunGatewayDNS}, nil
	}, set: func(s string, v []string) error { changed[s] = v; return nil }}
	if err := m.restore(); err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed["Ethernet"][0] != "Empty" {
		t.Fatal(changed)
	}
}
