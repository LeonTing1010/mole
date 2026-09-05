package core

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Supervisor keeps sing-box alive and probes VPS health for status reporting.
type Supervisor struct {
	configPath string
	// onRulesChanged 重新编译域名 rule-set，返回域名条数。由 cmd 层注入以避开
	// core→config 的循环依赖（config 已 import core）。nil = 关闭规则热更。
	onRulesChanged func() (int, error)
	probeAddr      string // "host:port" — direct UDP probe target
	obfsPassword   string // salamander key the probe must speak; "" when unobfuscated
	probeURL       string // URL driven through the proxy for keepalive (and fallback probe)
	probeEvery     time.Duration
	keepaliveEvery time.Duration
	probeTimeMs    int

	clash *ClashClient

	// Time-of-day Brutal ceiling. Inert unless a peak profile is configured.
	// When enabled, the config carries two hy2 outbounds (peak/off-peak) behind
	// a selector, and runBandwidthScheduler flips the selector via the Clash API
	// at each window boundary — no sing-box restart, so the TUN and existing
	// connections are never torn down.
	schedule BandwidthSchedule

	// Destroy-on-lid-close. Inert unless both are set; see runSleepDestroy.
	destroyOnLidClose bool
	onIdleDestroy     func(budget time.Duration) error
	// lidDryRun logs the lid close and destroys nothing, so the clamshell
	// detection can be proven on a given Mac without paying a redeploy.
	lidDryRun bool
	// lidOffReason is what to tell the operator when the watcher is not armed.
	lidOffReason string
	// lidClosedFn reads the clamshell. A field rather than a direct call to
	// lidClosed so the retry path is testable: the machine running the tests
	// has its lid open by definition, and the branch that matters most is the
	// one taken when it is shut. Defaults to the real reader.
	lidClosedFn func() int
	// Lid-destroy bookkeeping. It used to be reachable only from the
	// power-notification thread, which is single-threaded by construction (one
	// run loop, one callback at a time). The probe loop now settles debts too —
	// it is the only thing that ticks inside a dark wake — so the two can race,
	// and lidMu serializes them: one destroy in flight at a time, and one writer
	// for lastLidAttempt. The tick side takes it with TryLock, because a tick
	// that finds an attempt already running has nothing to add by waiting.
	lidMu            sync.Mutex
	lidDestroyed     chan struct{}
	lidDestroyedOnce sync.Once
	lastLidAttempt   time.Time

	stateMu sync.Mutex
	state   *utils.State
	// lastHealth (guarded by stateMu) is the previously observed synthesized
	// verdict, kept so flips get one durable log line — state.json is
	// overwritten in place, so the log is the only history of transitions.
	lastHealth utils.Health

	// The keepalive's HTTP client, built lazily and kept so its connections
	// survive between beats; see tunnelClient.
	tunnelClientOnce sync.Once
	tunnelHTTP       *http.Client

	// The fresh-dial arm's client: same request, but it never reuses a
	// connection; see dialClient.
	dialClientOnce sync.Once
	dialHTTP       *http.Client

	// Stall-kick bookkeeping: which rung a kick is parked on and until when.
	// Its own mutex because the keepalive goroutine sets it and the bandwidth
	// scheduler reads it every tick. Never taken while stateMu is held.
	kickMu    sync.Mutex
	kickUntil time.Time
	kickRung  int

	// Leg bookkeeping. hasReality records that the config carries a
	// RealityLegTag member at all — without one hy2 is the only leg, REALITY
	// cannot be the primary, and every failover check below short-circuits.
	// fallbackMu guards the policy value and, like kickMu, is never taken
	// while stateMu is held.
	hasReality  bool
	realityAddr string
	fallbackMu  sync.Mutex
	fallback    fallbackState

	// legTest rate-limits the comparative leg test in considerLegFailover.
	// The fresh-dial latch can stay set for an hour; without this the test
	// would re-dial REALITY every 15s for that entire hour to keep being told
	// the same thing. Its own mutex, taken alone, for the same reason as the
	// two above.
	legTestMu sync.Mutex
	legTestAt time.Time

	// linkPoke carries "the local address changed, re-dial now" from the link
	// watcher to the keepalive. Buffered so the watcher never blocks and a
	// second change while one is queued collapses into the same re-dial.
	linkPoke chan struct{}

	stopCh chan struct{}
	doneCh chan struct{}
}

// SupervisorOpts controls the supervisor's probe cadence. Zero values get sane defaults.
type SupervisorOpts struct {
	ClashAddr string
	// OnRulesChanged 在 custom-rules.json 变更时重新编译域名 rule-set，返回域名条数。
	// 由 cmd 层注入（core 不能 import config，会成循环依赖）。nil = 关闭规则热更。
	OnRulesChanged func() (int, error)
	ProbeAddr      string // VPS hy2 endpoint "ip:port" for direct UDP probe (recommended)
	ProbeURL       string // URL driven through the proxy for keepalive; also the fallback probe when ProbeAddr is empty
	// ObfsPassword is the server's salamander key. The direct probe must speak
	// the same obfuscation the server does, or every probe decodes to noise on
	// arrival and is dropped unanswered — health would report a dark path
	// through a perfectly healthy tunnel. Empty for unobfuscated servers.
	ObfsPassword   string
	ProbeEvery     time.Duration
	KeepaliveEvery time.Duration // in-tunnel keepalive cadence; 0 → default
	ProbeTimeMs    int

	// HasRealityLeg says the config carries the VLESS+REALITY member, which
	// makes it the primary leg and arms the automatic fallback onto the hy2
	// ladder. Derived from the same URI the config was built from, so the flag
	// and the selector member it names can never disagree — selecting a
	// RealityLegTag outbound sing-box never heard of would fail the call and
	// strand traffic on a leg already proven dark.
	HasRealityLeg bool
	// RealityAddr is the REALITY leg's ip:port, for the out-of-tunnel TCP probe
	// that tells a node refusing the port from a path dropping it. Empty when
	// there is no such leg.
	RealityAddr string

	// Schedule enables the time-of-day Brutal ceiling. Gated by Schedule.Enabled()
	// (a peak profile must be configured); the config must also carry the
	// peak/off-peak selector that config.Build emits for the same server. Leave
	// it zero to keep a plain fixed-bandwidth server.
	Schedule BandwidthSchedule

	// DestroyOnLidClose destroys the server when the lid is shut — the sleep
	// notification, gated on the clamshell actually being closed, so idle and
	// maintenance sleep do not count. macOS only. See runSleepDestroy for why
	// this is the only trigger rather than one of several.
	DestroyOnLidClose bool
	// OnIdleDestroy performs the destroy within budget. Required when
	// DestroyOnLidClose is set.
	//
	// The budget is passed rather than fixed by the implementation because the
	// two callers have incomparable windows: a lid close is a held-open ~30s,
	// a retry on a dark wake is 2 seconds of borrowed time. See lidCloseBudget
	// and retryBudget.
	OnIdleDestroy func(budget time.Duration) error
	// LidCloseDryRun arms the watcher but destroys nothing: a lid close is
	// logged and the daemon keeps running. The clamshell test in
	// sleepwatch_darwin.go reads properties whose placement varies by model, so
	// "it compiles" is not evidence it fires on this Mac. This is how to get
	// that evidence without deleting the server to find out.
	LidCloseDryRun bool
	// LidDestroyOffReason is why DestroyOnLidClose is false, in the operator's
	// terms ("no Vultr key", a flag they passed). Recorded in state.json and
	// logged at startup so a disarmed watcher is discoverable later instead of
	// only at the moment it fails to fire.
	LidDestroyOffReason string
}

