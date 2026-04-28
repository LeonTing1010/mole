package utils

import (
	"fmt"
	"io"
	"net/http"
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

// EnsureRuleSets pre-downloads any missing .srs file into ~/.mole/ so that
// sing-box can start without depending on the hy2 outbound for its own boot
// resources. The previous design routed rule-set fetches through the
// "proxy" outbound, which made startup a chicken-and-egg deadlock: when the
// VPS was down sing-box exited FATAL on rule-set init, supervisor never got a
// chance to flip into block-mode, and the user just saw an opaque restart
// loop in mole.log instead of "VPS unreachable".
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
	client := &http.Client{Timeout: 30 * time.Second}
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
