package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

// Quality / what-x-what: application DNS results outlive an engine process.
// Why: a cached fake address must still name its domain after engine recovery.
func TestRecoveryPersistsFakeIPMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Build("hy2://test@192.0.2.1:443?insecure=1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Experimental struct {
			CacheFile struct {
				Enabled     bool   `json:"enabled"`
				Path        string `json:"path"`
				StoreFakeIP bool   `json:"store_fakeip"`
			} `json:"cache_file"`
		} `json:"experimental"`
	}
	if err := json.Unmarshal(data, &actual); err != nil {
		t.Fatal(err)
	}
	cache := actual.Experimental.CacheFile
	if !cache.Enabled || !cache.StoreFakeIP || cache.Path != filepath.Join(utils.MoleDir(), "sing-box-cache.db") {
		t.Fatalf("FakeIP recovery cache missing or unstable: %+v", cache)
	}
}

// Quality / what-x-what: restart recovery must not publish browsing metadata.
// Why: sing-box defaults to 0644; precreate or tighten the DB without truncation.
func TestRecoveryCacheIsPrivateWithoutLosingExistingMappings(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "cache.db")
			if existing {
				if err := os.WriteFile(path, []byte("persisted mapping sentinel"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &SingboxConfig{Experimental: &ExperimentalConfig{CacheFile: &CacheFileConfig{Enabled: true, Path: path, StoreFakeIP: true}}}
			if err := Save(cfg, filepath.Join(root, "config.json")); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0600 {
				t.Fatalf("DNS metadata permissions: %o; want 600", st.Mode().Perm())
			}
			if existing {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "persisted mapping sentinel" {
					t.Fatal("privacy preparation destroyed existing mappings")
				}
			}
		})
	}
}

func TestConfigOmitsRemovedIndependentCacheOption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Build("hy2://test@192.0.2.1:443?insecure=1")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	var doc map[string]json.RawMessage
	json.Unmarshal(data, &doc)
	var dns map[string]json.RawMessage
	json.Unmarshal(doc["dns"], &dns)
	if _, present := dns["independent_cache"]; present {
		t.Fatal("redundant legacy option blocks future schema migration")
	}
}