// NewSupervisor builds a supervisor ready to start. It does not start sing-box.
func NewSupervisor(configPath, serverName string, opts SupervisorOpts) *Supervisor {
	if opts.ClashAddr == "" {
		opts.ClashAddr = "127.0.0.1:9090"
	}
	if opts.ProbeURL == "" {
		opts.ProbeURL = "https://www.gstatic.com/generate_204"
	}
	if opts.ProbeEvery == 0 {
		opts.ProbeEvery = 10 * time.Second
	}
	if opts.KeepaliveEvery == 0 {
		// Must beat the shortest thing that kills an idle UDP/QUIC flow — a home
		// router's UDP NAT mapping (commonly ~30s) or the QUIC idle timeout. 15s
		// gives two beats per 30s window, so one missed/failed beat still keeps
		// the mapping alive. The payload is a single ~200-byte fetch, so the cost is
		// negligible.
		opts.KeepaliveEvery = 15 * time.Second
	}
	if opts.ProbeTimeMs == 0 {
		opts.ProbeTimeMs = 5000
	}
	return &Supervisor{
		configPath:        configPath,
		probeAddr:         opts.ProbeAddr,
		obfsPassword:      opts.ObfsPassword,
		probeURL:          opts.ProbeURL,
		probeEvery:        opts.ProbeEvery,
		keepaliveEvery:    opts.KeepaliveEvery,
		probeTimeMs:       opts.ProbeTimeMs,
		schedule:          opts.Schedule,
		hasReality:        opts.HasRealityLeg,
		realityAddr:       opts.RealityAddr,
		destroyOnLidClose: opts.DestroyOnLidClose,
		onIdleDestroy:     opts.OnIdleDestroy,
		onRulesChanged:    opts.OnRulesChanged,
		lidDryRun:         opts.LidCloseDryRun,
		lidOffReason:      opts.LidDestroyOffReason,
		lidClosedFn:       lidClosed,
		lidDestroyed:      make(chan struct{}),
		linkPoke:          make(chan struct{}, 1),
		clash:             NewClashClient(opts.ClashAddr),
		state: &utils.State{
			Mode:      utils.ModeProxy,
			PID:       os.Getpid(),
			Server:    serverName,
			StartedAt: time.Now(),
		},
		lastHealth: utils.HealthOK,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Run blocks, starting sing-box and the probe loop until Stop() is called.
// Returns when both goroutines have exited.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := Start(s.configPath); err != nil {
		return fmt.Errorf("initial sing-box start: %w", err)
	}
	defer Stop()

	// The scheduler runs whenever the config carries a ladder — not just when a
	// peak window is configured. Without a peak profile it has no clock work to
	// do, but it is still the thing that applies a manual `mole ceiling` pin.
	scheduleOn := len(s.schedule.Rungs()) >= 2
	// Without a destroy hook the watcher could only log that it would have
	// deleted something, so treat a missing hook as the feature being off rather
	// than running a loop whose conclusion goes nowhere.
	sleepOn := s.destroyOnLidClose && s.onIdleDestroy != nil

	// Carry forward what the last daemon failed to finish. A destroy that fired
	// and never completed leaves a server running that nobody is watching.
	//
	// Read from the debt file, not from state.json: `mole down` deletes
	// state.json, so the version of this that read it printed nothing on the
	// path people actually take. On 2026-08-14 a destroy owed since 03:40
	// survived the night and was erased by the `mole down && mole up` at 10:02,
	// after which the machine that had been billing for six hours was mentioned
	// by nothing at all.
	if debt := utils.ReadLidDebt(); debt != nil {
		who := debt.Server
		if who == "" {
			who = "the server"
		}
		log.Printf("destroy-on-lid-close: %s has been billing since %s — the destroy owed then is %s after %d attempts (last: %s). `mole down --destroy` settles it",
			who, debt.Since.Format("2006-01-02 15:04"), debt.State, debt.Attempts, debt.Reason)
	}

	// Say which way this went before anything else can fail. Both branches are
	// recorded — in state.json for `mole status` and in the log for later — so
	// the answer to "was it armed last night?" is a file read rather than an
	// inference from the absence of output.
	s.recordLidDestroy(sleepOn)
	_ = utils.WriteState(s.snapshot())
	if !sleepOn {
		reason := s.lidOffReason
		if reason == "" {
			reason = "not enabled"
		}
		log.Printf("destroy-on-lid-close: OFF (%s) — the server bills until destroyed, shutting the lid will not stop it", reason)
	}

	var wg sync.WaitGroup
	// +1 = 规则热更监听（见 ruleswatch.go）：改 custom-rules.json 即时生效，
	// 无需任何命令、不重启进程、不断 TUN。
	// +1 = 本机上行地址监听（见 runLinkWatch）：macOS 自己重连 Wi-Fi 换 IP 后
	// 立刻重拨，不等 keepalive 的下一拍。
	n := 5
	if scheduleOn {
		n++
	}
	if sleepOn {
		n++
	}
	// +1 = the paired A/B sampler, only while an experiment is declared. Its
	// absence costs nothing and its presence costs two delay probes per beat.
	experimentOn := utils.ReadExperiment() != nil
	if experimentOn {
		n++
	}
	wg.Add(n)

	// Goroutine A: keep sing-box alive.
	go func() {
		defer wg.Done()
		defer recoverPanic("singbox-lifecycle")
		s.runLifecycle(ctx)
	}()

	// Goroutine A2: watch custom-rules.json and recompile the domain rule-sets
	// in place. sing-box watches those files itself, so a domain change lands
	// without a reconnect — no TUN teardown, no kill-switch gap, no dropped
	// connections. Scope and limits: config/hotrules.go.
	go func() {
		defer wg.Done()
		defer recoverPanic("rules-watch")
		s.watchRules(ctx)
	}()

	// Goroutine B: probe VPS health for status reporting.
	go func() {
		defer wg.Done()
		defer recoverPanic("health-probe")
		s.runProbe(ctx)
	}()

	// Goroutine C: keep the hy2/QUIC tunnel warm with low-frequency in-tunnel
	// traffic so it doesn't idle out (the bug where the connection went dead
	// until `mole status` accidentally revived it by fetching the exit IP).
	go func() {
		defer wg.Done()
		defer recoverPanic("keepalive")
		s.runKeepalive(ctx)
	}()

	// Goroutine C2: watch this machine's uplink address. macOS re-associates
	// Wi-Fi on its own; every session bound to the old address dies with it, and
	// waiting for the next keepalive beat to notice turned a few seconds of link
	// loss into 30–80s of dark tunnel.
	go func() {
		defer wg.Done()
		defer recoverPanic("link-watch")
		s.runLinkWatch(ctx)
	}()

	// Goroutine D (optional): drive the Brutal ceiling — follow the local clock
	// between peak/off-peak profiles so a single fixed rate can't flood the path
	// at peak or throttle it off-peak, unless a manual pin overrides it.
	if scheduleOn {
		go func() {
			defer wg.Done()
			defer recoverPanic("bandwidth-scheduler")
			s.runBandwidthScheduler(ctx)
		}()
	}

	// Goroutine D2 (optional): the paired A/B sampler. See runExperiment.
	if experimentOn {
		go func() {
			defer wg.Done()
			defer recoverPanic("experiment")
			s.runExperiment(ctx)
		}()
	}

	// Goroutine E (optional): destroy the server on the way into sleep, so a
	// laptop that has been shut for the night is not billing for a tunnel
	// nobody can be using.
	if sleepOn {
		go func() {
			defer wg.Done()
			defer recoverPanic("sleep-destroy")
			s.runSleepDestroy(ctx)
		}()
	}

	wg.Wait()
	close(s.doneCh)
	return nil
}

// Stop signals both goroutines to wind down and stops sing-box.
func (s *Supervisor) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	_ = Stop()
}

// Done returns a channel closed when Run has fully returned.
func (s *Supervisor) Done() <-chan struct{} { return s.doneCh }

func (s *Supervisor) runLifecycle(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for {
		startedAt := time.Now()
		exit := ExitChan()
		if exit == nil {
			exit = closedChan()
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-exit:
			runFor := time.Since(startedAt)
			if runFor > 30*time.Second {
				backoff = time.Second
			}
			log.Printf("sing-box exited after %s (err=%v); restarting in %s", runFor, ExitError(), backoff)
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			if err := Start(s.configPath); err != nil {
				log.Printf("restart failed: %v", err)
				continue
			}
			_ = utils.WriteState(s.snapshot())
			log.Printf("sing-box restarted")
		}
	}
}

func (s *Supervisor) runProbe(ctx context.Context) {
	// Wait for the Clash API to come up (sing-box takes a moment).
	if err := s.waitClashReady(ctx, 10*time.Second); err != nil {
		log.Printf("clash api not ready: %v (probe will keep trying)", err)
	}

	// Small initial jitter to avoid all clients firing on the same second.
	jitter := time.Duration(rand.Int63n(int64(s.probeEvery)))
	select {
	case <-ctx.Done():
		return
	case <-s.stopCh:
		return
	case <-time.After(jitter):
	}

	t := time.NewTicker(s.probeEvery)
	defer t.Stop()
	for {
		// Before the probe, not after: when both have something to do the
		// machine is in a dark wake with seconds to live, and deleting the
		// server is worth more than one more health sample. Returns immediately
		// unless a destroy is owed, which is every tick of a normal day.
		s.reconcileLidDebt("tick retry")
		s.probeOnce()
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
	}
}

