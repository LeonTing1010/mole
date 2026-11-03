package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Safety / what-x-what: concurrent deployment and daemon writes preserve peers.
func TestServerTransactionsPreserveConcurrentAdditions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := updateServersAt(context.Background(), path, func(servers []Server) ([]Server, error) {
				return append(servers, Server{Name: fmt.Sprint(i), InstanceID: fmt.Sprint(i)}), nil
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	servers, err := readServersAt(path)
	if err != nil || len(servers) != 24 {
		t.Fatalf("lost updates: count=%d err=%v", len(servers), err)
	}
}

// Safety / what: shutdown waits for work and cleanup, and reports cleanup failure.
func TestInterruptWaitsForCleanupAndPreservesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	workEnded, cleaned := false, false
	want := errors.New("delete not confirmed")
	err := runDeployLifecycle(ctx, func() error { cancel(); workEnded = true; return ctx.Err() }, func() error {
		if !workEnded {
			t.Fatal("cleanup raced with work")
		}
		cleaned = true
		return want
	})
	if !cleaned || !errors.Is(err, want) || !errors.Is(err, ErrDeployInterrupted) {
		t.Fatalf("cleanup=%v err=%v", cleaned, err)
	}
}

// Safety / what: a verification timeout is not consent to destroy an instance.
func TestVerificationTimeoutDoesNotDestroyInstance(t *testing.T) {
	cleaned := false
	err := runDeployLifecycle(context.Background(), func() error { return context.DeadlineExceeded }, func() error { cleaned = true; return nil })
	if cleaned || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
}
