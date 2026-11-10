package core

import (
	"bufio"
	"errors"
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

// BrutalRatePort is a second forwarder to the same keeper, carried by the
// REALITY leg but deliberately WITHOUT multiplex.
//
// The rate push has to happen before the client's mux pool exists, because TCP
// Brutal classifies new sockets only: a session opened while the node still had
// no rule stays on the kernel default (bbr) for its whole life. With the push on
// the multiplexed forwarder, its own dial could become the pool's first session
// and pin the entire tunnel to bbr. A dedicated non-mux channel keeps that dial
// out of the pool, so the pool's sockets are all created after the rule exists.
const BrutalRatePort = 8097

// BrutalRateInboundTag names that forwarder.
const BrutalRateInboundTag = "brutal-rate-in"

// RealityRateLegTag is the REALITY outbound used only by the rate channel. Same
// server and credentials as RealityLegTag, no multiplex.
const RealityRateLegTag = "proxy-reality-rate"

// LegGatePort is the mirror of BrutalKeeperPort for the OTHER leg. It reaches
// the same keeper on the same node — the only difference is the route rule
// that carries it, which is pinned to the hy2 ladder instead of REALITY.
//
// It exists so the failover gate can measure both legs with ONE instrument.
// Until 2026-09-12 it could not: the REALITY arm was the observed fresh-dial
// record of real user connections, while the hy2 arm was a single
// clash.TestDelay — a handshake against a URL sing-box 1.13 does not even
// honour (see PushBrutalRate). Same 1000ms threshold, two different exams, and
// the easy one was hy2's. The gate fired fourteen times on the evening of
// 2026-09-12, each time moving traffic onto the ladder because it "dialled
// clean" in 345-752ms; measured the same minute with one instrument, REALITY
// answered in 0.64-0.95s against hy2's 2.3-7.2s and won every pairing.
//
// A probe that flatters one side of a comparison is worse than no probe: it
// does not fail to choose, it chooses wrongly and logs a reason.
const LegGatePort = 8099

// LegGateInboundTag names that forwarder.
const LegGateInboundTag = "gate-hy2-in"

// LegProbe opens ONE fresh connection to the node's keeper through whichever
// leg the given local port is routed over, and reports how long the whole
// exchange took. Connection reuse is off (see brutalGET), so every call pays
// the handshake — which is the thing the fresh-dial arm is counting, and
// therefore the thing the other leg has to be judged on too.
func LegProbe(port, timeoutMs int) (time.Duration, error) {
	start := time.Now()
	res, err := brutalGET(fmt.Sprintf("http://127.0.0.1:%d/reachable", port), timeoutMs)
	var status *keeperHTTPError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		// Older nodes have only /health. Keep their stricter pacing check.
		_, err = brutalGET(fmt.Sprintf("http://127.0.0.1:%d/health", port), timeoutMs)
	} else if err == nil && !res.Reachable {
		err = fmt.Errorf("keeper did not confirm transport delivery")
	}
	return time.Since(start), err
}

// BrutalRateURL is the request that sets the rate the node paces its TCP
// toward this client at. Fetched whenever the resting rung changes, so
// `mole ceiling` governs both legs. It uses BrutalRatePort, not the
// multiplexed forwarder, for the cold-start ordering described there.
func BrutalRateURL(mbps int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/rate?mbps=%d", BrutalRatePort, mbps)
}

// BrutalResult is what the keeper says it did. Applied is the number of peer
// addresses it actually installed a pacing rule for; zero means the rate was
// received and NOT applied, which is a different failure from not being heard
// at all and is the one worth naming in a log.
type BrutalResult struct {
	Reachable bool
	Rate      int
	Applied   int
	Peers     int
	Detail    string
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
	return pushBrutalRate(mbps, 0, timeoutMs)
}

// PushBrutalRateWithGain pushes rate AND cwnd_gain together via a single keeper call.
// gain=0 skips the gain parameter (keeper uses its built-in default).
func PushBrutalRateWithGain(mbps, gain, timeoutMs int) (BrutalResult, error) {
	return pushBrutalRate(mbps, gain, timeoutMs)
}

// pushBrutalRate is the internal implementation. gain=0 → no &gain= query param.
func pushBrutalRate(mbps, gain, timeoutMs int) (BrutalResult, error) {
	return brutalGET(brutalRateURL(mbps, gain), timeoutMs)
}

// PushBrutalRateVia is PushBrutalRate through an arbitrary local forwarder.
// The startup pre-flight uses it because the production forwarder does not
// exist yet: its whole job is to install the rule before the first engine
// session is dialled.
func PushBrutalRateVia(port, mbps, timeoutMs int) (BrutalResult, error) {
	return PushBrutalRateViaWithGain(port, mbps, 0, timeoutMs)
}

// PushBrutalRateViaWithGain is PushBrutalRateVia with an optional cwnd_gain.
func PushBrutalRateViaWithGain(port, mbps, gain, timeoutMs int) (BrutalResult, error) {
	return brutalGET(brutalRateURLAt(port, mbps, gain), timeoutMs)
}

// brutalRateURL builds the /rate URL for the default keeper port.
func brutalRateURL(mbps, gain int) string {
	return brutalRateURLAt(BrutalKeeperPort, mbps, gain)
}

// BrutalRateURLAt names the keeper on a specific local forwarder port.
func BrutalRateURLAt(port, mbps int) string {
	return brutalRateURLAt(port, mbps, 0)
}

// brutalRateURLAt builds the /rate URL with optional gain. gain=0 omits the param.
func brutalRateURLAt(port, mbps, gain int) string {
	if gain > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d/rate?mbps=%d&gain=%d", port, mbps, gain)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/rate?mbps=%d", port, mbps)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<10)+1))
	if err != nil {
		return res, fmt.Errorf("keeper body incomplete: %w", err)
	}
	if len(body) > 8<<10 {
		return res, fmt.Errorf("keeper body exceeds 8 KiB")
	}
	res = parseBrutalReply(string(body))
	if resp.StatusCode >= 300 {
		if res.Detail == "" {
			res.Detail = strings.TrimSpace(string(body))
		}
		return res, &keeperHTTPError{Status: resp.StatusCode, Detail: res.Detail}
	}
	// All three fields belong to the keeper contract, including valid zeroes.
	// HTML, an empty body, or malformed numbers must not qualify a transport.
	fields := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || (key != "rate" && key != "applied" && key != "peers") {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return res, fmt.Errorf("keeper invalid %s", key)
		}
		fields[key] = true
	}
	if !fields["rate"] || !fields["applied"] || !fields["peers"] {
		return res, fmt.Errorf("keeper reply missing rate/applied/peers")
	}
	return res, nil
}

type keeperHTTPError struct {
	Status int
	Detail string
}

func (e *keeperHTTPError) Error() string {
	return fmt.Sprintf("keeper HTTP %d: %s", e.Status, e.Detail)
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
		case "reachable":
			res.Reachable = v == "1"
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
//
// `failing` is State.DialFails() past its threshold, i.e. new connections that
// carried NOTHING — never the merely-slow ones that State.DialBad also counts.
// A slow dial is worth reporting but not worth moving the leg for; see
// State.DialFails for the 2026-10-14 evening that turned on this distinction.
func legGateOpen(failing, probeAlive, hasReality bool) bool {
	return failing && (probeAlive || hasReality)
}
