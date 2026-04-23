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

// Supervisor keeps sing-box alive and toggles the "auto" selector between
// the VPS proxy outbound and a black-hole "block" outbound based on VPS health.
type Supervisor struct {
	configPath  string
	clashAddr   string
	probeURL    string
	probeEvery  time.Duration
	probeTimeMs int
	failThresh  int
	okThresh    int

	clash *ClashClient

	stateMu sync.Mutex
	state   *utils.State

	stopCh chan struct{}
	doneCh chan struct{}
}

// SupervisorOpts controls the supervisor's probe cadence. Zero values get sane defaults.
type SupervisorOpts struct {
	ClashAddr   string
	ProbeURL    string
	ProbeEvery  time.Duration
	ProbeTimeMs int
	FailThresh  int
	OkThresh    int
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
		opts.ProbeEvery = 5 * time.Second
	}
	if opts.ProbeTimeMs == 0 {
		opts.ProbeTimeMs = 3000
	}
	if opts.FailThresh == 0 {
		opts.FailThresh = 3
	}
	if opts.OkThresh == 0 {
		opts.OkThresh = 3
	}
	return &Supervisor{
		configPath:  configPath,
		clashAddr:   opts.ClashAddr,
		probeURL:    opts.ProbeURL,
		probeEvery:  opts.ProbeEvery,
		probeTimeMs: opts.ProbeTimeMs,
		failThresh:  opts.FailThresh,
		okThresh:    opts.OkThresh,
		clash:       NewClashClient(opts.ClashAddr),
		state: &utils.State{
			Mode:      utils.ModeProxy,
			PID:       os.Getpid(),
			Server:    serverName,
			StartedAt: time.Now(),
		},
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
	_ = utils.WriteState(s.snapshot())

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A: keep sing-box alive.
	go func() {
		defer wg.Done()
		defer recoverPanic("singbox-lifecycle")
		s.runLifecycle(ctx)
	}()

	// Goroutine B: probe VPS health and flip the selector.
	go func() {
		defer wg.Done()
		defer recoverPanic("health-probe")
		s.runProbe(ctx)
	}()

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
			// Race: process already gone. Loop to restart path below.
			exit = closedChan()
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-exit:
			// sing-box exited unexpectedly.
			runFor := time.Since(startedAt)
			if runFor > 30*time.Second {
				backoff = time.Second // reset if it was stable
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
				continue // backoff already grew; retry
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
	delay, err := s.clash.TestDelay("proxy", s.probeURL, s.probeTimeMs)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.state.LastProbeAt = time.Now()
	if err != nil {
		s.state.ConsecutiveFails++
		s.state.ConsecutiveOK = 0
		s.state.LastLatencyMs = 0
		s.state.LastProbeError = err.Error()
	} else {
		s.state.ConsecutiveOK++
		s.state.ConsecutiveFails = 0
		s.state.LastLatencyMs = delay
		s.state.LastProbeError = ""
	}

	// Decide mode switch.
	target := s.state.Mode
	switch s.state.Mode {
	case utils.ModeProxy:
		if s.state.ConsecutiveFails >= s.failThresh {
			target = utils.ModeBlock
		}
	case utils.ModeBlock:
		if s.state.ConsecutiveOK >= s.okThresh {
			target = utils.ModeProxy
		}
	}

	if target != s.state.Mode {
		option := "proxy"
		if target == utils.ModeBlock {
			option = "block"
		}
		if err := s.clash.SwitchSelector("auto", option); err != nil {
			log.Printf("switch selector auto→%s failed: %v", option, err)
		} else {
			log.Printf("mode: %s → %s (%s)", s.state.Mode, target, reasonFor(target, s.state))
			s.state.Mode = target
		}
	}

	_ = utils.WriteState(s.stateSnapshotLocked())
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

func reasonFor(target utils.Mode, st *utils.State) string {
	if target == utils.ModeBlock {
		return fmt.Sprintf("%d consecutive probe failures", st.ConsecutiveFails)
	}
	return fmt.Sprintf("%d consecutive probe successes (%dms)", st.ConsecutiveOK, st.LastLatencyMs)
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
