package desktop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
)

// Opt-in scenarios against a real Electron app, like internal/mcp's e2e test:
// MCP_APP_DESKTOP_E2E_TARGET (project dir or .app) and MCP_APP_DESKTOP_E2E_ENV
// ("A=1,B=@tmp"; "@tmp" becomes a fresh directory per test).
func e2eSpec(t *testing.T) (string, map[string]string) {
	t.Helper()
	target := os.Getenv("MCP_APP_DESKTOP_E2E_TARGET")
	if target == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_TARGET to an Electron project or .app")
	}
	env := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("MCP_APP_DESKTOP_E2E_ENV"), ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if v == "@tmp" {
			dir, err := os.MkdirTemp(tempRoot(), "mcp-e2e-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			v = dir
		}
		env[k] = v
	}
	return target, env
}

func waitState(t *testing.T, s *Session, want State, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.State() == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("state %s not reached within %s (now %s, last error %q)", want, d, s.State(), s.Status().LastError)
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("%s: not within %s", what, d)
}

// processesMentioning lists live processes whose command line contains s.
func processesMentioning(s string) []int {
	var out []int
	for _, e := range ps() {
		if strings.Contains(e.cmd, s) {
			out = append(out, e.pid)
		}
	}
	return out
}

func pageTitle(t *testing.T, sess *Session, window int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, page, err := sess.Page(ctx, window)
	if err != nil {
		t.Fatalf("page %d: %v", window, err)
	}
	v, err := page.Eval(`() => document.title`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return v.Value.Str()
}

func TestE2EAttachLeavesTheAppRunning(t *testing.T) {
	target, env := e2eSpec(t)
	tgt, err := Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	cdp, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	insp, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := os.MkdirTemp(tempRoot(), "mcp-e2e-attach-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	args := append([]string{"--remote-debugging-port=" + strconv.Itoa(cdp), "--inspect=127.0.0.1:" + strconv.Itoa(insp), "--user-data-dir=" + profile}, tgt.Args...)
	// The app is started outside the manager, like a developer would.
	p, err := startOwned(tgt.Executable, args, mergeEnv(env, ports{CDP: cdp, Inspect: insp}), tgt.Dir, NewRing(200), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.terminate() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	waitFor(t, "debugging port", 60*time.Second, func() bool { _, ok := probeCDP(ctx, loopbackEndpoint(cdp)); return ok })

	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.Attach(ctx, AttachSpec{Endpoint: strconv.Itoa(cdp), Inspector: loopbackEndpoint(insp)})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if sess.Owned || sess.PID != 0 {
		t.Fatalf("an attached session must not own the app: %+v", sess.Info)
	}
	waitFor(t, "first window", 30*time.Second, func() bool {
		b, err := sess.Browser(ctx)
		if err != nil {
			return false
		}
		w, _ := b.Windows()
		return len(w) > 0
	})
	if title := pageTitle(t, sess, 0); title == "" {
		t.Fatal("renderer unreachable through the attached session")
	}
	if sess.Main() == nil {
		t.Fatal("inspector not connected on attach")
	}
	if v, err := sess.Main().Eval(ctx, "app.isReady()", 5*time.Second); err != nil || v != "true" {
		t.Fatalf("eval_main through attach: %q %v", v, err)
	}
	rep, err := m.Stop(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Terminated {
		t.Fatal("stop of an attached session must not terminate the app")
	}
	time.Sleep(time.Second)
	if p.exited() {
		t.Fatal("the app died when the attached session was closed")
	}
	if _, ok := probeCDP(ctx, loopbackEndpoint(cdp)); !ok {
		t.Fatal("the app's debugging port stopped answering after detach")
	}
	survivors := p.terminate()
	if len(survivors) > 0 {
		t.Fatalf("survivors after terminate: %v", survivors)
	}
}

func TestE2ECrashIsDetectedAndCleanedUp(t *testing.T) {
	target, env := e2eSpec(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := m.Launch(ctx, LaunchSpec{Target: target, Env: env, Isolate: true, Inspect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(sess.ID) })
	pageTitle(t, sess, 0)
	profile := sess.Profile
	if len(processesMentioning(profile)) == 0 {
		t.Fatal("the app does not run with the private profile")
	}
	// Simulate a hard crash of the main process.
	if err := syscall.Kill(sess.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitState(t, sess, StateExited, 15*time.Second)
	if _, err := sess.Browser(ctx); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("tools must learn the app exited, got %v", err)
	}
	st := sess.Status()
	if !strings.Contains(st.LastError, "killed") {
		t.Fatalf("exit reason not recorded: %+v", st)
	}
	rep, err := m.Stop(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Survivors) > 0 {
		t.Fatalf("survivors: %v", rep.Survivors)
	}
	waitFor(t, "helpers gone", 15*time.Second, func() bool { return len(processesMentioning(profile)) == 0 })
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile not removed: %v", err)
	}
}

func TestE2EDevLoopRestartReattaches(t *testing.T) {
	target, env := e2eSpec(t)
	tgt, err := Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	profile, err := os.MkdirTemp(tempRoot(), "mcp-e2e-devloop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	// A minimal dev loop: relaunch Electron whenever it exits, passing the
	// ports the server chose through the documented variables.
	script := filepath.Join(dir, "dev.sh")
	body := fmt.Sprintf(`#!/bin/sh
while :; do
  %q --remote-debugging-port="$MCP_APP_DESKTOP_CDP_PORT" --inspect=127.0.0.1:"$MCP_APP_DESKTOP_INSPECT_PORT" --user-data-dir=%q %s
  echo "[dev] electron exited, restarting"
  sleep 1
done
`, tgt.Executable, profile, strings.Join(tgt.Args, " "))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := m.Launch(ctx, LaunchSpec{Command: []string{"/bin/sh", script}, Dir: tgt.Dir, Env: env, Inspect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(sess.ID) })
	pageTitle(t, sess, 0)
	port := strings.TrimPrefix(sess.Endpoint, "http://127.0.0.1:")
	mains := func() []int {
		var out []int
		for _, e := range ps() {
			if strings.Contains(e.cmd, "--remote-debugging-port="+port) && strings.HasPrefix(e.cmd, tgt.Executable) {
				out = append(out, e.pid)
			}
		}
		return out
	}
	first := mains()
	if len(first) != 1 {
		t.Fatalf("expected one Electron main process, got %v", first)
	}
	// The "recompile": the dev loop kills Electron and starts it again.
	if err := syscall.Kill(first[0], syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitState(t, sess, StateRestarting, 15*time.Second)
	if _, err := sess.Browser(ctx); err == nil || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("tools must report the restart, got %v", err)
	}
	waitState(t, sess, StateRunning, 90*time.Second)
	st := sess.Status()
	if st.Generation != 2 || len(st.Restarts) != 1 {
		t.Fatalf("restart not counted: %+v", st)
	}
	waitFor(t, "window after restart", 30*time.Second, func() bool {
		b, err := sess.Browser(ctx)
		if err != nil {
			return false
		}
		w, _ := b.Windows()
		return len(w) > 0
	})
	if title := pageTitle(t, sess, 0); title == "" {
		t.Fatal("renderer unreachable after the restart")
	}
	second := mains()
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("expected a new Electron main process, got %v then %v", first, second)
	}
	if sess.Main() == nil {
		t.Fatal("inspector not reconnected after the restart")
	}
	rep, err := m.Stop(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Survivors) > 0 {
		t.Fatalf("survivors: %v", rep.Survivors)
	}
	waitFor(t, "dev loop and app gone", 15*time.Second, func() bool {
		return len(processesMentioning(profile)) == 0 && len(processesMentioning(script)) == 0
	})
}

func TestE2ESecondWindowIsDrivenSeparately(t *testing.T) {
	target, env := e2eSpec(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := m.Launch(ctx, LaunchSpec{Target: target, Env: env, Isolate: true, Inspect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(sess.ID) })
	pageTitle(t, sess, 0)
	main := sess.Main()
	if main == nil {
		t.Fatal("inspector not connected")
	}
	const html = `data:text/html,<title>Second</title><button onclick="document.title='Clicked'">Press me</button>`
	if _, err := main.Eval(ctx, `const w = new BrowserWindow({width: 400, height: 300}); w.loadURL(`+strconv.Quote(html)+`); 'opened'`, 10*time.Second); err != nil {
		t.Fatalf("open second window: %v", err)
	}
	b, err := sess.Browser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := -1
	waitFor(t, "second window listed", 20*time.Second, func() bool {
		wins, err := b.Windows()
		if err != nil {
			return false
		}
		for _, w := range wins {
			if strings.HasPrefix(w.URL, "data:") {
				second = w.Index
			}
		}
		return len(wins) == 2 && second >= 0
	})
	if second == 0 {
		t.Fatal("the first window must keep index 0")
	}
	_, page, err := sess.Page(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Extract(page, engine.LevelSkeleton, "")
	if err != nil || !strings.Contains(engine.FormatTextProfile(res, engine.ProfileAgent("text")), "Press me") {
		t.Fatalf("second window snapshot: %v", err)
	}
	el, err := engine.ResolveTarget(page, `getByRole("button", { name: "Press me" })`, nil)
	if err != nil {
		t.Fatal(err)
	}
	el.MustClick()
	waitFor(t, "click landed in window 2", 10*time.Second, func() bool { return pageTitle(t, sess, second) == "Clicked" })
	if pageTitle(t, sess, 0) == "Clicked" {
		t.Fatal("the click reached the first window")
	}
	if _, err := main.Eval(ctx, `BrowserWindow.getAllWindows().find(w => w.webContents.getURL().startsWith('data:')).close(); 'closed'`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second window gone", 20*time.Second, func() bool { w, _ := b.Windows(); return len(w) == 1 })
	if _, _, err := sess.Page(ctx, second); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("closed window must be reported missing, got %v", err)
	}
	if pageTitle(t, sess, 0) == "" {
		t.Fatal("first window lost after closing the second")
	}
}