func (s *Supervisor) probeOnce() {
	var (
		delay   int
		verdict utils.ProbeVerdict
		err     error
	)
	if s.probeAddr != "" {
		// Direct (interface-bound, outside-the-tunnel) probe to the VPS hy2
		// endpoint; see ProbeHy2UDP for why both properties matter.
		rtt, v, perr := ProbeHy2UDP(s.probeAddr, s.obfsPassword, time.Duration(s.probeTimeMs)*time.Millisecond)
		delay = int(rtt / time.Millisecond)
		verdict = v
		err = perr
	} else {
		delay, err = s.clash.TestDelay("proxy", s.probeURL, s.probeTimeMs)
		if err != nil {
			verdict = utils.ProbeError
		} else {
			verdict = utils.ProbeAlive
		}
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.state.LastProbeAt = time.Now()
	s.state.LastProbeVerdict = verdict
	if err != nil {
		s.state.ConsecutiveFails++
		s.state.ConsecutiveOK = 0
		s.state.LastLatencyMs = 0
		s.state.LastProbeError = err.Error()
		log.Printf("direct probe %s (×%d): %v", verdict, s.state.ConsecutiveFails, err)
	} else {
		s.state.ConsecutiveOK++
		s.state.ConsecutiveFails = 0
		s.state.LastLatencyMs = delay
		s.state.LastProbeError = ""
	}
	s.noteHealthLocked()

	_ = utils.WriteState(s.stateSnapshotLocked())
}

// Self-healing cadence, in scheduler ticks (one tick = one minute).
//
// Asymmetric on purpose. Descending is cheap and reversible, so two bad minutes
// are enough to act on; climbing back re-exposes the link to the ceiling that
// was just failing, so it waits for a long clean stretch. healMaxBackoff bounds
// the descent — past a few rungs the ceiling is no longer what is wrong, and the
// ladder's own bottom rung is the real floor.
//
// "Bad" here means unhealthy *and flooding*, never unhealthy alone; see
// healState.tick.
const (
	healBackoffTicks = 2
	healRecoverTicks = 15
	healMaxBackoff   = 3
)

// healState folds a stream of per-tick health observations into the number of
// rungs the ceiling should sit below its clock/pin target.
//
// Kept as a value with no dependencies so the policy can be exercised directly:
// the failure it guards against takes tens of minutes to reproduce against a
// real link, which is exactly the kind of logic that otherwise only ever gets
// tested in production.
type healState struct {
	backoff, bad, good int
	// target is the ceiling the descent was accumulated against, so a change of
	// intent can be told apart from a change of health.
	target int
	// held records that the last tick was an unhealthy one with no flood
	// evidence, so the hold is logged once per dark stretch instead of once a
	// minute for as long as it lasts.
	held bool
}

// retarget restarts the descent when the ceiling target changes under it.
//
// The backoff is evidence about one particular ceiling: "30 Mbps is currently
// too much for this path". The moment someone runs `mole ceiling 12` that
// evidence is about a setting nobody is asking for any more, and carrying it
// over lands the tunnel some rungs below what they typed — while `mole ceiling`
// prints the number they asked for and `mole status` prints a third one. An
// explicit pin is a fresh statement of intent, so the descent starts over and
// re-earns itself from the probes if the path really is still bad.
func (h *healState) retarget(pin int) {
	if pin == h.target {
		return
	}
	if h.backoff > 0 {
		log.Printf("self-heal: ceiling target changed (%d → %d) — clearing backoff %d", h.target, pin, h.backoff)
	}
	*h = healState{target: pin}
}

// freeze folds in a tick that carries no evidence about the tunnel, and returns
// the unchanged backoff.
//
// The case is a local uplink that is gone (utils.HealthNoLink): no probe can
// leave the machine and no keepalive can reach the far end, so "unhealthy" would
// be a statement about Wi-Fi wearing the costume of a statement about the path.
// Ticking it as unhealthy did real damage — it cleared the clean streak, so a
// 60-second DHCP re-association cost fifteen more minutes before the ceiling
// could climb one rung back.
//
// The streak is preserved rather than advanced: the link was not proven good
// either.
func (h *healState) freeze() int {
	h.held = false
	return h.backoff
}

// tick folds in one observation and returns the backoff to apply now.
//
// flooding is what makes the descent honest. "Unhealthy" alone does not mean
// the ceiling is too high — it is equally the signature of a path that has gone
// dark, and no rung on the ladder passes traffic through a dark path. Descending
// there buys nothing and costs half the bandwidth for at least healRecoverTicks
// minutes afterwards. Only the flood evidence (see floodMarker) says the ceiling
// itself is the thing that is wrong, so only the flood evidence moves it down.
//
// A dark tick still clears the clean streak — the path is not well, so the
// ceiling must not climb either. It just holds where it is and waits, which for
// a blackhole is the only thing that has ever worked.
func (h *healState) tick(healthy, flooding bool) int {
	// Flooding is decisive on its own, and is checked BEFORE health. ENOBUFS
	// means this machine could not push the declared rate into the link — a
	// statement about the local send path that is equally true whether or not
	// the tunnel happens to be answering right now.
	//
	// Gating it on "unhealthy" was a hole. The keepalive recovers between
	// failures, so health spends most of a bad stretch reading OK, and the
	// healthy branch resets h.bad to zero: on 2026-08-22 a link whose Wi-Fi rate
	// had collapsed to 14-47 Mbps against a 65 Mbps ceiling produced 45 ENOBUFS
	// in 45 minutes and not one descent, because no two consecutive ticks ever
	// found the tunnel unhealthy at the same moment they found it flooding.
	//
	// This is not a reversal of the rule that flooding is REQUIRED to descend
	// (see the doc comment above); it is dropping the second, unrelated
	// condition that was silently ANDed onto it.
	if flooding {
		h.held = false
		h.good = 0
		h.bad++
		// Stop descending at healMaxBackoff. Below that the ceiling is no longer
		// the problem and dropping further only throttles a recovery we still
		// want to be ready for.
		if h.bad >= healBackoffTicks && h.backoff < healMaxBackoff {
			h.backoff, h.bad = h.backoff+1, 0
			log.Printf("self-heal: sing-box flooding (ENOBUFS) %d min — lowering the ceiling one rung (backoff %d)", healBackoffTicks, h.backoff)
		}
		return h.backoff
	}
	if healthy {
		h.held = false
		h.bad, h.good = 0, h.good+1
		if h.backoff > 0 && h.good >= healRecoverTicks {
			h.backoff, h.good = h.backoff-1, 0
			log.Printf("self-heal: %d min clean — raising the ceiling one rung (backoff %d)", healRecoverTicks, h.backoff)
		}
		return h.backoff
	}
	h.good = 0
	if !h.held {
		h.held = true
		log.Printf("self-heal: path unhealthy but sing-box is not flooding (no ENOBUFS) — holding the ceiling; this is a dark path, not a ceiling that is too high")
	}
	return h.backoff
}

// noteHealthLocked logs one line whenever the synthesized health verdict
// flips. Callers must hold stateMu. This log (in ~/.mole/mole.log, which
// rotates) is the only durable record of when the connection went dark and
// when it recovered — state.json can't provide that, it's overwritten in
// place on every probe.
func (s *Supervisor) noteHealthLocked() {
	h := s.state.Health()
	if h == s.lastHealth {
		return
	}
	log.Printf("health: %s → %s (probe %s fails=%d err=%q; keepalive fails=%d err=%q; fresh dials %d/%d bad)",
		s.lastHealth, h,
		s.state.LastProbeVerdict, s.state.ConsecutiveFails, s.state.LastProbeError,
		s.state.KeepaliveFails, s.state.LastKeepaliveError,
		s.state.DialBad(), len(s.state.DialSamples))
	s.lastHealth = h
}

// runKeepalive drives low-frequency traffic through the proxy outbound to keep
// the hy2/QUIC session and its UDP NAT mapping warm. This is the actual fix for
// the "connection dies until I run mole status" symptom: status only appeared
// to heal things because GetMyIPInfo sent a request through the tunnel. Now the
// supervisor does that on a timer, and — because a request through a dead hy2
// outbound makes sing-box re-dial — it both prevents idle death and re-
// establishes the session promptly if it dies anyway (e.g. after sleep/wake).
func (s *Supervisor) runKeepalive(ctx context.Context) {
	// Keepalive rides the Clash API (it drives the proxy outbound), so wait for
	// the API the same way the probe does.
	if err := s.waitClashReady(ctx, 10*time.Second); err != nil {
		log.Printf("clash api not ready for keepalive: %v (will keep trying)", err)
	}

	// Offset from the health probe so the two loops don't fire in lockstep.
	jitter := time.Duration(rand.Int63n(int64(s.keepaliveEvery)))
	select {
	case <-ctx.Done():
		return
	case <-s.stopCh:
		return
	case <-time.After(jitter):
	}

	t := time.NewTicker(s.keepaliveEvery)
	defer t.Stop()
	for {
		s.keepaliveOnce()
		s.dialOnce()
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-s.linkPoke:
			// The local address just changed. Every established session is bound
			// to an address that no longer exists, so beat now instead of waiting
			// out the rest of the tick: the request is what makes sing-box notice
			// and re-dial.
		case <-t.C:
		}
	}
}

