package core

import (
	"context"
	"fmt"
	"log"
	"math/rand"
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
		// the mapping alive. The payload is a single generate_204, so the cost is
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
		destroyOnLidClose: opts.DestroyOnLidClose,
		onIdleDestroy:     opts.OnIdleDestroy,
		onRulesChanged:    opts.OnRulesChanged,
		lidDryRun:         opts.LidCloseDryRun,
		lidOffReason:      opts.LidDestroyOffReason,
		lidClosedFn:       lidClosed,
		lidDestroyed:      make(chan struct{}),
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
	n := 4
	if scheduleOn {
		n++
	}
	if sleepOn {
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
	if !flooding {
		if !h.held {
			h.held = true
			log.Printf("self-heal: path unhealthy but sing-box is not flooding (no ENOBUFS) — holding the ceiling; this is a dark path, not a ceiling that is too high")
		}
		return h.backoff
	}
	h.held = false
	h.bad++
	// Stop descending at healMaxBackoff. Below that the ceiling is no longer the
	// problem and dropping further only throttles a recovery we still want to be
	// ready for.
	if h.bad >= healBackoffTicks && h.backoff < healMaxBackoff {
		h.backoff, h.bad = h.backoff+1, 0
		log.Printf("self-heal: path unhealthy and flooding %d min — lowering the ceiling one rung (backoff %d)", healBackoffTicks, h.backoff)
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
	log.Printf("health: %s → %s (probe %s fails=%d err=%q; keepalive fails=%d err=%q)",
		s.lastHealth, h,
		s.state.LastProbeVerdict, s.state.ConsecutiveFails, s.state.LastProbeError,
		s.state.KeepaliveFails, s.state.LastKeepaliveError)
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
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
	}
}

// keepaliveOnce sends one request through the proxy outbound and records the
// result. A single failure stays informational — this path runs through
// sing-box's DNS engine, so one hiccup must not flip the verdict. But a streak
// of failures while the direct-UDP probe stays green is exactly the path-
// blackhole signature (probe proves the VPS process answers; keepalive proves
// the path passes traffic), so consecutive failures feed State.Health via
// KeepaliveFails. The vps-down verdict remains the probe's exclusive call.
func (s *Supervisor) keepaliveOnce() {
	delay, err := s.clash.TestDelay("proxy", s.probeURL, s.probeTimeMs)

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state.LastKeepaliveAt = time.Now()
	if err != nil {
		s.state.KeepaliveFails++
		if s.state.KeepaliveFails == 1 {
			s.state.KeepaliveFailingSince = time.Now()
		}
		s.state.LastKeepaliveMs = 0
		s.state.LastKeepaliveError = err.Error()
		log.Printf("tunnel keepalive failed (×%d): %v", s.state.KeepaliveFails, err)
	} else {
		s.state.KeepaliveFails = 0
		s.state.KeepaliveFailingSince = time.Time{}
		s.state.LastKeepaliveMs = delay
		s.state.LastKeepaliveError = ""
	}
	s.noteHealthLocked()
	_ = utils.WriteState(s.stateSnapshotLocked())
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
		healthy := s.state.Health() == utils.HealthOK
		s.stateMu.Unlock()
		// Drained every tick whether or not it is read, so the count never
		// straddles two ticks and one short burst cannot descend a rung by
		// itself.
		flooding := TakeFloodSightings() > 0
		backoff = heal.tick(healthy, flooding)
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
