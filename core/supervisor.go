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
	configPath     string
	probeAddr      string // "host:port" — direct UDP probe target
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
	ClashAddr      string
	ProbeAddr      string // VPS hy2 endpoint "ip:port" for direct UDP probe (recommended)
	ProbeURL       string // URL driven through the proxy for keepalive; also the fallback probe when ProbeAddr is empty
	ProbeEvery     time.Duration
	KeepaliveEvery time.Duration // in-tunnel keepalive cadence; 0 → default
	ProbeTimeMs    int

	// Schedule enables the time-of-day Brutal ceiling. Gated by Schedule.Enabled()
	// (a peak profile must be configured); the config must also carry the
	// peak/off-peak selector that config.Build emits for the same server. Leave
	// it zero to keep a plain fixed-bandwidth server.
	Schedule BandwidthSchedule
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
		configPath:     configPath,
		probeAddr:      opts.ProbeAddr,
		probeURL:       opts.ProbeURL,
		probeEvery:     opts.ProbeEvery,
		keepaliveEvery: opts.KeepaliveEvery,
		probeTimeMs:    opts.ProbeTimeMs,
		schedule:       opts.Schedule,
		clash:          NewClashClient(opts.ClashAddr),
		state: &utils.State{
			Mode:      utils.ModeProxy,
			PID:       os.Getpid(),
			Server:    serverName,
			StartedAt: time.Now(),
		},
		lastHealth: utils.HealthOK,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Run blocks, starting sing-box and the probe loop until Stop() is called.
// Returns when both goroutines have exited.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := Start(s.configPath); err != nil {
		return fmt.Errorf("initial sing-box start: %w", err)
	}
	defer Stop()
	_ = utils.WriteState(s.snapshot())

	// The scheduler runs whenever the config carries a ladder — not just when a
	// peak window is configured. Without a peak profile it has no clock work to
	// do, but it is still the thing that applies a manual `mole ceiling` pin.
	scheduleOn := len(s.schedule.Rungs()) >= 2

	var wg sync.WaitGroup
	n := 3
	if scheduleOn {
		n = 4
	}
	wg.Add(n)

	// Goroutine A: keep sing-box alive.
	go func() {
		defer wg.Done()
		defer recoverPanic("singbox-lifecycle")
		s.runLifecycle(ctx)
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
		rtt, v, perr := ProbeHy2UDP(s.probeAddr, time.Duration(s.probeTimeMs)*time.Millisecond)
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
	apply := func() {
		now := time.Now()
		member := s.schedule.Member(now)
		name, _, down := s.schedule.Profile(now)
		// A pin only counts if the config actually carries that rung — otherwise
		// we would select a member sing-box has never heard of and the call would
		// fail every tick. An out-of-range pin degrades to clock control.
		if pin := utils.ReadCeiling(); pin > 0 {
			if s.schedule.HasRung(pin) {
				member, name, down = BandwidthRungTag(pin), "pinned", pin
				warnedPin = 0
			} else if warnedPin != pin {
				warnedPin = pin
				log.Printf("bandwidth scheduler: pinned ceiling %d Mbps has no rung in this config (available: %v) — following the clock", pin, s.schedule.Rungs())
			}
		}
		if member == applied {
			return
		}
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
		applied = member

		s.stateMu.Lock()
		s.state.BandwidthProfile = name
		s.state.BandwidthDownMbps = down
		s.stateMu.Unlock()
		_ = utils.WriteState(s.snapshot())
		log.Printf("bandwidth profile → %s (↓%d Mbps) via %s", name, down, member)
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
