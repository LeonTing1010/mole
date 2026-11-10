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
	"sync/atomic"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Supervisor keeps sing-box alive and probes VPS health for status reporting.
type Supervisor struct {
	configPath string
	// onRulesChanged 重新编译域名 rule-set，返回域名条数。由 cmd 层注入以避开
	// core→config 的循环依赖（config 已 import core）。nil = 关闭规则热更。
	onRulesChanged func() (int, error)
	// directDomains 返回当前判给 direct 出站的域名集合，供 PF 豁免学习当闸门用。
	// 同样由 cmd 层注入（core 不能 import config）。nil = 关闭豁免学习。
	directDomains  func() utils.DirectDomainSet
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
	// realityRateMbps overrides the shared ladder rate for node-side TCP Brutal.
	// Zero preserves the shared rate used by older server entries.
	realityRateMbps int
	// brutalGain forwards the per-server cwnd_gain to the keeper; zero = skip.
	brutalGain int
	// realityRateAuto hands the declared REALITY rate to the client-side
	// RateGovernor instead of pinning it. See SupervisorOpts.RealityRateAuto.
	realityRateAuto bool

	// Destroy-on-lid-close. Inert unless both are set; see runSleepDestroy.
	destroyOnLidClose bool
	onIdleDestroy     func(budget time.Duration) error
	// lidDryRun logs the lid close and destroys nothing, so the clamshell
	// detection can be proven on a given Mac without paying a redeploy.
	lidDryRun bool
	// lidGrace is SupervisorOpts.LidGrace. See onLidClose.
	lidGrace time.Duration
	// lidOffReason is what to tell the operator when the watcher is not armed.
	lidOffReason string
	// lidClosedFn reads the clamshell. A field rather than a direct call to
	// lidClosed so the retry path is testable: the machine running the tests
	// has its lid open by definition, and the branch that matters most is the
	// one taken when it is shut. Defaults to the real reader.
	lidClosedFn func() int
	// uplinkFn reports whether a physical interface carries a usable IPv4 —
	// injectable for the same reason as lidClosedFn: the case that matters, a
	// dark wake with Wi-Fi still unjoined, cannot be produced on demand.
	uplinkFn func() bool
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
	noUplinkLogged   bool // guarded by lidMu; one "waiting for the uplink" line per stretch

	stateMu sync.Mutex
	state   *utils.State
	// Incremented at handoff so an in-flight request on the old leg cannot
	// add its result to the new leg's fresh window or keepalive streak.
	probeGeneration atomic.Uint64
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

	// Leg bookkeeping. hasReality records that the config carries a
	// RealityLegTag member at all — without one hy2 is the only leg, REALITY
	// cannot be the primary, and every failover check below short-circuits.
	// fallbackMu guards the policy value and is never taken while stateMu is
	// held.
	hasReality bool
	// Unconfirmed TCP pacing is a capability warning. Authenticated transport
	// health and recovery are measured independently of this optional module.
	brutalUnpaced atomic.Bool
	realityAddr   string
	fallbackMu    sync.Mutex
	fallback      fallbackState

	// legTest rate-limits the comparative leg test in considerLegFailover.
	// The fresh-dial latch can stay set for an hour; without this the test
	// would re-dial REALITY every 15s for that entire hour to keep being told
	// the same thing. Its own mutex, taken alone, for the same reason as the
	// two above.
	legTestMu sync.Mutex
	legTestAt time.Time
	// legProbeFn substitutes the network boundary in recovery tests. Nil uses
	// fresh keeper and (when configured) external TLS/body requests.
	legProbeFn func(port, timeoutMs int) (time.Duration, error)
	// Enabled only with the transport-bound external forwarders in the config.
	verifyLegEgress           bool
	pressureSnapshotFn        func() floodSnapshot    // Nil uses the live sing-box log sensor.
	applicationDialSnapshotFn func() nodeDialSnapshot // Nil observes this engine's node dials.

	// dialsPaused records that fresh-dial sampling is suspended for a bad
	// local link, so the suspension is logged once per stretch.
	dialsPaused bool

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
	// DirectDomains 返回 custom-rules.json 判给 direct 的域名集合。守护进程用它
	// 当闸门，把这些域名实际拨往的境外地址补进 PF 的 direct 豁免表 ——
	// 没有它，一条 "outbound": "direct" 的境外规则会被 kill switch 自己丢掉
	// （见 utils/directexempt.go）。nil = 关闭。
	DirectDomains func() utils.DirectDomainSet
	ProbeAddr     string // VPS hy2 endpoint "ip:port" for direct UDP probe (recommended)
	ProbeURL      string // URL driven through the proxy for keepalive; also the fallback probe when ProbeAddr is empty
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
	// VerifyLegEgress requires real external TLS/body delivery as well as the
	// node's keeper response. Older configs without forwarders leave this off.
	VerifyLegEgress bool
	// RealityAddr is the REALITY leg's ip:port, for the out-of-tunnel TCP probe
	// that tells a node refusing the port from a path dropping it. Empty when
	// there is no such leg.
	RealityAddr string

	// Schedule enables the time-of-day Brutal ceiling. Gated by Schedule.Enabled()
	// (a peak profile must be configured); the config must also carry the
	// peak/off-peak selector that config.Build emits for the same server. Leave
	// it zero to keep a plain fixed-bandwidth server.
	Schedule        BandwidthSchedule
	RealityRateMbps int
	// BrutalGain forwards the per-server cwnd_gain pin to the keeper on every
	// rate push. Zero skips the gain parameter and lets the keeper use its
	// built-in default. See cmd/server.go:Server.BrutalGain for full doc.
	BrutalGain int
	// RealityRateAuto hands the REALITY leg's declared Brutal rate to the
	// client-side RateGovernor instead of treating RealityRateMbps as a fixed
	// pin. RealityRateMbps then seeds the loop, and the peak profile (four times
	// the seed when no peak is configured) is its ceiling. See core/governor.go
	// for why the correction has to happen on this side.
	RealityRateAuto bool

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
	// LidGrace holds the destroy for this long after the lid shuts. A lid
	// that reopens inside it keeps the server; one still shut on the first
	// wake past it loses it. Zero destroys at the lid close, as before.
	LidGrace time.Duration
	// LidDestroyOffReason is why DestroyOnLidClose is false, in the operator's
	// terms ("no Vultr key", a flag they passed). Recorded in state.json and
	// logged at startup so a disarmed watcher is discoverable later instead of
	// only at the moment it fails to fire.
	LidDestroyOffReason string
}

