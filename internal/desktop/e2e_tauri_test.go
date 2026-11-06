package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
)

// tauriFixture returns the fixture binary named by MCP_APP_DESKTOP_E2E_TAURI
// ("1": the debug build of examples/tauri-fixture), or skips.
func tauriFixture(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MCP_APP_DESKTOP_E2E_TAURI")
	if v == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_TAURI to the tauri-fixture binary (or 1)")
	}
	if v == "1" {
		v = filepath.Join("..", "..", "examples", "tauri-fixture", "src-tauri", "target", "debug", "tauri-fixture")
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("tauri fixture: %v", err)
	}
	return abs
}

func fixtureTitle(t *testing.T, sess *Session) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := sess.Window(ctx, 0)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	v, err := w.Eval(ctx, "document.title", "", 5*time.Second)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return v
}

// TestE2ETauriDevLoopRestartAndAdoption runs the fixture under a command
// that relaunches it whenever it exits, like tauri dev after a rebuild. The
// framework is told by the bridge answering; a kill of the app is followed
// (restarting, then running again on the same bridge); a new server adopts
// the command left running; stop ends it.
func TestE2ETauriDevLoopRestartAndAdoption(t *testing.T) {
	exe := tauriFixture(t)
	root := filepath.Join(t.TempDir(), "sessions")
	dir := t.TempDir()
	script := `while true; do "` + exe + `"; sleep 0.3; done`
	spec := LaunchSpec{Command: []string{"sh", "-c", script}, Dir: dir, StartupTimeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_, _ = m.Stop(sess.ID)
		}
	}()
	if sess.Framework != FrameworkTauri || sess.Backend != driver.BackendBridge || sess.Mode != "command" {
		t.Fatalf("detected %s/%s/%s", sess.Framework, sess.Backend, sess.Mode)
	}
	if !strings.Contains(sess.Note, "Tauri dev command") {
		t.Fatalf("note: %s", sess.Note)
	}
	if got := fixtureTitle(t, sess); got != "Tauri Fixture" {
		t.Fatalf("title %q", got)
	}

	// A rebuild: the app exits, the loop starts a new one on the same port.
	apps := processesMentioning(exe)
	var app int
	for _, pid := range apps {
		if pid != sess.PID {
			app = pid
		}
	}
	if app == 0 {
		t.Fatalf("fixture process not found among %v", apps)
	}
	if err := syscall.Kill(app, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restart noticed", 10*time.Second, func() bool {
		st := sess.Status()
		return st.Generation >= 2 || st.State == StateRestarting
	})
	waitState(t, sess, StateRunning, 30*time.Second)
	waitFor(t, "reconnected", 30*time.Second, func() bool { return sess.Status().Generation >= 2 })
	if got := fixtureTitle(t, sess); got != "Tauri Fixture" {
		t.Fatalf("title after restart %q", got)
	}

	// The server goes away: the dev command keeps running and a new server
	// adopts it (same process, same bridge).
	pid, endpoint := sess.PID, sess.Endpoint
	m.Close()
	if len(processesMentioning(exe)) == 0 {
		t.Fatal("closing the server must release the dev command, not stop it")
	}
	m2, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := m2.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	sess = adopted
	m = m2
	if adopted.PID != pid || adopted.Endpoint != endpoint || adopted.Backend != driver.BackendBridge || !strings.Contains(adopted.Note, "adopted") {
		t.Fatalf("adopted %+v (want pid %d endpoint %s)", adopted.Info, pid, endpoint)
	}
	if got := fixtureTitle(t, adopted); got != "Tauri Fixture" {
		t.Fatalf("title after adoption %q", got)
	}
	ctx2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	w, err := adopted.Window(ctx2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := w.Snapshot(ctx2, engine.LevelSkeleton, ""); err != nil || res.Stats.InteractiveCount == 0 {
		t.Fatalf("snapshot after adoption: %v %+v", err, res)
	}

	rep, err := m2.Stop(adopted.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopped = true
	if !rep.Terminated {
		t.Fatalf("stop: %+v", rep)
	}
	waitFor(t, "fixture gone", 15*time.Second, func() bool { return len(processesMentioning(exe)) == 0 })
}
