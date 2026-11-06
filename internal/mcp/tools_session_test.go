package mcp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// status panicked with « index out of range [-1] » on a session reconnected
// without a recorded restart (2026-10-24). Every shape a session can have
// mid-restart must render.
func TestStatusTextNeverPanics(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		st   desktop.Status
		wins []driver.WindowInfo
		err  error
		want string
	}{
		{"reconnected without a restart time", desktop.Status{Info: desktop.Info{ID: "s1"}, State: desktop.StateRunning, Generation: 3}, nil, nil, "restarts=2\n  no window open"},
		{"restart time known", desktop.Status{Info: desktop.Info{ID: "s1"}, State: desktop.StateRunning, Generation: 2, Restarts: []time.Time{now}}, []driver.WindowInfo{{Index: 0, Type: "page", Title: "App", URL: "app://x"}}, nil, "restarts=1 (last " + now.Format("15:04:05") + ")\n  window 0: \"App\" app://x"},
		{"restarting", desktop.Status{Info: desktop.Info{ID: "s7"}, State: desktop.StateRestarting, Generation: 1}, nil, nil, "state=restarting"},
		{"zero generation", desktop.Status{Info: desktop.Info{ID: "s1"}, State: desktop.StateRunning}, nil, errors.New("list targets: closed"), "windows: list targets: closed"},
	}
	for _, c := range cases {
		got := statusText(c.st, c.wins, c.err)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, got, c.want)
		}
	}
}

// chromeOn launches a private headless Chrome on a fixed debugging port; the
// returned stop kills it by its own pid.
func chromeOn(t *testing.T, port int) (stop func()) {
	t.Helper()
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("no Chrome/Chromium installed")
	}
	dir, err := os.MkdirTemp("", "mcp-app-desktop-restart-chrome-*")
	if err != nil {
		t.Fatal(err)
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).Delete("no-startup-window").UserDataDir(dir).Set("remote-debugging-port", strconv.Itoa(port))
	ws, err := l.Launch()
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("launch chrome: %v", err)
	}
	if u, err := url.Parse(ws); err != nil || !strings.HasSuffix(u.Host, ":"+strconv.Itoa(port)) {
		l.Kill()
		_ = os.RemoveAll(dir)
		t.Fatalf("chrome did not take port %d: %s", port, ws)
	}
	done := false
	stop = func() {
		if done {
			return
		}
		done = true
		l.Kill()
		l.Cleanup()
		_ = os.RemoveAll(dir)
	}
	t.Cleanup(stop)
	return stop
}

// TestAttachTwiceReusesAndFollowsARestart: a second attach to the same app
// returns the open session, and that session keeps working after the app is
// restarted on the same port, without a new attach.
func TestAttachTwiceReusesAndFollowsARestart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Chrome")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	stopFirst := chromeOn(t, port)

	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		for _, d := range s.tools() {
			if d.tool.Name != name {
				continue
			}
			req := mcpgo.CallToolRequest{}
			req.Params.Name = name
			req.Params.Arguments = args
			res, err := d.handler(s, ctx, req)
			if err != nil {
				t.Fatalf("%s: protocol error: %v", name, err)
			}
			var sb strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(mcpgo.TextContent); ok {
					sb.WriteString(tc.Text)
				}
			}
			return sb.String(), res.IsError
		}
		t.Fatalf("no tool %s", name)
		return "", true
	}
	ok := func(name string, args map[string]any) string {
		t.Helper()
		out, isErr := call(name, args)
		if isErr {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}

	first := ok("attach", map[string]any{"endpoint": "http://127.0.0.1:" + strconv.Itoa(port)})
	if !strings.Contains(first, "Attached") || !strings.Contains(first, "session s1") {
		t.Fatalf("first attach: %s", first)
	}
	second := ok("attach", map[string]any{"endpoint": "localhost:" + strconv.Itoa(port)})
	if !strings.Contains(second, "Already attached") || !strings.Contains(second, "session s1") {
		t.Fatalf("second attach did not reuse s1: %s", second)
	}
	if st := ok("status", nil); strings.Count(st, "cdp=") != 1 {
		t.Fatalf("status lists more than one session:\n%s", st)
	}
	ok("eval", map[string]any{"expression": "window.mark = 'first'"})

	// The app goes away and comes back on the same port.
	stopFirst()
	chromeOn(t, port)
	deadline := time.Now().Add(60 * time.Second)
	var out string
	for {
		var isErr bool
		out, isErr = call("eval", map[string]any{"expression": "typeof window.mark"})
		if !isErr {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never came back after the restart: %s", out)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if out != "undefined" {
		t.Fatalf("eval reached the old page (%q), not the restarted app", out)
	}
	st := ok("status", nil)
	if strings.Count(st, "cdp=") != 1 || !strings.Contains(st, "restarts=1") || !strings.Contains(st, "state=running") {
		t.Fatalf("status after the restart:\n%s", st)
	}

	// The restarted app's window is minimised by someone else: the session
	// kept it active (focus emulation applied again after the restart), so
	// a timer loop and animation frames still run at full rate.
	minimizeAll(t, ctx, "http://127.0.0.1:"+strconv.Itoa(port))
	got := ok("eval", map[string]any{"expression": `new Promise(done => {
		const t0 = performance.now(); let raf = 0, timers = 0;
		requestAnimationFrame(function f() { raf++; requestAnimationFrame(f); });
		(function loop() { timers++; if (performance.now() - t0 < 600) setTimeout(loop, 10); else done(document.visibilityState + " " + (raf > 10) + " " + (timers > 20)); })();
	})`, "timeout_ms": 5000})
	if got != "visible true true" {
		t.Fatalf("hidden window throttled after the restart: %q", got)
	}
	ok("stop", map[string]any{})
}

// minimizeAll minimises every window of the browser at endpoint through a
// connection of its own, as the user hiding the app would, and waits until
// the browser reports them minimised.
func minimizeAll(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	b, err := engine.Connect(ctx, endpoint, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	wins, err := b.Windows()
	if err != nil || len(wins) == 0 {
		t.Fatalf("windows: %v %v", wins, err)
	}
	for _, w := range wins {
		win, err := proto.BrowserGetWindowForTarget{TargetID: proto.TargetTargetID(w.TargetID)}.Call(b.RodBrowser())
		if err != nil {
			t.Fatal(err)
		}
		if err := (proto.BrowserSetWindowBounds{WindowID: win.WindowID, Bounds: &proto.BrowserBounds{WindowState: proto.BrowserWindowStateMinimized}}).Call(b.RodBrowser()); err != nil {
			t.Fatal(err)
		}
	}
}
