package utils

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ruleSetSource describes one of the .srs rule-sets sing-box needs at startup.
type ruleSetSource struct {
	Tag string
	URL string
}

// ruleSetSources is the canonical list of rule-sets needed at startup.
// geosite rules have been replaced with domain-based rules.
var ruleSetSources = []ruleSetSource{
	{"geoip-cn", "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"},
}

// builtinRulesURL is the canonical curated direct-domain list (same schema as
// custom-rules.json). Pulled at startup and cached locally so the list of
// "domestic services that should route direct" can update WITHOUT recompiling
// the binary. Mirrors the rule-set prefetch pattern below.
const builtinRulesURL = "https://raw.githubusercontent.com/LeonTing1010/mole/master/config/builtin-rules.json"

// builtinRulesMaxAge is how stale the cached copy may be before we re-fetch.
// Keeps the list fresh for frequently-changing entries while staying
// offline-safe and avoiding a network hit on every startup.
const builtinRulesMaxAge = 7 * 24 * time.Hour

// EnsureRuleSets pre-downloads any missing .srs file into ~/.mole/ so that
// sing-box can start without depending on the hy2 outbound for its own boot
// resources. The previous design routed rule-set fetches through the
// "proxy" outbound, which made startup a chicken-and-egg deadlock: when the
// VPS was down sing-box exited FATAL on rule-set init and the supervisor was
// stuck restarting it in a loop, so the user just saw an opaque restart loop
// in mole.log instead of sing-box coming up cleanly.
//
// Best-effort: a failed download prints a warning but does not block startup.
// In that case the config builder falls back to remote-with-direct-detour so
// sing-box still tries (and at worst fails with a clearer error than the
// proxy-deadlock we have today).
func EnsureRuleSets() {
	for _, s := range ruleSetSources {
		path := filepath.Join(MoleDir(), s.Tag+".srs")
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			continue
		}
		if err := downloadRuleSet(s.URL, path); err != nil {
			fmt.Printf("⚠️  rule-set %s prefetch failed: %v (sing-box will retry direct at startup)\n", s.Tag, err)
		}
	}
}

func downloadRuleSet(url, dst string) error {
	// Direct fetch — explicitly do NOT route through the proxy. The whole
	// point is to remove the proxy from the cold-start critical path.
	// Use direct HTTP client to avoid DNS takeover issues if called while
	// the VPN is running.
	client := newDirectHTTPClient(30 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// EnsureBuiltinRules pre-downloads the curated direct-domain list into
// ~/.mole/builtin-rules.json, mirroring EnsureRuleSets. The list of "domestic
// services that should route direct" (oray, qq.com, …) changes over time, so
// pulling it lets the list update WITHOUT recompiling the binary.
//
// Fetched only when the cache is missing or older than builtinRulesMaxAge, so a
// dead network never blocks startup and we don't hit the source on every
// `mole up`. Best-effort: a failed download prints a warning but does not block
// startup — loadBuiltinRules() falls back to the embedded copy.
func EnsureBuiltinRules() {
	path := filepath.Join(MoleDir(), "builtin-rules.json")
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < builtinRulesMaxAge {
		return
	}
	if err := downloadRuleSet(builtinRulesURL, path); err != nil {
		fmt.Printf("⚠️  builtin-rules prefetch failed: %v (using embedded copy)\n", err)
	}
}
