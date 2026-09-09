package core

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TCP Brutal on the REALITY leg — the client half. The node side, and the
// measurements behind it, are in cmd/brutal.go.

// BrutalKeeperPort is where the node's mole-brutal-keeper listens, on the
// node's own loopback — and, mirrored, where this client listens for the
// forwarder that reaches it. Reachable from here only THROUGH the tunnel: a
// connection to 127.0.0.1:8098 here is carried by the REALITY leg and dialled
// by the node's sing-box to itself, which is what makes an unauthenticated
// endpoint safe. Nothing outside the authenticated leg can name it.
const BrutalKeeperPort = 8098

// BrutalInboundTag names the fixed forwarder that carries this. It is bound to
// the REALITY leg by a route rule rather than by the proxy selector, so the
// rate travels over the leg it describes even while traffic is on the hy2
// ladder.
const BrutalInboundTag = "brutal-in"

// BrutalRateURL is the request that sets the rate the node paces its TCP
// toward this client at. Fetched whenever the resting rung changes, so
// `mole ceiling` governs both legs.
func BrutalRateURL(mbps int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/rate?mbps=%d", BrutalKeeperPort, mbps)
}

// BrutalBulkURL is a throughput sample served by the node itself: N bytes of
// incompressible data, so the figure is the VPS→client segment with no origin
// and no rate limit in the way. For hand-run A/Bs through the tunnel:
//
//	curl -o /dev/null -w '%{speed_download}\n' 'http://127.0.0.1:8098/bulk?bytes=8000000'
func BrutalBulkURL(bytes int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/bulk?bytes=%d", BrutalKeeperPort, bytes)
}

// BrutalResult is what the keeper says it did. Applied is the number of peer
// addresses it actually installed a pacing rule for; zero means the rate was
// received and NOT applied, which is a different failure from not being heard
// at all and is the one worth naming in a log.
type BrutalResult struct {
	Rate    int
	Applied int
	Peers   int
	Detail  string
}

// PushBrutalRate sets the node's pacing rate and reads back what it did.
//
// Reading the reply is the whole point. Until 2026-09-09 this went out as a
// Clash delay probe against the same URL, on the assumption that the probe
// fetches what it is handed. It does not: sing-box 1.13's /proxies/{n}/delay
// ignores the `url` query entirely and times its own default target, so it
// answers with a plausible delay for a URL it never dialled — proven by
// pointing it at a listener on the LAN, which recorded no connection at all
// while the API returned {"delay":180}. So the rate never left this machine,
// `brutal: node paces TCP toward this client at ↓100 Mbps` was true of
// nothing, and REALITY ran unpaced: measured the same minute, alternating,
// 0.15–0.48 Mbps against the hy2 ladder's 38–65.
//
// The lesson generalises past this one call: a success signal that cannot
// distinguish "it worked" from "nothing happened" is not a success signal.
func PushBrutalRate(mbps, timeoutMs int) (BrutalResult, error) {
	return brutalGET(BrutalRateURL(mbps), timeoutMs)
}

// BrutalHealth reports what the keeper currently has installed, without
// changing it.
func BrutalHealth(timeoutMs int) (BrutalResult, error) {
	return brutalGET(fmt.Sprintf("http://127.0.0.1:%d/health", BrutalKeeperPort), timeoutMs)
}

func brutalGET(url string, timeoutMs int) (BrutalResult, error) {
	var res BrutalResult
	client := &http.Client{
		Timeout: time.Duration(timeoutMs) * time.Millisecond,
		// One connection per push, closed after. The forwarder is a tunnel to
		// the far side of a leg that may be the very thing failing; a pooled
		// socket held open across pushes would report a stale success.
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Get(url)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	res = parseBrutalReply(string(body))
	if resp.StatusCode >= 300 {
		if res.Detail == "" {
			res.Detail = strings.TrimSpace(string(body))
		}
		return res, fmt.Errorf("keeper %s: %s", resp.Status, res.Detail)
	}
	return res, nil
}

// parseBrutalReply reads the keeper's key=value lines. Unknown keys are
// ignored so the node can grow a field without this end needing to agree.
func parseBrutalReply(body string) BrutalResult {
	var res BrutalResult
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "rate":
			res.Rate, _ = strconv.Atoi(v)
		case "applied":
			res.Applied, _ = strconv.Atoi(v)
		case "peers":
			res.Peers, _ = strconv.Atoi(v)
		case "detail":
			res.Detail = v
		}
	}
	return res
}

// legGateOpen says whether the fresh-dial verdict should be taken to the
// comparative leg test at all.
//
// It used to require the out-of-tunnel UDP probe alive as well, and that was
// the hole item 44 predicted: when UDP is what the path is treating, the
// probe is silent for exactly as long as the outage, and the gate never
// opened — 2026-09-06 22:15–22:19, 13 of 16 new connections dead on hy2,
// probe silent, no move, while REALITY dialled in 0.33s. With a second leg
// the test itself is the proof: an idle leg that dials clean says the host
// and the route are up, which a lost datagram cannot refute. Without one
// there is nothing to test and the probe is the only word on the route.
func legGateOpen(slow, probeAlive, hasReality bool) bool {
	return slow && (probeAlive || hasReality)
}