// runLinkWatch notices when this machine's physical uplink address changes and
// pokes the keepalive to re-dial immediately.
//
// Why it exists: macOS releases and re-acquires the DHCP lease on its own —
// when symptomsd decides an interface has stalled it re-associates Wi-Fi, which
// on 2026-08-22 happened five times. Each one left every hy2 session bound to a
// source address that no longer existed, which sing-box reports as
// `write: can't assign requested address` and quic-go answers by destroying the
// connection. Recovery then waited on the next keepalive beat to trigger a
// re-dial, and the outages ran 30–80 seconds when the link itself was back in a
// few.
//
// Polling rather than a route-socket subscription: this needs to be dependable
// and boring across macOS releases, and a 2s poll of an in-kernel table costs
// nothing measurable.
func (s *Supervisor) runLinkWatch(ctx context.Context) {
	last, _ := currentUplink()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
		now, _ := currentUplink()
		if now == last {
			continue
		}
		switch {
		case now == "":
			log.Printf("uplink lost (was %s) — the tunnel cannot recover until it returns; this is local, not the path", last)
		case last == "":
			log.Printf("uplink back (%s) — re-dialing the tunnel", now)
		default:
			log.Printf("uplink changed (%s → %s) — sessions are bound to the old address; re-dialing", last, now)
		}
		last = now
		if now != "" {
			select {
			case s.linkPoke <- struct{}{}:
			default: // a poke is already queued; one re-dial covers both
			}
		}
	}
}

// currentUplink identifies the physical interface and IPv4 address traffic
// currently leaves from, as a comparable string. Empty means there is none —
// which is a state, not an error.
func currentUplink() (string, error) {
	ifi, err := utils.DefaultPhysicalInterface()
	if err != nil {
		return "", err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil && !v4.IsLoopback() && !v4.IsLinkLocalUnicast() {
			return ifi.Name + "/" + v4.String(), nil
		}
	}
	return "", fmt.Errorf("%s has no usable IPv4", ifi.Name)
}

// keepaliveURL is what the in-tunnel keepalive fetches. Two properties matter
// and neither is the latency:
//
//   - it returns a BODY. The old keepalive drove a Clash delay probe at a
//     generate_204, which completes with an empty response — so it proved a
//     request could be issued and answered, and proved nothing about whether the
//     tunnel moves bytes. A session that connects but carries nothing looked
//     perfectly healthy through that check.
//   - it is small (~200 bytes) and stable, so doing it every 15s costs nothing.
//
// As a bonus for anyone debugging by hand: the body names the exit IP, so
// `curl https://cloudflare.com/cdn-cgi/trace` is also the one-liner that says
// whether traffic is leaving through the VPS at all.
const keepaliveURL = "https://cloudflare.com/cdn-cgi/trace"

// Stall-kick thresholds. See requestStallKick.
const (
	// stallKickThreshold: consecutive keepalive failures before concluding the
	// established session is the problem. At the 15s cadence, 2 ≈ 30s — early
	// enough to act before KeepaliveFailThreshold declares the path dead.
	stallKickThreshold = 2
	// stallKickHold: how long to stay off the original rung. Must exceed
	// hysteria's MaxIdleTimeout (30s), or returning would reuse the very session
	// we are trying to shed instead of dialing a fresh one.
	stallKickHold = 45 * time.Second
)

// keepaliveOnce drives one real request through the tunnel and records the
// result. A single failure stays informational — this path runs through
// sing-box's DNS engine, so one hiccup must not flip the verdict. But a streak
// of failures is the signature users actually feel, so consecutive failures feed
// State.Health via KeepaliveFails. The vps-down verdict remains the probe's
// exclusive call.
//
// The request follows the selector rather than naming an outbound, which is the
// point: it measures the path traffic is really taking, and — because a request
// routed to a dead hy2 outbound makes sing-box re-dial — it both prevents idle
// death and re-establishes a session that died anyway (e.g. after sleep/wake).
func (s *Supervisor) keepaliveOnce() {
	start := time.Now()
	n, err := s.fetchThroughTunnel(s.tunnelClient(), keepaliveURL)
	delay := int(time.Since(start) / time.Millisecond)

	action := legHold
	s.stateMu.Lock()
	s.state.LastKeepaliveAt = time.Now()
	if err != nil {
		s.state.KeepaliveFails++
		if s.state.KeepaliveFails == 1 {
			s.state.KeepaliveFailingSince = time.Now()
		}
		s.state.LastKeepaliveMs = 0
		s.state.LastKeepaliveError = err.Error()
		log.Printf("tunnel keepalive failed (×%d): %v", s.state.KeepaliveFails, err)
		// Every move below needs the out-of-tunnel datagram still answered:
		// that says the far end and the route are fine and it is the way
		// traffic is CARRIED that has stopped working. The kick is for a stuck
		// hy2 session (rotate the five-tuple, 45s); the fallback is for a
		// primary that stayed dark past that; abandoning the fallback is for
		// an hy2 that turned out dark too. See keepaliveLegAction.
		action = keepaliveLegAction(s.state.KeepaliveFails,
			s.state.LastProbeVerdict == utils.ProbeAlive, s.activeOnHy2(), s.onFallback(), utils.ReadLeg())
	} else {
		s.state.KeepaliveFails = 0
		s.state.KeepaliveFailingSince = time.Time{}
		s.state.LastKeepaliveMs = delay
		s.state.LastKeepaliveError = ""
		_ = n
	}
	s.noteHealthLocked()
	_ = utils.WriteState(s.stateSnapshotLocked())
	s.stateMu.Unlock()

	switch action {
	case legFallback:
		s.enterFallback(fmt.Sprintf("keepalive dark ×%d on %s while the UDP probe still answers", fallbackEnterFails, RealityLegTag))
	case legAbandonFallback:
		s.releaseFallback(fmt.Sprintf("keepalive dark ×%d on the hy2 fallback as well — back to %s", fallbackEnterFails, RealityLegTag))
	case legKick:
		s.requestStallKick()
	}
}

// activeOnHy2 reports whether traffic is on the hysteria2 ladder right now:
// pinned there, or there because the primary failed, or there because the node
// has no REALITY leg at all.
func (s *Supervisor) activeOnHy2() bool {
	switch utils.ReadLeg() {
	case utils.LegHy2:
		return true
	case utils.LegReality:
		return false
	}
	return !s.hasReality || s.onFallback()
}

// ladderTarget is the ladder rung that SHOULD carry traffic if hy2 is the leg:
// the ceiling pin when it names a real rung, otherwise the clock's profile. The
// self-heal backoff and a running stall kick are deliberately left out — those
// are refinements the scheduler applies on its own tick; this is the resting
// point the failover pre-warms and the comparative test dials.
func (s *Supervisor) ladderTarget() string {
	if pin := utils.ReadCeiling(); pin > 0 && s.schedule.HasRung(pin) {
		return BandwidthRungTag(pin)
	}
	return s.schedule.Member(time.Now())
}

