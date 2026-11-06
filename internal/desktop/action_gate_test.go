package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestActionGateAnswersBusyInsteadOfQueueing(t *testing.T) {
	s := &Session{Info: Info{ID: "s1"}}
	release, err := s.AcquireActions(context.Background(), "type", time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if tool, _ := s.Busy(); tool != "type" {
		t.Fatalf("Busy() = %q, want type", tool)
	}

	start := time.Now()
	_, err = s.AcquireActions(context.Background(), "click", 50*time.Millisecond)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("second acquire: %v, want *BusyError", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s, want about the 50ms budget", waited)
	}
	if busy.Session != "s1" || busy.Tool != "type" || !strings.Contains(busy.Error(), "type has been running") {
		t.Fatalf("busy error = %+v (%v)", busy, busy)
	}

	release()
	release() // a second release must not unlock someone else's hold
	again, err := s.AcquireActions(context.Background(), "click", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if tool, _ := s.Busy(); tool != "click" {
		t.Fatalf("Busy() = %q, want click", tool)
	}
	_, err = s.AcquireActions(context.Background(), "press", 20*time.Millisecond)
	if !errors.As(err, &busy) {
		t.Fatalf("double release freed the gate: %v", err)
	}
	again()
	if tool, _ := s.Busy(); tool != "" {
		t.Fatalf("Busy() = %q after release, want idle", tool)
	}
}

func TestActionGateHonoursCancellation(t *testing.T) {
	s := &Session{Info: Info{ID: "s1"}}
	release, err := s.AcquireActions(context.Background(), "wait_for", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AcquireActions(ctx, "click", time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v, want context.Canceled", err)
	}
}
