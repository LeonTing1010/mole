package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Quality / what: requested protocols define readiness, not an assigned IP.
func TestVerificationRequiresEveryRequestedProtocol(t *testing.T) {
	alive := legReport{Verdict: utils.ProbeAlive}
	silent := legReport{Verdict: utils.ProbeSilent}
	dual := Server{UUID: "u", RealityPubKey: "p", IP: "192.0.2.1", Port: 443}
	for _, tc := range []struct {
		name         string
		server       Server
		hy2, reality legReport
		want         string
	}{
		{"dual ready", dual, alive, alive, "ready"},
		{"partial", dual, alive, silent, "degraded"},
		{"none verified", dual, silent, silent, "unverified"},
		{"hy2 only", Server{}, alive, silent, "ready"},
		{"reality only", Server{Proto: protoReality}, silent, alive, "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verificationStatus(tc.server, tc.hy2, tc.reality); got != tc.want {
				t.Fatalf("status=%s want=%s", got, tc.want)
			}
		})
	}
}

// Safety / what: cancellation ends verification and cannot report success.
func TestVerificationDeadlineAndProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	result, err := verifyNodeWith(ctx, Server{}, nodeVerifyOptions{
		Writer: &out, Every: time.Millisecond, Heartbeat: 10 * time.Millisecond,
		Probe: func(ctx context.Context, leg string) legReport {
			<-ctx.Done()
			return legReport{Verdict: utils.ProbeSilent, Err: ctx.Err()}
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) || result.Status == "ready" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("verification exceeded cancellation budget")
	}
	if strings.Count(out.String(), "elapsed=") < 3 {
		t.Fatalf("no ongoing progress: %s", out.String())
	}
}

// Safety / what: a usable single leg is preserved but is not full deployment success.
func TestPartialVerificationReturnsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := verifyNodeWith(ctx, Server{UUID: "u", RealityPubKey: "p"}, nodeVerifyOptions{
		Every: time.Millisecond,
		Probe: func(ctx context.Context, leg string) legReport {
			if leg == "hy2" {
				return legReport{Verdict: utils.ProbeAlive}
			}
			return legReport{Verdict: utils.ProbeRefused}
		},
	})
	if err == nil || result.Status != "degraded" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// Quality / what: old records have no current verification evidence.
func TestServerAddressDoesNotImplyVerification(t *testing.T) {
	srv := Server{IP: "192.0.2.1"}
	if !srv.HasAddress() || srv.deployStatus() != "unverified" {
		t.Fatalf("unexpected status %s", srv.deployStatus())
	}
}