// dialOnce drives the fresh-dial arm: one GET over a connection that has never
// been used, recorded into the rolling window Health() latches on.
//
// It runs after the warm arm rather than alongside it, on purpose. Two requests
// racing through the same tunnel contend for the same session and would each
// measure a little of the other; sequential beats cost a few hundred
// milliseconds more and keep the two numbers independent, which is the only
// reason to have two of them.
//
// It drives exactly one thing: the leg failover, and only through the
// comparative gate in considerLegFailover. Not the stall kick (that rotates a
// five-tuple, and the five-tuple is not what is broken here), and not the
// ceiling (measured 2026-09-01: the tunnel was near-idle at 0.0-0.1 Mbps with
// zero ENOBUFS through the whole episode, so there is no flood to back off —
// and an A/B during the episode had the LOWER rung failing 25% of new
// connections against 4% on the higher one, making a descent actively worse).
func (s *Supervisor) dialOnce() {
	start := time.Now()
	_, err := s.fetchThroughTunnel(s.dialClient(), keepaliveURL)
	ms := int(time.Since(start) / time.Millisecond)

	s.stateMu.Lock()
	s.state.RecordDial(ms, err)
	bad, window := s.state.DialBad(), len(s.state.DialSamples)
	// Logged per bad beat, not per streak: the complaint this whole arm answers
	// is that the log was completely silent through hours of it. Bounded by
	// construction — at the 15s cadence this is at most four lines a minute,
	// and only while new connections are actually failing.
	if err != nil {
		log.Printf("fresh dial failed (%d/%d recent new connections bad): %v", bad, window, err)
	} else if ms > utils.DialSlowMs {
		log.Printf("fresh dial slow: %.1fs (%d/%d recent new connections bad)", float64(ms)/1000, bad, window)
	}
	// Read under the same lock that just updated them, act after releasing it:
	// considerLegFailover dials the network and takes fallbackMu, and stateMu is
	// never held across either.
	slow := s.state.SlowNewFlows
	probeAlive := s.state.LastProbeVerdict == utils.ProbeAlive
	s.noteHealthLocked()
	_ = utils.WriteState(s.stateSnapshotLocked())
	s.stateMu.Unlock()

	if slow && probeAlive {
		s.considerLegFailover()
	}
}

// considerLegFailover decides whether the fresh-dial verdict is evidence about
// the active TRANSPORT — the one thing switching legs can fix — or about
// something both legs share.
//
// The distinction is the whole reason this is not a straight `if slow then
// switch`. Both of the following look identical to the fresh-dial arm:
//
//   - 2026-09-01, 14:46-18:00: 12-16 of every 20 new connections slow or dead,
//     while an A/B in the same minute put REALITY at 0/20 failures and hy2 at
//     3/20 on the same server over the same route. UDP was being targeted.
//     Switching legs was the entire fix.
//   - 2026-09-01, 18:30: new connections slow again — because the Wi-Fi PHY
//     rate was flapping 51<->97 Mbps against a co-channel neighbour stronger
//     than us. Both legs measured the same: hy2 27-38 Mbps, REALITY 21.9-24.6.
//     Switching would have bought nothing.
//
// So the gate is a measurement, not a heuristic: dial the idle leg and look. A
// leg that dials cleanly while this one does not is the first case; a leg that
// dials just as badly is the second, and the log says so by name, because
// "your Wi-Fi is the problem" is the single most useful thing mole can say on
// an evening when it has nothing to fix.
//
// With REALITY primary the idle leg is the hy2 ladder rung that should be in
// force, and the switch it argues for is onto UDP. The caller has already
// established the two preconditions: the fresh-dial latch is set, and the
// out-of-tunnel probe is still being answered (so the VPS and the route are
// up, and this is about how traffic is carried rather than whether the far end
// is there).
func (s *Supervisor) considerLegFailover() {
	s.legTestMu.Lock()
	if !legTestDue(s.hasReality, s.onFallback(), utils.ReadLeg(), time.Now(), s.legTestAt) {
		s.legTestMu.Unlock()
		return
	}
	s.legTestAt = time.Now()
	s.legTestMu.Unlock()

	bad, window := s.dialWindow()
	idle := s.ladderTarget()
	ms, err := s.legDialsClean(idle)
	if !legTestSaysTransport(ms, err, utils.DialSlowMs) {
		if err != nil {
			log.Printf("new connections failing (%d/%d) but %s will not dial either (%v) — both legs are down, so this is the path or the local link, not the transport; holding %s",
				bad, window, idle, err, RealityLegTag)
		} else {
			log.Printf("new connections failing (%d/%d) but %s is no better (%dms) — both legs are slow, so the bottleneck is what they share (local link or route), not the transport; holding %s",
				bad, window, idle, ms, RealityLegTag)
		}
		return
	}
	s.enterFallback(fmt.Sprintf("%d of the last %d new connections were slow or failed on %s while %s dialled clean (%dms), which is a difference only the transport explains",
		bad, window, RealityLegTag, idle, ms))
}

// Comparative gate: how many probes the idle leg has to pass, and how far
// apart. One was not enough — 2026-09-04 09:10, a single 431ms probe declared
// REALITY clean in the middle of a Wi-Fi stall and traffic was moved onto a
// leg that then timed out 124 times in the hour. Three spaced by four seconds
// span the ~10s lulls a bad link has between bursts.
const (
	legGateProbes  = 3
	legGateSpacing = 4 * time.Second
)

// legDialsClean dials the idle leg legGateProbes times and reports the WORST
// result: the slowest answered probe, or the first failure. A leg is only
// clean if all of them were.
func (s *Supervisor) legDialsClean(tag string) (worstMs int, err error) {
	for i := 0; i < legGateProbes; i++ {
		if i > 0 {
			select {
			case <-s.stopCh:
				return worstMs, fmt.Errorf("stopping")
			case <-time.After(legGateSpacing):
			}
		}
		ms, perr := s.clash.TestDelay(tag, s.probeURL, s.probeTimeMs)
		if perr != nil {
			return ms, perr
		}
		if ms > worstMs {
			worstMs = ms
		}
	}
	return worstMs, nil
}

// dialWindow reports the fresh-dial verdict's shape for log lines.
func (s *Supervisor) dialWindow() (bad, window int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state.DialBad(), len(s.state.DialSamples)
}

// fetchThroughTunnel performs one GET on the given client and returns how many
// body bytes arrived. An empty body is an error: carrying zero bytes is the
// failure this exists to catch. The client is a parameter because the two
// keepalive arms differ in exactly one thing — whether the connection is reused
// — and nothing else about the request may drift between them, or the two
// numbers stop being comparable.
func (s *Supervisor) fetchThroughTunnel(c *http.Client, url string) (int64, error) {
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return n, fmt.Errorf("read body after %d bytes: %w", n, err)
	}
	if resp.StatusCode != http.StatusOK {
		return n, fmt.Errorf("http %s", resp.Status)
	}
	if n == 0 {
		return 0, fmt.Errorf("empty body: tunnel connected but carried nothing")
	}
	return n, nil
}

// tunnelClient is the HTTP client the warm keepalive arm uses, built once and
// reused so its connections persist between beats.
//
// The reuse is deliberate: holding the connection means this arm rides the same
// kind of long-lived flow as the user's actual traffic, which is the only way
// to see an established session being throttled to death.
//
// What it cannot see is the mirror image, and the comment here used to claim
// otherwise — that a fresh connection "would test only what the direct probe
// already tests". That is wrong, and it cost a day of blind flying. The direct
// probe opens a new UDP five-tuple to the VPS; a fresh dial opens a new proxied
// TCP connection through the tunnel to a foreign host. On 2026-09-01 the probe
// was alive 291 times running and this client reported a flat 120ms, while 15%
// of new connections took 1–20s. Both numbers were true and neither was the
// answer. See dialClient for the arm that was missing.
func (s *Supervisor) tunnelClient() *http.Client {
	s.tunnelClientOnce.Do(func() {
		s.tunnelHTTP = &http.Client{
			Timeout: time.Duration(s.probeTimeMs) * time.Millisecond,
		}
	})
	return s.tunnelHTTP
}

