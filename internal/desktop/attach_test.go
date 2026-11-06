package desktop

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestNormalizeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"9333":                     "http://127.0.0.1:9333",
		"127.0.0.1:9333":           "http://127.0.0.1:9333",
		"localhost:9333/":          "http://127.0.0.1:9333",
		"HTTP://LocalHost:9333":    "http://127.0.0.1:9333",
		" http://127.0.0.1:9333/ ": "http://127.0.0.1:9333",
		"http://[::1]:9333":        "http://[::1]:9333",
		"ws://127.0.0.1:9333/devtools/browser/AbC": "ws://127.0.0.1:9333/devtools/browser/AbC",
	} {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// closedEndpoint is a loopback endpoint nothing listens on, so a fake
// session's watch loop never reaches a real application.
func closedEndpoint(t *testing.T) (string, int) {
	t.Helper()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return loopbackEndpoint(port), port
}

func TestAttachReusesTheSessionOnTheSameEndpoint(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.MaxSessions = 1
	endpoint, port := closedEndpoint(t)
	first, err := m.add(&Session{Info: Info{Name: "app", Mode: "attach", Endpoint: endpoint}, generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(first.ID) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Other spellings of the same endpoint, at the session cap: reuse never
	// needs a free slot and never opens a connection.
	for _, spelling := range []string{endpoint, endpoint + "/", "localhost:" + strconv.Itoa(port), strconv.Itoa(port)} {
		got, reused, err := m.AttachOrReuse(ctx, AttachSpec{Endpoint: spelling})
		if err != nil {
			t.Fatalf("attach %q: %v", spelling, err)
		}
		if !reused || got != first {
			t.Fatalf("attach %q: got %p (reused %t), want session %s reused", spelling, got, reused, first.ID)
		}
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("%d sessions open, want 1", n)
	}
	if _, err := m.Get(""); err != nil {
		t.Fatalf("a tool without session must find the only one: %v", err)
	}
}

func TestAddUniqueKeepsTheFirstSessionOfARace(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	endpoint, port := closedEndpoint(t)
	first, reused, err := m.addUnique(&Session{Info: Info{Name: "a", Mode: "attach", Endpoint: endpoint}, generation: 1})
	if err != nil || reused {
		t.Fatalf("first add: reused %t, %v", reused, err)
	}
	t.Cleanup(func() { _, _ = m.Stop(first.ID) })
	loser := &Session{Info: Info{Name: "b", Mode: "attach", Endpoint: "http://localhost:" + strconv.Itoa(port)}, generation: 1}
	got, reused, err := m.addUnique(loser)
	if err != nil || !reused || got != first {
		t.Fatalf("second add: got %v reused %t err %v, want the first session", got, reused, err)
	}
	if loser.ID != "" || loser.stopWatch != nil {
		t.Fatalf("the losing session was registered: %+v", loser.Info)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("%d sessions open, want 1", n)
	}
}

func TestStatusCountsEveryReconnectAsARestart(t *testing.T) {
	// A reconnect that happens between two probes (the dev loop restarted
	// Electron on the same port within a second) never passes through the
	// restarting state; it must still be listed, or status reads the last
	// restart of an empty list.
	s := &Session{generation: 1, state: StateRunning}
	s.mu.Lock()
	for i := 0; i < maxRestartsKept+5; i++ {
		s.recordReconnectLocked()
	}
	s.mu.Unlock()
	st := s.Status()
	if st.Generation != maxRestartsKept+6 {
		t.Fatalf("generation %d, want %d", st.Generation, maxRestartsKept+6)
	}
	if len(st.Restarts) != maxRestartsKept {
		t.Fatalf("%d restarts kept, want the last %d", len(st.Restarts), maxRestartsKept)
	}
}
