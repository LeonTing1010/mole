package core

import (
	"errors"
	"testing"
	"time"
)

func TestRulesRetryUnchangedFailureAndNewInput(t *testing.T) {
	now := time.Unix(100, 0)
	r := rulesRetry{observed: "old"}
	if r.due("old", now) || !r.due("new", now) {
		t.Fatal("only a changed source should initially be pending")
	}
	r.finish(now, errors.New("temporary write failure"))
	if r.due("new", now.Add(time.Second)) || !r.due("new", now.Add(2*time.Second)) {
		t.Fatal("unchanged failed input must retry after backoff")
	}
	r.finish(now.Add(2*time.Second), errors.New("still failing"))
	if r.due("new", now.Add(4*time.Second)) {
		t.Fatal("backoff must increase")
	}
	if !r.due("corrected", now.Add(4*time.Second)) {
		t.Fatal("new input must bypass stale backoff")
	}
	r.finish(now.Add(4*time.Second), nil)
	if r.due("corrected", now.Add(time.Hour)) {
		t.Fatal("success must not repeat")
	}
}

func TestRulesRetryBackoffIsCapped(t *testing.T) {
	now := time.Unix(100, 0)
	r := rulesRetry{pending: true}
	for i := 0; i < 20; i++ {
		r.finish(now, errors.New("invalid source"))
		if r.delay > time.Minute {
			t.Fatal("retry backoff exceeded cap")
		}
		now = r.next
	}
	if r.delay != time.Minute {
		t.Fatal(r.delay)
	}
}
