package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineContractRejectsUnqualifiedVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{
		{"1.14.3", true}, {"1.14.4", true}, {"1.14.2", false}, {"1.15.0-alpha.13", false}, {"1.15.0", false}, {"dev", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			err := validateEngineVersion("sing-box version " + tc.version + "\nEnvironment: go1.26.10")
			if (err == nil) != tc.ok {
				t.Fatalf("version %s: %v; supported=%v", tc.version, err, tc.ok)
			}
		})
	}
}

func TestEngineContractChecksConfigurationAndBoundsOutput(t *testing.T) {
	for _, valid := range []bool{true, false} {
		dir := t.TempDir()
		engine := filepath.Join(dir, "sing-box")
		marker := filepath.Join(dir, "checked")
		code := "0"
		if !valid {
			code = "1"
		}
		script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo 'sing-box version 1.14.4'; exit 0; fi\nif [ \"$1\" = check ]; then touch '" + marker + "'; echo 'invalid field'; exit " + code + "; fi\nexit 99\n"
		if err := os.WriteFile(engine, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		err := checkEngineConfig(engine, filepath.Join(dir, "config.json"))
		if (err == nil) != valid {
			t.Fatalf("valid=%v error=%v", valid, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("configuration never checked")
		}
		if !valid && !strings.Contains(err.Error(), "invalid field") {
			t.Fatal("check failure lost diagnostic")
		}
	}
}
