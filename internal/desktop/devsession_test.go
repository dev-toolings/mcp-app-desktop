package desktop

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDevKeyNamesTheCommandAndDirectory(t *testing.T) {
	a := devKey("/p", []string{"bun", "run", "dev"}, true)
	if a != devKey("/p", []string{"bun", "run", "dev"}, true) {
		t.Fatal("the same command must give the same key")
	}
	for _, other := range []string{devKey("/q", []string{"bun", "run", "dev"}, true), devKey("/p", []string{"bun", "run", "start"}, true), devKey("/p", []string{"bun", "run", "dev"}, false), devKey("/p", []string{"bun", "run dev"}, true)} {
		if other == a {
			t.Fatalf("distinct commands share key %s", a)
		}
	}
}

func TestDevStateRoundTripAndRecycledPid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dev commands are not adopted on Windows")
	}
	path := filepath.Join(t.TempDir(), "k.json")
	cmd := exec.Command("sleep", "30")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var root string
	for i := 0; i < 50 && root == ""; i++ {
		root = rootCommand(cmd.Process.Pid)
		time.Sleep(20 * time.Millisecond)
	}
	st := &devState{Name: "dev", PID: cmd.Process.Pid, PGID: cmd.Process.Pid, RootCmd: root, CDPPort: 9222}
	if err := writeDevState(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := readDevState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.stillOurs() {
		t.Fatalf("the running process was not recognised: %+v", got)
	}
	got.RootCmd = "something else"
	if got.stillOurs() {
		t.Fatal("a pid running another command was adopted")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	got.RootCmd = root
	if got.stillOurs() {
		t.Fatal("an exited process was adopted")
	}
}

func TestTailFileFollowsAppendsFromTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ring := NewRing(10)
	tail := tailFile(path, -4, ring.Writer("stdout"))
	defer tail.close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("new line\n")
	f.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(ring.Tail(5), "new line") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := ring.Tail(5); !strings.Contains(got, "new line") || strings.Contains(got, "old line") {
		t.Fatalf("tail from the end: %q", got)
	}
}

func TestTailFileStartsAtALineStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.log")
	if err := os.WriteFile(path, []byte("first line\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for offset, want := range map[int64]string{-6: "third", -9: "third", -13: "second"} {
		ring := NewRing(10)
		tail := tailFile(path, offset, ring.Writer("stdout"))
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(ring.Tail(5), want) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		tail.close()
		got := ring.Tail(5)
		if !strings.Contains(got, want) || strings.Contains(got, "line") || (want == "third" && strings.Contains(got, "ond")) {
			t.Errorf("offset %d: %q, want whole lines from %q", offset, got, want)
		}
	}
}
