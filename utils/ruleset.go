package utils

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ruleSetSource describes one of the .srs rule-sets sing-box needs at startup.
// MaxAge, when non-zero, re-fetches a cached copy once it goes stale.
type ruleSetSource struct {
	Tag    string
	URL    string
	MaxAge time.Duration
}

// ruleSetSources is the canonical list of rule-sets needed at startup.
//
// geosite-cn was removed once before ("keep only local geoip-cn rule-set to
// avoid download failures", 2e9a278) and is back deliberately. That removal
// traded a real capability for an operational worry, and the worry is now
// handled properly: the fetch is best-effort, the file is cached in ~/.mole, and
// — critically — the config builder only references a rule-set that is actually
// on disk (see geositeCNPath in config/parser.go). A failed download therefore
// degrades to the old suffix+allowlist behaviour instead of producing a config
// sing-box refuses to load, which is what made the dependency scary the first
// time.
//
// Why it is needed: geoip-cn can only classify traffic once a REAL IP is known,
// and the DNS layer hands out fake IPs to everything it doesn't already believe
// is Chinese. Domestic sites on non-.cn domains (baidu.com, bilibili.com,
// xiaohongshu.com, aliyun.com …) therefore fell through to fakeip and got routed
// abroad — measured 2026-07-21 at ~13× the TTFB of a direct fetch (0.99s vs
// 0.058s), plus they consumed the scarce cross-border tunnel. An allowlist of
// such domains cannot be maintained by hand; that is exactly what geosite-cn is.
//
// geoip-cn has no MaxAge: CN address allocations move slowly, and a stale copy
// misroutes nothing that matters. geosite-cn does, because a domain list is only
// as good as its last update — letting it rot is how "site X is suddenly slow"
// comes back as a recurring mystery, which is the very whack-a-mole this
// rule-set exists to end.
var ruleSetSources = []ruleSetSource{
	{Tag: "geoip-cn", URL: "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"},
	{Tag: "geosite-cn", URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs", MaxAge: 7 * 24 * time.Hour},
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

// EnsureRuleSets pre-downloads any MISSING .srs file into ~/.mole/ so that
// sing-box can start without depending on the hy2 outbound for its own boot
// resources. The previous design routed rule-set fetches through the
// "proxy" outbound, which made startup a chicken-and-egg deadlock: when the
// VPS was down sing-box exited FATAL on rule-set init and the supervisor was
// stuck restarting it in a loop, so the user just saw an opaque restart loop
// in mole.log instead of sing-box coming up cleanly.
//
// A cached-but-stale copy is deliberately NOT refreshed here. The cold-start
// path has no tunnel yet, so the fetch has to leave the physical interface —
// and on networks that block raw.githubusercontent.com directly (IPv4 SYN
// dropped, IPv6 reset after the TLS ClientHello) that fetch fails every single
// time. Warming the cache is best-effort and must not block startup; stale
// freshness is handled by RefreshStaleCaches once the tunnel is carrying
// traffic. Measured 2026-11-14: geosite-cn.srs sat 25 days past its 7-day
// MaxAge because every cold-start refresh was failing on this exact path.
func EnsureRuleSets() {
	for _, s := range ruleSetSources {
		path := filepath.Join(MoleDir(), s.Tag+".srs")
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			continue
		}
		if err := downloadRuleSet(s.URL, path); err != nil {
			WarnOncePerDay("ruleset-"+s.Tag, "⚠️  Rule set %s could not be downloaded (%s); sing-box will start without it.\n", s.Tag, briefFetchError(err))
		}
	}
}

// RefreshStaleCaches re-fetches every stale cache through client, which the
// caller must point at the live tunnel (a plain http.Client is enough: once
// the TUN owns the default route, it leaves that way). Returns how many files
// it refreshed so the caller knows whether anything changed.
//
// The new files take effect on the next `mole up` — sing-box has already read
// the previous copies — which is why this runs as a background nicety rather
// than a blocking step.
func RefreshStaleCaches(client *http.Client) int {
	refreshed := 0

	for _, s := range ruleSetSources {
		if s.MaxAge == 0 {
			continue
		}
		path := filepath.Join(MoleDir(), s.Tag+".srs")
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < s.MaxAge {
			continue
		}
		if err := downloadRuleSetWith(client, s.URL, path); err != nil {
			WarnOncePerDay("ruleset-"+s.Tag, "⚠️  Rule set %s is out of date and could not be refreshed (%s); still using the cached copy.\n", s.Tag, briefFetchError(err))
			continue
		}
		refreshed++
	}

	builtinPath := filepath.Join(MoleDir(), "builtin-rules.json")
	if st, err := os.Stat(builtinPath); err != nil || time.Since(st.ModTime()) >= builtinRulesMaxAge {
		if err := downloadRuleSetWith(client, builtinRulesURL, builtinPath); err != nil {
			WarnOncePerDay("builtin-rules", "⚠️  Built-in rules could not be refreshed (%s); using the embedded copy.\n", briefFetchError(err))
		} else {
			refreshed++
		}
	}

	if st, err := os.Stat(CNCIDRPath()); err != nil || time.Since(st.ModTime()) >= cnCIDRMaxAge {
		if err := fetchCNCIDRsWith(client, CNCIDRPath()); err != nil {
			WarnOncePerDay("cn-cidr", "⚠️  CN CIDR list could not be refreshed (%s); using the cached copy.\n", briefFetchError(err))
		} else {
			refreshed++
		}
	}

	if refreshed > 0 {
		fmt.Printf("🔄 Refreshed %d cached list(s) through the tunnel; they take effect on the next `mole up`.\n", refreshed)
	}
	return refreshed
}

// HasStaleCaches reports whether any cache is past its MaxAge, so the
// post-tunnel refresher can skip its wait entirely when nothing needs fetching.
func HasStaleCaches() bool {
	for _, s := range ruleSetSources {
		if s.MaxAge == 0 {
			continue
		}
		st, err := os.Stat(filepath.Join(MoleDir(), s.Tag+".srs"))
		if err != nil || time.Since(st.ModTime()) >= s.MaxAge {
			return true
		}
	}
	if st, err := os.Stat(filepath.Join(MoleDir(), "builtin-rules.json")); err != nil || time.Since(st.ModTime()) >= builtinRulesMaxAge {
		return true
	}
	if st, err := os.Stat(CNCIDRPath()); err != nil || time.Since(st.ModTime()) >= cnCIDRMaxAge {
		return true
	}
	return false
}

// briefFetchError reduces a Go network error to the one part a person can act
// on. The raw error repeats the URL that was already named and carries
// bracketed IPv6 addresses, which push the cause off the end of the line.
func briefFetchError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection reset by peer"):
		return "connection reset"
	case strings.Contains(s, "no such host"):
		return "DNS lookup failed"
	case strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return "timed out"
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "certificate"):
		return "TLS certificate rejected"
	}
	return s
}

func downloadRuleSet(url, dst string) error {
	// Direct fetch — explicitly do NOT route through the proxy. The whole
	// point is to remove the proxy from the cold-start critical path.
	// Use direct HTTP client to avoid DNS takeover issues if called while
	// the VPN is running.
	return downloadRuleSetWith(newDirectHTTPClient(30*time.Second), url, dst)
}

func downloadRuleSetWith(client *http.Client, url, dst string) error {
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
// Only a missing file is fetched here; staleness is refreshed after the tunnel
// is up (see RefreshStaleCaches) for the same reason as EnsureRuleSets. A
// failed download never blocks startup — loadBuiltinRules() falls back to the
// embedded copy.
func EnsureBuiltinRules() {
	path := filepath.Join(MoleDir(), "builtin-rules.json")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return
	}
	if err := downloadRuleSet(builtinRulesURL, path); err != nil {
		WarnOncePerDay("builtin-rules", "⚠️  Built-in rules could not be downloaded (%s); using the embedded copy.\n", briefFetchError(err))
	}
}