// dialClient is the fresh-dial arm's client: the same request to the same URL,
// with connection reuse switched off so every beat pays for a real TCP and TLS
// handshake through the tunnel. HTTP/2 stays off (a hand-built Transport does
// not enable it) and no TLS session cache is set, so nothing is resumed — which
// makes this beat cost exactly what the user's browser pays when it opens a
// site it has not visited in a while.
//
// That is the whole measurement. On 2026-09-01 the difference between reusing
// and not reusing the connection was the difference between 0.107s with
// near-zero variance and 1–20s with timeouts, on the same tunnel in the same
// second.
func (s *Supervisor) dialClient() *http.Client {
	s.dialClientOnce.Do(func() {
		s.dialHTTP = &http.Client{
			Timeout: time.Duration(s.probeTimeMs) * time.Millisecond,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
	})
	return s.dialHTTP
}

// requestStallKick moves traffic onto a neighbouring ladder rung for
// stallKickHold, then lets the scheduler put it back.
//
// The failure it answers: the keepalive stops passing traffic while the
// out-of-tunnel probe keeps getting answers. That combination is not a dark
// path — it is this particular five-tuple being throttled or blackholed while
// the host and the route are fine. Measured 2026-08-22: 13 such events, each
// lasting 27–42 seconds, which is hysteria's 30s MaxIdleTimeout plus change —
// i.e. they ended when the session finally timed out and was re-dialed, and the
// whole outage was time spent waiting for that.
//
// There is no Clash call that resets an outbound's session, so the kick works by
// routing through a different outbound, which dials its own. See Sibling.
func (s *Supervisor) requestStallKick() {
	s.kickMu.Lock()
	if time.Now().Before(s.kickUntil) {
		s.kickMu.Unlock()
		return // one already running; let it finish before judging it
	}
	s.stateMu.Lock()
	down := s.state.BandwidthDownMbps
	s.stateMu.Unlock()

	sib := s.schedule.Sibling(down)
	if sib == 0 {
		s.kickMu.Unlock()
		return // no ladder to step onto
	}
	s.kickUntil = time.Now().Add(stallKickHold)
	s.kickRung = sib
	s.kickMu.Unlock()

	member := BandwidthRungTag(sib)
	if err := s.clash.SelectProxy(ProxySelectorTag, member); err != nil {
		log.Printf("stall kick: select %s failed: %v", member, err)
		return
	}
	log.Printf("stall kick: keepalive dark ×%d while the probe answers — moving to %s for %s to dial a fresh session",
		stallKickThreshold, member, stallKickHold)
}

// stallKick reports the rung a running kick is holding, and whether one is
// running at all.
func (s *Supervisor) stallKick() (int, bool) {
	s.kickMu.Lock()
	defer s.kickMu.Unlock()
	if time.Now().After(s.kickUntil) {
		return 0, false
	}
	return s.kickRung, true
}

// onFallback reports whether traffic is on the hy2 ladder because the primary
// was measured failing.
func (s *Supervisor) onFallback() bool {
	if !s.hasReality {
		return false
	}
	s.fallbackMu.Lock()
	defer s.fallbackMu.Unlock()
	return s.fallback.active()
}

// enterFallback moves traffic off the REALITY primary onto the hy2 ladder.
//
// This is the only lever that changes transport, and it is a bet: REALITY has
// been measured not carrying while the UDP probe to the same host says the
// host and the route are fine, so the other transport is the thing left to
// try. Brutal on the ladder is also the one property hy2 still has over TCP —
// holding its rate through loss — so a lossy-but-open path is precisely where
// the fallback earns its keep.
//
// The switch itself is one Clash API call, because both legs are members of
// the same selector. sing-box is not restarted, the TUN is not rebuilt, and
// connections already established are untouched.
//
// cause names the evidence that triggered this, and is not decoration: two
// independent signals can reach here — an established session going dark, and
// newly opened ones failing while the old one is fine — and they are opposite
// failures that happen to share a remedy. A log line that always claimed the
// first would misattribute every firing of the second, in the one record
// anybody has when reconstructing an episode hours later.
func (s *Supervisor) enterFallback(cause string) {
	if !s.hasReality {
		return
	}
	// A `mole leg` pin disarms the failover outright. Pinning a leg mole would
	// otherwise move off is a deliberate act — measuring it, or knowing
	// something about tonight that the health signals do not — and a daemon
	// that overrides it two minutes later is not offering a choice.
	if utils.ReadLeg() != utils.LegAuto {
		return
	}
	s.fallbackMu.Lock()
	changed := s.fallback.enter(time.Now())
	dwell := s.fallback.dwell
	s.fallbackMu.Unlock()
	if !changed {
		return
	}
	target := s.ladderTarget()
	// Pre-warm, but do not gate on it: the leg we are on is carrying nothing, so
	// even an unproven alternative is the better bet.
	if _, err := s.clash.TestDelay(target, s.probeURL, s.probeTimeMs); err != nil {
		log.Printf("fallback: pre-warm %s failed: %v — switching anyway, the current leg is carrying nothing", target, err)
	}
	if err := s.clash.SelectProxy(ProxySelectorTag, target); err != nil {
		log.Printf("fallback: select %s failed: %v — staying on %s", target, err, RealityLegTag)
		s.fallbackMu.Lock()
		s.fallback.leave(time.Now())
		s.fallbackMu.Unlock()
		return
	}
	log.Printf("fallback: %s — moving to the hy2 ladder (%s, UDP/443) for %s; TCP/443 is what stopped carrying, and Brutal on UDP is the other bet",
		cause, target, dwell)
	go s.diagnoseRealityPort()
	s.resetKeepaliveCount()
}

// tryReturnToPrimary re-tests REALITY once the dwell has elapsed and hands
// traffic back if it will dial.
//
// A passing delay probe is weaker evidence than it looks: establishing a NEW
// connection is precisely what can keep working while established ones are
// being killed. So the probe GATES the attempt rather than proving the leg — if
// TCP/443 is still dark, the keepalive says so within ~90s, the fallback fires
// again, and fallbackState doubles the dwell so the next test costs less often.
func (s *Supervisor) tryReturnToPrimary() {
	if !s.hasReality {
		return
	}
	s.fallbackMu.Lock()
	due := s.fallback.dueBack(time.Now())
	s.fallbackMu.Unlock()
	if !due {
		return
	}
	// The port first, then the leg. A refused SYN is the node saying nothing
	// is listening — the one answer that does not get better with waiting —
	// and it is told apart here from a silent one, which the delay test
	// below folds into the same generic error.
	if s.realityAddr != "" {
		rtt, v, perr := ProbeTCP(s.realityAddr, time.Duration(s.probeTimeMs)*time.Millisecond)
		switch v {
		case utils.ProbeRefused:
			s.noteRealityServerDown(fmt.Sprintf("connection refused in %dms", int(rtt/time.Millisecond)))
			s.fallbackMu.Lock()
			s.fallback.extend(time.Now())
			dwell := s.fallback.dwell
			s.fallbackMu.Unlock()
			log.Printf("fallback: %s REFUSED at %s (%v) — the node is up and nothing is listening on the port; this is the server, not the path. `mole server repair` reinstalls it. Re-testing in %s",
				RealityLegTag, s.realityAddr, perr, dwell)
			return
		case utils.ProbeAlive:
			s.noteRealityServerDown("")
		}
	}
	if _, err := s.clash.TestDelay(RealityLegTag, s.probeURL, s.probeTimeMs); err != nil {
		s.fallbackMu.Lock()
		s.fallback.extend(time.Now())
		dwell := s.fallback.dwell
		s.fallbackMu.Unlock()
		log.Printf("fallback: %s still will not dial (%v) — holding the hy2 ladder another %s", RealityLegTag, err, dwell)
		return
	}
	s.fallbackMu.Lock()
	left := s.fallback.leave(time.Now())
	s.fallbackMu.Unlock()
	if left {
		log.Printf("fallback: %s dials again — handing traffic back to it, on probation for %s", RealityLegTag, fallbackProbation)
		s.resetKeepaliveCount()
	}
}

// noteRealityServerDown records (or clears, with "") the out-of-tunnel
// finding that the REALITY port refuses, so `mole status` can say "the server
// is not installed" instead of "measured dark".
func (s *Supervisor) noteRealityServerDown(reason string) {
	s.stateMu.Lock()
	changed := s.state.RealityServerDown != reason
	s.state.RealityServerDown = reason
	s.stateMu.Unlock()
	if changed {
		_ = utils.WriteState(s.snapshot())
	}
}

// diagnoseRealityPort runs the TCP probe once, off the caller's goroutine, so
// the first fallback of a node whose REALITY server never came up says so in
// the log within seconds rather than after the first dwell.
func (s *Supervisor) diagnoseRealityPort() {
	if s.realityAddr == "" {
		return
	}
	rtt, v, err := ProbeTCP(s.realityAddr, time.Duration(s.probeTimeMs)*time.Millisecond)
	switch v {
	case utils.ProbeRefused:
		s.noteRealityServerDown(fmt.Sprintf("connection refused in %dms", int(rtt/time.Millisecond)))
		log.Printf("fallback: diagnosis — %s REFUSES TCP at %s (%v): the node is up and no REALITY server is listening. Not a path problem; `mole server repair` reinstalls it",
			RealityLegTag, s.realityAddr, err)
	case utils.ProbeSilent:
		log.Printf("fallback: diagnosis — %s is silent on TCP at %s: SYNs are being dropped on the path (or by the cloud firewall), the server may well be fine", RealityLegTag, s.realityAddr)
	case utils.ProbeAlive:
		s.noteRealityServerDown("")
		log.Printf("fallback: diagnosis — %s accepts TCP at %s in %dms: the port answers, so it is the flow that is being killed, not the listener", RealityLegTag, s.realityAddr, int(rtt/time.Millisecond))
	}
}

// releaseFallback drops the failover's hold on the hy2 ladder without waiting
// for the dwell: for when a `mole leg` pin has taken the decision away from it,
// or when the fallback turned out to be dark as well.
//
// Necessary because the pin is a file the scheduler reads and fallbackState is
// memory the scheduler writes: leaving the two disagreeing would mean the pin
// selects a leg every tick while onFallback() keeps reporting the failover is
// in charge, and `mole status` would print whichever one it happened to ask.
func (s *Supervisor) releaseFallback(reason string) {
	s.fallbackMu.Lock()
	left := s.fallback.leave(time.Now())
	s.fallbackMu.Unlock()
	if left {
		log.Printf("fallback: released — %s", reason)
		s.resetKeepaliveCount()
	}
}

// resetKeepaliveCount clears the failure streak after a transport change. The
// count measured the leg just left; carrying it across would make the first beat
// on the new one read as a continuing failure and could re-trip the very
// threshold that caused the switch.
func (s *Supervisor) resetKeepaliveCount() {
	s.stateMu.Lock()
	s.state.KeepaliveFails = 0
	s.state.KeepaliveFailingSince = time.Time{}
	s.state.LastKeepaliveError = ""
	// The fresh-dial window goes with it: every caller of this is a leg
	// change, and dials made on the previous leg are not evidence about the
	// one traffic is on now. See State.ResetDials for the episode.
	s.state.ResetDials()
	s.noteHealthLocked()
	s.stateMu.Unlock()
}

// runBandwidthScheduler points the `proxy` selector at the ladder rung carrying
// the ceiling that should be in force. It never restarts sing-box: the switch is
// a single Clash-API call, so the TUN and in-flight connections survive untouched.
// Before each flip it pre-warms the target outbound (a delay probe re-dials its
// QUIC session) so the first connection after the switch is instant, not a fresh
// handshake.
//
// Two inputs decide the rung, in priority order:
//  1. a manual pin from `mole ceiling <n>` (~/.mole/ceiling), which wins outright
//  2. otherwise the local clock's peak/off-peak profile
//
// The pin is re-read every tick rather than latched at startup, so `mole ceiling`
// takes effect on a running daemon without signalling it. The CLI also fires its
// own Clash call so the change is immediate instead of up to a tick late; this
// loop is what makes it *stick* against the clock.
func (s *Supervisor) runBandwidthScheduler(ctx context.Context) {
	// Selector flips ride the Clash API; wait for it like the probe/keepalive do.
	if err := s.waitClashReady(ctx, 10*time.Second); err != nil {
		log.Printf("clash api not ready for bandwidth scheduler: %v (will keep trying)", err)
	}

	applied := ""
	warnedPin := 0 // last bad pin we complained about, so we complain once per value
	backoff := 0   // rungs currently stepped down below the clock/pin target
	var heal healState
	apply := func() {
		now := time.Now()
		member := s.schedule.Member(now)
		name, _, down := s.schedule.Profile(now)
		// A pin only counts if the config actually carries that rung — otherwise
		// we would select a member sing-box has never heard of and the call would
		// fail every tick. An out-of-range pin degrades to clock control.
		pin := utils.ReadCeiling()
		if pin > 0 {
			if s.schedule.HasRung(pin) {
				member, name, down = BandwidthRungTag(pin), "pinned", pin
				warnedPin = 0
			} else if warnedPin != pin {
				warnedPin = pin
				log.Printf("bandwidth scheduler: pinned ceiling %d Mbps has no rung in this config (available: %v) — following the clock", pin, s.schedule.Rungs())
			}
		}
		// Retiring a pin is as much a change of intent as setting one, so the
		// clock target (pin 0) gets the same treatment. Done here rather than in
		// retune() because the pin is read on this path and the cleared backoff
		// has to apply to the very tick that noticed the change.
		heal.retarget(pin)
		backoff = heal.backoff
		// Self-healing. The clock and the pin both encode what the link was
		// measured to carry, and a link that is currently failing is by
		// definition not carrying it. Brutal holds its declared rate straight
		// through loss, so an over-set ceiling keeps flooding a path that is
		// already dropping packets — descending is the only lever that helps, and
		// it is the one that used to be missing: health was observed, logged, and
		// then acted upon by nobody.
		//
		// The pin stays the resting point, not a floor. "3 Mbps is my normal"
		// still means the tunnel returns to 3 once the path is clean; it should
		// not mean the tunnel keeps hammering 3 through a blackout.
		if backoff > 0 {
			if lower := s.schedule.StepDown(down, backoff); lower > 0 && lower != down {
				member, down = BandwidthRungTag(lower), lower
				name = fmt.Sprintf("%s↓%d", name, backoff)
			}
		}
		// A running stall kick outranks both the clock and the pin, for as long
		// as it lasts. Without this the very next tick would select the target
		// rung again — and with it the session the kick was shedding, since the
		// outbound is only re-dialed once its old session has idled out.
		if rung, kicking := s.stallKick(); kicking {
			member, down = BandwidthRungTag(rung), rung
			name += "↷kick"
		}
		// The leg decision sits above the kick and everything under it. REALITY
		// is TCP, so no Brutal ceiling applies to it and none of the reasoning
		// above — clock, pin, backoff, kick — has anything to say while it is
		// the leg; all of that is the hy2 ladder's business and applies the
		// moment traffic is on the ladder, whether pinned there or pushed there
		// by the failover.
		//
		// Above all of it sits `mole leg`. Everything else on this path is an
		// inference from evidence about which leg or rung the tunnel should be
		// on. A leg pin is not evidence, it is instruction, so it is read last
		// and wins. It is also re-read every tick rather than latched, for the
		// same reason the ceiling pin is: `mole leg` must take on a running
		// daemon without signalling it. Either pin also releases a fallback in
		// progress — otherwise the pin would select a leg here every tick while
		// the failover still believed it owned the decision.
		switch utils.ReadLeg() {
		case utils.LegReality:
			if s.onFallback() {
				s.releaseFallback("pinned to reality by `mole leg`")
			}
			member, name, down = RealityLegTag, "leg-reality", 0
		case utils.LegHy2:
			if s.onFallback() {
				s.releaseFallback("pinned to hy2 by `mole leg`")
			}
			name = "leg-hy2/" + name
		default:
			if s.hasReality {
				// The return is attempted first so a recovered primary is
				// handed back on this same tick rather than one later.
				s.tryReturnToPrimary()
				if s.onFallback() {
					name = "fallback-hy2/" + name
				} else {
					member, name, down = RealityLegTag, "reality", 0
				}
			}
		}
		// Compare against what the selector is *actually* pointing at, not against
		// what we last set. `mole ceiling` drives the same selector directly, so
		// the two can disagree — and when they do, skipping on the stale cache
		// leaves the CLI's choice live while this loop keeps reporting its own.
		// Falling back to the cache on a query failure keeps a flaky API from
		// turning into a re-select every tick.
		live, err := s.clash.Selection(ProxySelectorTag)
		if err != nil {
			live = applied
		}
		if member != live {
			// Pre-warm the target so it has a live QUIC session before we route real
			// traffic onto it. Best-effort: a cold target just costs one handshake on
			// the first connection.
			if _, err := s.clash.TestDelay(member, s.probeURL, s.probeTimeMs); err != nil {
				log.Printf("bandwidth scheduler: pre-warm %s failed: %v", member, err)
			}
			if err := s.clash.SelectProxy(ProxySelectorTag, member); err != nil {
				log.Printf("bandwidth scheduler: select %s failed: %v", member, err)
				return
			}
			log.Printf("bandwidth profile → %s (↓%d Mbps) via %s", name, down, member)
		}
		applied = member

		// Record unconditionally of whether a switch was needed: the selector can
		// already be right while the published state is stale (an out-of-band
		// `mole ceiling` puts it exactly there), and this state is what `mole
		// status` reads. Guarded on the recorded values so a steady tunnel still
		// costs no writes.
		s.stateMu.Lock()
		stale := s.state.BandwidthProfile != name || s.state.BandwidthDownMbps != down
		if stale {
			s.state.BandwidthProfile = name
			s.state.BandwidthDownMbps = down
		}
		s.stateMu.Unlock()
		if stale {
			_ = utils.WriteState(s.snapshot())
		}
	}

	// retune adjusts the backoff from the health the probes are already
	// measuring, so a degrading path lowers its own ceiling instead of waiting
	// for someone to notice. Deliberately slow in both directions and asymmetric
	// between them — see healState.
	retune := func() {
		s.stateMu.Lock()
		h := s.state.Health()
		s.stateMu.Unlock()
		// Drained every tick whether or not it is read, so the count never
		// straddles two ticks and one short burst cannot descend a rung by
		// itself.
		flooding := TakeFloodSightings() > 0
		if h == utils.HealthNoLink {
			backoff = heal.freeze()
			return
		}
		// Usable, not OK: a degraded verdict means the out-of-tunnel probe is
		// dropping datagrams while the tunnel still carries traffic. Counting
		// that as unhealthy is what made the ceiling unable to climb — the probe
		// loses three in a row often enough that the clean streak never survived
		// to fifteen.
		backoff = heal.tick(h.Usable(), flooding)
	}

	apply()

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			retune()
			apply()
		}
	}
}

