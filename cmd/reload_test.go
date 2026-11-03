package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/utils"
)

func TestMalformedReloadKeepsLastGoodRuleSets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(utils.MoleDir(), 0755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(utils.CustomRulesPath(), []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"domain_suffix":["test.example"],"outbound":"direct"}]`)
	if _, err := RecompileHotRules(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.HotRuleSetDir(), "mole-direct.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(`[{`)
	if _, err := RecompileHotRules(); err == nil {
		t.Fatal("malformed input accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("last good rules changed", err)
	}
	write(`[{"domain_suffix":["fixed.example"],"outbound":"direct"}]`)
	if _, err := RecompileHotRules(); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Contains(after, []byte("fixed.example")) {
		t.Fatal("corrected rules not applied")
	}
}