// NewSupervisor builds a supervisor ready to start. It does not start sing-box.
func NewSupervisor(configPath, serverName string, opts SupervisorOpts) *Supervisor {
	singboxNodeDials.configure(opts.RealityAddr)
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
		realityRateMbps:   opts.RealityRateMbps,
		brutalGain:        opts.BrutalGain,
		realityRateAuto:   opts.RealityRateAuto,
		hasReality:        opts.HasRealityLeg,
		verifyLegEgress:   opts.VerifyLegEgress,
		realityAddr:       opts.RealityAddr,
		destroyOnLidClose: opts.DestroyOnLidClose,
		onIdleDestroy:     opts.OnIdleDestroy,
		onRulesChanged:    opts.OnRulesChanged,
		directDomains:     opts.DirectDomains,
		lidDryRun:         opts.LidCloseDryRun,
		lidGrace:          opts.LidGrace,
		lidOffReason:      opts.LidDestroyOffReason,
		lidClosedFn:       lidClosed,
		uplinkFn:          hasUplink,
		lidDestroyed:      make(chan struct{}),
		linkPoke:          make(chan struct{}, 1),
		clash:             NewClashClient(opts.ClashAddr),
		state: &utils.State{
			Balance:   initialBalanceStatus(opts.HasRealityLeg && opts.VerifyLegEgress),
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
	// +1 = the gateway echo (runLinkProbe): the one instrument on this side of
	// the Wi-Fi, which is where a third of the bad minutes turned out to be.
	n := 6
	if s.hasReality && s.verifyLegEgress {
		n++
	}
	if s.directDomains != nil {
		n++
	}
	if scheduleOn {
		n++
	}
	if sleepOn {
		n++
	}
	wg.Add(n)
	if s.hasReality && s.verifyLegEgress {
		go func() { defer wg.Done(); defer recoverPanic("dual-routing"); s.runBalance(ctx) }()
	}

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

	// Goroutine C3: echo the default gateway and keep the local-link verdict.
	go func() {
		defer wg.Done()
		defer recoverPanic("link-probe")
		s.runLinkProbe(ctx)
	}()

	// Goroutine A3 (optional): keep the PF direct-exemption table in step with
	// what the direct rules actually dial. Without it every foreign domain
	// carrying "outbound": "direct" is silently dropped by our own kill switch
	// — see core/directexempt.go and utils/directexempt.go.
	if s.directDomains != nil {
		go func() {
			defer wg.Done()
			defer recoverPanic("direct-exempt")
			s.runDirectExempt(ctx)
		}()
	}

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
//
// dark says the in-tunnel keepalive is failing past its threshold: the path is
// not carrying traffic at all. Flooding during that is not evidence about the
// ceiling — nothing is being pushed through, and what ENOBUFS measures then is
// the send queue backing up behind a link that is not draining. All eighteen
// ENOBUFS bursts in the 2026-09-03..05 logs fell inside keepalive blackouts,
// and the nine descents they caused cost 15-45 minutes of bandwidth each for a
// path no rung could have fixed. So a flood inside a blackout holds, like a
// dark tick without one; a flood while the tunnel otherwise answers (the
// 2026-08-22 case this branch was built for) still descends.
func (h *healState) tick(healthy, flooding, dark bool) int {
	if flooding && dark {
		h.good = 0
		if !h.held {
			h.held = true
			log.Printf("self-heal: ENOBUFS during a keepalive blackout — holding the ceiling; the send queue is backing up behind a dark path, not a ceiling that is too high")
		}
		return h.backoff
	}
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
	var lastFreshDial time.Time
	for {
		s.keepaliveOnce()
		if s.shouldSampleFreshDial(lastFreshDial) {
			lastFreshDial = time.Now()
			s.dialOnce()
		}
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

// shouldSampleFreshDial keeps the fresh TCP/TLS measurement frequent while it
// is collecting a window or seeing any degraded sample. A full, clean window
// is enough evidence to stretch the next sample to one minute. Link failures
// are excluded because those samples would describe Wi-Fi rather than the leg.
func (s *Supervisor) shouldSampleFreshDial(last time.Time) bool {
	if s.linkBad() {
		return false
	}
	snapshot := singboxNodeDials.snapshot()
	if s.applicationDialSnapshotFn != nil {
		snapshot = s.applicationDialSnapshotFn()
	}
	s.stateMu.Lock()
	s.state.ApplicationNodeDialFailures = snapshot.sequence
	s.state.LastApplicationNodeDialFailureAt = snapshot.last
	wellKnownHealthy := len(s.state.DialSamples) == utils.DialWindow &&
		s.state.DialBad() == 0 && !s.state.SlowNewFlows
	s.stateMu.Unlock()
	interval := s.keepaliveEvery
	if wellKnownHealthy {
		interval = healthyFreshDialEvery
		// A clean sparse monitor does not erase real application failures.
		// Request another real sample at normal cadence; never insert a failed
		// vote or bypass the existing comparative handoff and local-link gates.
		age := time.Since(snapshot.last)
		pin := utils.ReadLeg()
		if age >= 0 && age <= 30*time.Second && !s.onFallback() && pin != utils.LegHy2 && pin != utils.LegBoth {
			interval = s.keepaliveEvery
		}
	}
	return last.IsZero() || time.Since(last) >= interval
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
		if last == "" && now != "" {
			// A dark wake joins Wi-Fi seconds after it starts, often after the
			// tick that would have retried an owed destroy has already found
			// no link and gone back to waiting. This is the moment the destroy
			// can actually reach Vultr; don't leave it to the next tick.
			go s.reconcileLidDebt("uplink back")
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

// healthyFreshDialEvery reduces full TCP/TLS handshake traffic while the
// rolling window has enough evidence that new connections are healthy.
const healthyFreshDialEvery = time.Minute

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
	generation := s.probeGeneration.Load()
	start := time.Now()
	err := s.fetchMonitor(s.tunnelClient())
	delay := int(time.Since(start) / time.Millisecond)

	action := legHold
	s.stateMu.Lock()
	if generation != s.probeGeneration.Load() {
		s.stateMu.Unlock()
		return
	}
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
		// traffic is CARRIED that has stopped working. The fallback is for a
		// primary that stayed dark; abandoning the fallback is for an hy2 that
		// turned out dark too. See keepaliveLegAction.
		action = keepaliveLegAction(s.state.KeepaliveFails,
			s.state.LastProbeVerdict == utils.ProbeAlive, !s.state.LinkBad, s.activeOnHy2(), s.onFallback(), utils.ReadLeg())
	} else {
		s.state.KeepaliveFails = 0
		s.state.KeepaliveFailingSince = time.Time{}
		s.state.LastKeepaliveMs = delay
		s.state.LastKeepaliveError = ""
	}
	// UDP silence must not strand a failed fallback. Recovery still requires
	// three authenticated delivery checks; a bare TCP listener is not enough.
	rescueStuck := action == legHold &&
		s.state.KeepaliveFails >= fallbackEnterFails &&
		s.onFallback() &&
		!s.state.LinkBad &&
		s.realityAddr != ""

	s.noteHealthLocked()
	_ = utils.WriteState(s.stateSnapshotLocked())
	s.stateMu.Unlock()

	switch action {
	case legFallback:
		s.enterFallback(fmt.Sprintf("keepalive dark ×%d on %s while the UDP probe still answers", fallbackEnterFails, RealityLegTag))
	case legAbandonFallback:
		s.recoverPrimary()
	}

	if rescueStuck {
		s.rescueStuckFallback()
	}
}

// rescueStuckFallback requires real protocol delivery even when UDP probes are
// silent. A TCP SYN-ACK alone cannot establish recovery of authenticated traffic.
func (s *Supervisor) rescueStuckFallback() { s.recoverPrimary() }

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
// self-heal backoff is deliberately left out — it is a refinement the scheduler
// applies on its own tick; this is the resting point the failover pre-warms and
// the comparative test dials.
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
// comparative gate in considerLegFailover. Not the ceiling (measured
// 2026-09-01: the tunnel was near-idle at 0.0-0.1 Mbps with
// zero ENOBUFS through the whole episode, so there is no flood to back off —
// and an A/B during the episode had the LOWER rung failing 25% of new
// connections against 4% on the higher one, making a descent actively worse).
func (s *Supervisor) dialOnce() {
	// A bad first hop makes every fresh dial slow, and a window filled with
	// those would convict whichever leg is in force the moment the link comes
	// back. Sample nothing until it clears — the verdict is about the leg, and
	// the leg is not what is being measured right now.
	if s.linkBad() {
		if !s.dialsPaused {
			s.dialsPaused = true
			log.Printf("fresh dial: sampling paused while the local link is bad — these would measure the Wi-Fi, not the leg")
		}
		return
	}
	s.dialsPaused = false
	generation := s.probeGeneration.Load()
	start := time.Now()
	err := s.fetchMonitor(s.dialClient())
	ms := int(time.Since(start) / time.Millisecond)

	s.stateMu.Lock()
	if generation != s.probeGeneration.Load() {
		s.stateMu.Unlock()
		return
	}
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
	failing := freshFlowsFailing(s.state)
	probeAlive := s.state.LastProbeVerdict == utils.ProbeAlive
	s.noteHealthLocked()
	_ = utils.WriteState(s.stateSnapshotLocked())
	s.stateMu.Unlock()

	if legGateOpen(failing, probeAlive, s.hasReality) {
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
// The comparison only moves traffic on hard evidence: the active leg's NEW
// connections must be FAILING, not merely slow. Measured 2026-10-14: the gate
// fired six times between 09:03 and 13:37, and every enter was opened by
// "slow or failed" samples that were in fact four SUCCESSFUL dials of 1.1-2.4s
// — zero failures — while the standby's keeper dial happened to read faster
// (371ms against REALITY's 768ms). Traffic moved onto a leg whose next five
// real connections then timed out, and moved back on the mirror measurement.
// A dial that answers in 1.2s is slow, not dead; acting on it is what made the
// tunnel ping-pong and reset Brutal's ramp each time. Only a failed dial — a
// connection that carried nothing — opens this gate. (on fallback, the same
// bar applies to the hy2 ladder's new flows, which is what rescues the October
// 7 case: warm flows fine, every new UDP flow timing out.)
//
// On primary it qualifies the scheduled UDP rung; on fallback it qualifies
// REALITY for an early handback. The fresh-dial latch opens either comparison,
// even when the UDP datagram probe is silent: the candidate's real requests
// must prove that it carries traffic before a switch is allowed.
func (s *Supervisor) considerLegFailover() {
	// Not on a bad local link: the idle leg would be measured through the
	// same radio and the answer would be about the Wi-Fi.
	if s.linkBad() {
		return
	}
	s.legTestMu.Lock()
	if !legTestDue(s.hasReality, s.onFallback(), utils.ReadLeg(), time.Now(), s.legTestAt) {
		s.legTestMu.Unlock()
		return
	}
	s.legTestAt = time.Now()
	s.legTestMu.Unlock()

	onFallback := s.onFallback()
	_, fails, window := s.dialWindow()
	idle := s.ladderTarget()
	hy2, reality, err := s.legRace()
	activeTag, candidateTag := RealityLegTag, idle
	active, candidate := reality, hy2
	if onFallback {
		activeTag, candidateTag = idle, RealityLegTag
		active, candidate = hy2, reality
	}
	// Another scheduler tick or a manual pin may have changed the leg while
	// the spaced probes were running. Do not apply evidence to a new leg.
	if s.onFallback() != onFallback || utils.ReadLeg() != utils.LegAuto || s.linkBad() {
		return
	}
	if err != nil || !legComparison(candidate, active) {
		switch {
		case err != nil:
			log.Printf("new connections failed (%d/%d), paired leg probe stopped (%v); holding %s", fails, window, err, activeTag)
		case candidate.err != nil:
			log.Printf("new connections failed (%d/%d), candidate %s failed (%v), active probe error=%v; holding %s", fails, window, candidateTag, candidate.err, active.err, activeTag)
		case candidate.delay > time.Duration(utils.DialSlowMs)*time.Millisecond:
			log.Printf("new connections failed (%d/%d), candidate %s is slow too (%dms against %s's %dms); holding %s",
				fails, window, candidateTag, candidate.delay.Milliseconds(), activeTag, active.delay.Milliseconds(), activeTag)
		default:
			log.Printf("new connections failed (%d/%d), candidate %s is not meaningfully better (%dms against %s's %dms); holding %s",
				fails, window, candidateTag, candidate.delay.Milliseconds(), activeTag, active.delay.Milliseconds(), activeTag)
		}
		return
	}
	if onFallback {
		s.handoffToPrimary(fmt.Sprintf("%d/%d new connections failed on %s; %s passed all %d paired probes (worst %dms) — abandoning unhealthy fallback before dwell",
			fails, window, activeTag, RealityLegTag, legGateProbes, reality.delay.Milliseconds()))
		return
	}
	s.enterFallback(fmt.Sprintf("%d/%d new connections failed on %s; standby %s passed all %d probes (worst %dms), primary worst %dms error=%v",
		fails, window, RealityLegTag, idle, legGateProbes, hy2.delay.Milliseconds(), reality.delay.Milliseconds(), reality.err))
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

// legRace times BOTH legs against the same destination, alternating, and
// records the worst delay and any error separately on each leg. A leg is
// only clean if all of its probes were; a failed primary does not prevent
// gathering the standby's remaining samples.
//
// Both halves reach the same keeper and external TLS origin through separate,
// transport-bound forwarders. Symmetric instruments avoid qualifying a leg
// with a quick node-local response while real external connections time out.
//
// Alternating rather than running the two blocks back to back matters on an
// intermittent path. The fault this gate exists to catch comes and goes on a
// scale of seconds; measure one leg for twelve seconds and then the other for
// twelve, and a lull that lands on one of them is indistinguishable from a leg
// that is genuinely better. Interleaved, an outage is common-mode.
func (s *Supervisor) legRace() (hy2, reality legMeasurement, err error) {
	if s.verifyLegEgress {
		// On fallback, measure the rung that is actually carrying new flows,
		// including self-heal changes; on primary, qualify the target rung.
		target := s.ladderTarget()
		if s.onFallback() {
			if live, e := s.clash.Selection(ProxySelectorTag); e == nil && BandwidthRungMbps(live) > 0 {
				target = live
			}
		}
		if e := s.clash.SelectProxy(Hy2ProbeSelectorTag, target); e != nil {
			return hy2, reality, fmt.Errorf("select UDP probe rung %s: %w", target, e)
		}
	}
	return measureLegs(func(port int) (time.Duration, error) {
		return s.probeLeg(port)
	}, s.waitLegProbe)
}

// dialWindow reports the fresh-dial verdict's shape for log lines: how many of
// the window were slow-or-failed (the latch's reading, and what `mole status`
// shows as degraded), how many carried NOTHING (the count that may open the
// failover gate — see State.DialFails), and the window size.
func (s *Supervisor) dialWindow() (bad, fails, window int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state.DialBad(), s.state.DialFails(), len(s.state.DialSamples)
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
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		s.tunnelHTTP = &http.Client{
			Timeout:       time.Duration(s.probeTimeMs) * time.Millisecond,
			Transport:     transport,
			CheckRedirect: rejectMonitorRedirect,
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
			Timeout:       time.Duration(s.probeTimeMs) * time.Millisecond,
			CheckRedirect: rejectMonitorRedirect,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
	})
	return s.dialHTTP
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
// Three spaced, fresh keeper requests must all succeed within the healthy
// dial budget. This avoids returning on one fortunate moment of an intermittent
// fault. The normal keepalive and fresh-dial arms still watch the returned leg;
// fallbackState doubles the dwell if it fails again during probation.
func (s *Supervisor) tryReturnToPrimary() {
	if !s.hasReality {
		return
	}
	// Not during a bad local link: the test would fail for the Wi-Fi's
	// reasons and extend the dwell for the path's.
	if s.linkBad() {
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
	if err := verifyLegRecovery(func() (time.Duration, error) {
		return s.probeLeg(BrutalKeeperPort)
	}, s.waitLegProbe); err != nil {
		s.fallbackMu.Lock()
		s.fallback.extend(time.Now())
		dwell := s.fallback.dwell
		s.fallbackMu.Unlock()
		log.Printf("fallback: %s still will not dial (%v) — holding the hy2 ladder another %s", RealityLegTag, err, dwell)
		return
	}
	s.handoffToPrimary(fmt.Sprintf("%s passed %d spaced fresh dials; probation %s", RealityLegTag, legGateProbes, fallbackProbation))
}

// Commit the routing change before reporting recovery or discarding evidence.
func (s *Supervisor) handoffToPrimary(reason string) {
	if utils.ReadLeg() != utils.LegAuto || s.linkBad() {
		return
	}
	if err := s.clash.SelectProxy(ProxySelectorTag, RealityLegTag); err != nil {
		log.Printf("fallback: primary selection failed (%v); retaining fallback", err)
		return
	}
	s.releaseFallback(reason)
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
	// A selector change leaves existing flows on their old outbound. Close
	// only this monitor's idle connection so its next beat tests the new leg.
	s.tunnelClient().CloseIdleConnections()
	s.stateMu.Lock()
	s.probeGeneration.Add(1)
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
	pushedRate, pushedAt := 0, time.Time{} // the TCP Brutal rate last CONFIRMED by the node's keeper
	unconfirmed := 0                       // consecutive pushes the keeper would not confirm
	warnedPin := 0                         // last bad pin we complained about, so we complain once per value
	backoff := 0                           // rungs currently stepped down below the clock/pin target
	var heal healState

	// Client-side rate governor (opt-in). When enabled it owns the declared
	// REALITY rate, seeded from the configured value; otherwise the configured
	// value stays a fixed pin. The governor runs on this goroutine only, so
	// govRate needs no lock — it is read by apply below and written by the
	// sampling branch in the same loop.
	govRate := 0
	var gov *RateGovernor
	if s.realityRateAuto && s.realityRateMbps > 0 {
		govRate = s.realityRateMbps
		gov = NewRateGovernor(govRate, 3, s.governorCeiling(govRate))
	}
	apply := func() {
		generation := s.probeGeneration.Load()
		pacingChanged := false
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
		// The leg decision selects a transport. The ladder controls HY2; REALITY
		// also has node-side TCP Brutal pacing, independently overridden by
		// realityRateMbps below. A HY2 descent does not change that override.
		//
		// Nothing else may write the ladder's selector between ticks. Until
		// 2026-10-14 a "stall kick" did, moving the whole ladder onto a
		// neighbouring rung for 45s whenever the keepalive failed twice; it was
		// measured never to restore the tunnel (8 firings in one day, keepalive
		// still failing across four of them) and only ever added a second writer
		// to this selector. See spec.md F3.
		//
		// Above all of it sits `mole leg`. Everything else on this path is an
		// inference from evidence about which leg or rung the tunnel should be
		// on. A leg pin is not evidence, it is instruction, so it is read last
		// and wins. It is also re-read every tick rather than latched, for the
		// same reason the ceiling pin is: `mole leg` must take on a running
		// daemon without signalling it. Either pin also releases a fallback in
		// progress — otherwise the pin would select a leg here every tick while
		// the failover still believed it owned the decision.
		// Above all of it sits `mole leg`. A leg pin is not evidence, it is
		// instruction, so it is read last and wins. Also re-read every tick
		// so `mole leg` takes on a running daemon without signalling it.
		member, name = s.schedulerPickLeg(member, name, down)
		// `down` is kept whichever leg carries traffic: since 2026-09-06 the
		// node paces its TCP toward this client at the same rung (TCP Brutal,
		// see cmd/brutal.go), so the ceiling governs REALITY too. Push it
		// through the REALITY leg on change, and every ten minutes regardless
		// in case the keeper or the node restarted — a GET through a named
		// outbound is what the delay probe already is, one fresh connection.
		tcpRate := down
		if govRate > 0 {
			tcpRate = govRate
		} else if s.realityRateMbps > 0 {
			tcpRate = s.realityRateMbps
		}
		if s.hasReality && tcpRate > 0 && (tcpRate != pushedRate || time.Since(pushedAt) > 10*time.Minute) {
			pushedRate, pushedAt, unconfirmed, pacingChanged = s.schedulerPushBrutal(tcpRate, pushedRate, pushedAt, unconfirmed)
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
			if generation != s.probeGeneration.Load() {
				return
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
		// costs no writes unless pacing capability was refreshed.
		s.stateMu.Lock()
		stale := s.state.BandwidthProfile != name || s.state.BandwidthDownMbps != down
		if stale {
			s.state.BandwidthProfile = name
			s.state.BandwidthDownMbps = down
		}
		s.stateMu.Unlock()
		if stale || pacingChanged {
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
		// No uplink, or an uplink whose first hop is dropping packets: neither
		// is evidence about the tunnel. See healState.freeze.
		if h == utils.HealthNoLink || h == utils.HealthLocalLink {
			backoff = heal.freeze()
			return
		}
		// Usable, not OK: a degraded verdict means the out-of-tunnel probe is
		// dropping datagrams while the tunnel still carries traffic. Counting
		// that as unhealthy is what made the ceiling unable to climb — the probe
		// loses three in a row often enough that the clean streak never survived
		// to fifteen.
		backoff = heal.tick(h.Usable(), flooding, h == utils.HealthPathDead)
	}

	apply()

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	// The governor samples every third tick. A probe costs real bandwidth
	// (it saturates the link for about a second), so it cannot ride every
	// minute; three minutes is the compromise between noticing a degraded
	// Wi-Fi link and not spending the user's quota on diagnostics.
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			retune()
			apply()
			ticks++
			if gov == nil || ticks%3 != 0 || !s.healthUsable() {
				continue
			}
			next, changed, why := s.sampleGovernor(gov)
			if !changed {
				continue
			}
			log.Printf("rate governor: ↓%d → ↓%d Mbps (%s)", govRate, next, why)
			govRate = next
			apply()
		}
	}
}

// governorCeiling bounds how far the rate governor may climb. The peak profile
// is the operator's statement about what the path can carry at its best, so it
// wins when configured; otherwise the seed gets four rungs of headroom.
func (s *Supervisor) governorCeiling(seed int) int {
	if s.schedule.PeakDown > seed {
		return s.schedule.PeakDown
	}
	return seed * 4
}

// healthUsable reports whether the path is in a state where a throughput probe
// says something about capacity rather than about an outage.
func (s *Supervisor) healthUsable() bool {
	s.stateMu.Lock()
	h := s.state.Health()
	s.stateMu.Unlock()
	return h.Usable() && h != utils.HealthPathDead
}

// sampleGovernor measures what the tunnel actually delivers right now and feeds
// it to the governor.
//
// The burst is sized to about one second at the declared rate, capped at 6 MB:
// scaling it with the rate keeps a low rung from taking a full minute to probe,
// and the cap keeps an over-declared rate from turning a diagnostic into a
// download. A failed probe is not evidence about the path — the health machine
// owns "dead" — so it is dropped rather than reported as zero.
func (s *Supervisor) sampleGovernor(g *RateGovernor) (int, bool, string) {
	rate := g.Rate()
	total := int64(rate) * 125000 // Mbps → bytes for one second
	if total < 1<<20 {
		total = 1 << 20
	}
	if total > 6<<20 {
		total = 6 << 20
	}
	const streams = 8
	perStream := int(total) / streams
	if perStream < 64<<10 {
		perStream = 64 << 10
	}
	achieved, err := BulkConcurrent(fmt.Sprintf("http://127.0.0.1:%d", BrutalKeeperPort), streams, perStream)
	if err != nil {
		return rate, false, ""
	}
	return g.Observe(achieved)
}

// schedulerPushBrutal pushes the REALITY leg's TCP Brutal pacing rate to the
// node's keeper via PushBrutalRateViaWithGain. When the keeper confirms it
// updates pushedRate/pushedAt and clears unconfirmed; on persistent refusal
// it flips s.brutalUnpaced. Also updates s.state.RealityRateMbps and the
// RealityPacingState/Error triplet so `mole status` shows what happened.
//
// The 10-minute re-push interval (fire regardless of change) is enforced by
// the caller so this method stays pure: it only decides *whether* to emit a
// log line based on (tcpRate != pushedRate || brutalUnpaced was true).
//
// Returns (newPushedRate, newPushedAt, newUnconfirmed, pacingChanged).
func (s *Supervisor) schedulerPushBrutal(tcpRate, pushedRate int, pushedAt time.Time, unconfirmed int) (int, time.Time, int, bool) {
	pacingChanged := true
	res, err := PushBrutalRateViaWithGain(BrutalKeeperPort, tcpRate, s.brutalGain, s.probeTimeMs)
	switch {
	case err != nil:
		unconfirmed++
		if tcpRate != pushedRate || unconfirmed == brutalGiveUp {
			log.Printf("brutal: no answer from the node's keeper for ↓%d Mbps (%v) — REALITY is unpaced", tcpRate, err)
		}
	case res.Applied == 0:
		unconfirmed++
		log.Printf("brutal: the node heard ↓%d Mbps and did not apply it (%s; %d peer(s)) — REALITY is unpaced",
			tcpRate, res.Detail, res.Peers)
	default:
		if tcpRate != pushedRate || s.brutalUnpaced.Load() {
			log.Printf("brutal: node paces TCP toward this client at ↓%d Mbps (%d of %d peer(s))",
				tcpRate, res.Applied, res.Peers)
		}
		pushedRate, pushedAt, unconfirmed = tcpRate, time.Now(), 0
		s.stateMu.Lock()
		s.state.RealityRateMbps = tcpRate
		s.stateMu.Unlock()
		s.brutalUnpaced.Store(false)
	}
	if unconfirmed >= brutalGiveUp {
		s.brutalUnpaced.Store(true)
	}
	s.stateMu.Lock()
	s.state.RealityPacingState = "confirmed"
	s.state.RealityPacingError = ""
	if unconfirmed > 0 {
		s.state.RealityPacingState = "unconfirmed"
		if err != nil {
			s.state.RealityPacingError = err.Error()
		} else {
			s.state.RealityPacingError = res.Detail
		}
	}
	s.stateMu.Unlock()
	return pushedRate, pushedAt, unconfirmed, pacingChanged
}

// schedulerPickLeg reads the leg pin (mole leg) and applies it as the
// ultimate selector, overriding whatever the clock/backoff chose. A pin
// also releases any active fallback since both cannot own the decision.
// When no pin is set and a REALITY leg exists, it either returns to primary
// or stays on fallback. In LegBoth mode, it additionally consults the
// balance policy to decide whether to favour the HY2 probe selector.
func (s *Supervisor) schedulerPickLeg(member, name string, down int) (string, string) {
	switch utils.ReadLeg() {
	case utils.LegBoth:
		if s.onFallback() {
			s.releaseFallback("dual routing enabled")
		}
		member, name = RealityLegTag, "both/"+name
		s.stateMu.Lock()
		preferHy2 := s.state.Balance != nil && s.state.Balance.Enabled && s.state.Balance.Hy2Lanes >= BalanceLanes/2
		s.stateMu.Unlock()
		if preferHy2 {
			if live, e := s.clash.Selection(Hy2ProbeSelectorTag); e == nil {
				member = live
			} else {
				member = BandwidthRungTag(down)
			}
		}
	case utils.LegReality:
		if s.onFallback() {
			s.releaseFallback("pinned to reality by `mole leg`")
		}
		member, name = RealityLegTag, "leg-reality"
	case utils.LegHy2:
		if s.onFallback() {
			s.releaseFallback("pinned to hy2 by `mole leg`")
		}
		name = "leg-hy2/" + name
	default:
		if s.hasReality {
			s.tryReturnToPrimary()
			if s.onFallback() {
				name = "fallback-hy2/" + name
			} else {
				member, name = RealityLegTag, "reality/"+name
			}
		}
	}
	return member, name
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
// recordLidDeferred records that the lid shut and the destroy is being held
// for the grace period. Not persisted as debt: nothing has failed, and a
// daemon that dies here leaves a server that was deliberately kept.
func (s *Supervisor) recordLidDeferred(when time.Time) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state.LidDestroy = utils.LidDestroyDeferred
	s.state.LidDestroyPendingSince = when
	s.state.LidDestroyAttempts = 0
	s.state.LidDestroyReason = "lid shut; destroy held for the grace period"
}

// recordLidReopened re-arms the watcher after a lid that reopened inside the
// grace period. The server was kept on purpose, so this is the armed state
// again, not a cancelled one.
func (s *Supervisor) recordLidReopened() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state.LidDestroy = utils.LidDestroyArmed
	s.state.LidDestroyPendingSince = time.Time{}
	s.state.LidDestroyAttempts = 0
	s.state.LidDestroyReason = ""
}

// lidDeferred reports whether a destroy is being held, and since when.
func (s *Supervisor) lidDeferred() (since time.Time, held bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state.LidDestroyPendingSince, s.state.LidDestroy == utils.LidDestroyDeferred
}

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

// hasUplink reports whether any physical interface could carry a request now.
func hasUplink() bool {
	_, err := utils.DefaultPhysicalInterface()
	return err == nil
}

// uplinkUp reads the uplink through the injectable hook.
func (s *Supervisor) uplinkUp() bool {
	if s.uplinkFn == nil {
		return hasUplink()
	}
	return s.uplinkFn()
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