func (s *Supervisor) waitClashReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := s.clash.Ping(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for clash api")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return fmt.Errorf("stopped")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// recordLidDestroy stores what will happen when the lid is shut, so `mole
// status` can answer it at any time instead of only the terminal that ran
// `mole up` being able to, and only until it scrolls away.
func (s *Supervisor) recordLidDestroy(armed bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	switch {
	case armed && s.lidDryRun:
		s.state.LidDestroy = utils.LidDestroyDryRun
		s.state.LidDestroyReason = "--lid-close-dry-run"
	case armed:
		s.state.LidDestroy = utils.LidDestroyArmed
		s.state.LidDestroyReason = ""
	default:
		s.state.LidDestroy = utils.LidDestroyOff
		s.state.LidDestroyReason = s.lidOffReason
	}
}

// recordLidDestroyed flips the marker from "what the daemon will do" to "what
// it did", stamped with when. Called from the sleep notification thread right
// after a successful lid-close destroy; the caller persists it. Kept a method
// so the transition is testable the same way recordLidDestroy is.
func (s *Supervisor) recordLidDestroyed(when time.Time) {
	s.markLidSettled("server destroyed on lid close at " + when.Format("2006-01-02 15:04"))
}

// markLidSettled records that no server is running for this daemon to owe a
// destroy on, whatever route got there: one this daemon deleted, or one that was
// already gone when the lid shut. Both end the debt, and both must end it the
// same way — the record is about whether anything is billing, not about who
// deleted what.
func (s *Supervisor) markLidSettled(reason string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state.LidDestroy = utils.LidDestroyFired
	s.state.LidDestroyReason = reason
	s.state.LidDestroyPendingSince = time.Time{}
	s.state.LidDestroyAttempts = 0
	// The debt is settled — the server this named is gone, so the record that
	// outlives daemons has to go with it or `mole status` nags about a VPS that
	// no longer exists.
	_ = utils.ClearLidDebt()
}

// recordLidDestroyPending marks the destroy as owed: it fired, it failed, and
// the server is still billing. Stamps the lid close on the first failure only,
// so PendingSince keeps measuring from when the debt was incurred rather than
// resetting on every retry.
func (s *Supervisor) recordLidDestroyPending(when time.Time, cause error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state.LidDestroy != utils.LidDestroyPending {
		s.state.LidDestroyPendingSince = when
		s.state.LidDestroyAttempts = 0
	}
	s.state.LidDestroy = utils.LidDestroyPending
	s.state.LidDestroyAttempts++
	s.state.LidDestroyReason = cause.Error()
	s.persistLidDebtLocked()
}

// recordLidDestroyCancelled gives up on an owed destroy because the lid was
// opened. Keeps PendingSince and the attempt count: they are the record of how
// long the server billed unattended, which is the only thing anyone can still
// learn from this once the tunnel is in use again.
func (s *Supervisor) recordLidDestroyCancelled(reason string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state.LidDestroy = utils.LidDestroyCancelled
	s.state.LidDestroyReason = reason
	s.persistLidDebtLocked()
}

// persistLidDebtLocked mirrors the debt out of state.json into a file that
// survives this daemon. Best effort: it is a record, not a lock, and a write
// that fails costs a warning nobody sees rather than correctness.
//
// Called under stateMu.
func (s *Supervisor) persistLidDebtLocked() {
	_ = utils.WriteLidDebt(&utils.LidDebt{
		Server:   s.state.Server,
		Since:    s.state.LidDestroyPendingSince.Round(0),
		Attempts: s.state.LidDestroyAttempts,
		Reason:   s.state.LidDestroyReason,
		State:    s.state.LidDestroy,
	})
}

// lidIsClosed reads the clamshell through the injectable hook.
func (s *Supervisor) lidIsClosed() int {
	if s.lidClosedFn == nil {
		return lidClosed()
	}
	return s.lidClosedFn()
}

// lidDestroyPending reports whether a destroy is still owed.
func (s *Supervisor) lidDestroyPending() (since time.Time, attempts int, owed bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state.LidDestroyPendingSince, s.state.LidDestroyAttempts,
		s.state.LidDestroy == utils.LidDestroyPending
}

func (s *Supervisor) snapshot() *utils.State {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.stateSnapshotLocked()
}

func (s *Supervisor) stateSnapshotLocked() *utils.State {
	cp := *s.state
	cp.SingboxPID = SingboxPID()
	return &cp
}

func recoverPanic(where string) {
	if r := recover(); r != nil {
		log.Printf("panic in %s: %v", where, r)
	}
}

func closedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// experimentInterval is how often each arm is sampled. Two arms at 20s is six
// probes a minute through the tunnel — small next to the keepalive's own two,
// and the cadence is what decides how long an answer takes: at 90 samples per
// arm, a full window is half an hour.
const experimentInterval = 20 * time.Second

// runExperiment drives the paired A/B sampler.
//
// One beat probes EVERY arm, back to back, in shuffled order. That pairing is
// the entire design. This tunnel's failure is intermittent at a scale that
// swamps sequential trials — 73% of one afternoon and 0% of another day, same
// node, same route — so an arm measured on Tuesday and an arm measured on
// Wednesday cannot be compared at all, and every previous verdict on this
// tunnel (port hopping, obfs) was reached exactly that way. Sampling both arms
// in the same seconds makes the episode common-mode: whatever the path is doing
// it is doing to both, and what survives the subtraction is the variable.
//
// Probes are sequential rather than concurrent for the same reason the two
// keepalive arms are: two requests racing through one tunnel measure each other.
//
// The sampler never touches the selector, never fails over and never changes a
// ceiling. It is an instrument, and an instrument that steers is one that
// cannot be trusted about what it steered.
func (s *Supervisor) runExperiment(ctx context.Context) {
	if err := s.waitClashReady(ctx, 10*time.Second); err != nil {
		log.Printf("experiment: Clash API never came up (%v) — not sampling", err)
		return
	}
	e := utils.ReadExperiment()
	if e == nil {
		return
	}
	log.Printf("experiment %q: sampling %d arms every %s (%s)", e.Name, len(e.Arms), experimentInterval, e.Note)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	t := time.NewTicker(experimentInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
		for _, i := range armOrder(len(e.Arms), r) {
			arm := &e.Arms[i]
			ms, err := s.clash.TestDelay(ExperimentArmTag(arm.Name), s.probeURL, s.probeTimeMs)
			arm.Record(ms, err)
		}
		// Checkpointed every beat so an evening of samples survives the restart
		// that any of these experiments is eventually going to involve.
		if err := utils.WriteExperiment(e); err != nil {
			log.Printf("experiment: could not persist samples: %v", err)
		}
	}
}
