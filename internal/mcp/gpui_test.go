package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
)

// TestGPUIToolsOverAFakeBridge checks what differs for a GPUI session: invoke
// (and not emit) is listed and calls the bridge's invoke, and everything
// needing JavaScript fails with what to do instead.
func TestGPUIToolsOverAFakeBridge(t *testing.T) {
	b := tauritest.StartGPUI(t, "")
	b.CommandNames = []string{"state"}
	b.Invoke = func(label, command string, args any) (any, error) {
		switch command {
		case "state":
			return map[string]any{"label": label, "args": args}, nil
		case "name":
			return "codescape", nil
		}
		return nil, errors.New(`no command "` + command + `" registered by the app (known: name, state)`)
	}
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}

	out := c.ok("attach", map[string]any{"endpoint": strconv.Itoa(b.Port())})
	if !strings.Contains(out, "Attached Fake GPUI") || !strings.Contains(out, "gpui app") || !strings.Contains(out, "@1 b OK") {
		t.Fatalf("attach: %s", out)
	}
	if !listed(s, "invoke") || listed(s, "emit") || listed(s, "eval_main") {
		t.Fatal("a GPUI session lists invoke only")
	}
	if st := c.ok("status", nil); !strings.Contains(st, " gpui attach state=running") {
		t.Fatalf("status: %s", st)
	}
	if got := c.ok("invoke", map[string]any{"command": "state", "args": map[string]any{"n": 1}}); !strings.Contains(got, `{"args":{"n":1},"label":"4294967297"}`) {
		t.Fatalf("invoke: %s", got)
	}
	if got := c.ok("invoke", map[string]any{"command": "name"}); !strings.HasSuffix(strings.TrimSpace(got), "\ncodescape") {
		t.Fatalf("a string answer comes back as is: %q", got)
	}
	if got := c.fails("invoke", map[string]any{"command": "nope"}); !strings.Contains(got, "known: name, state") {
		t.Fatalf("unknown command: %s", got)
	}
	if got := c.fails("eval", map[string]any{"expression": "1"}); !strings.Contains(got, "no JavaScript in a GPUI app") || !strings.Contains(got, "use invoke") || !strings.Contains(got, "registered: state") {
		t.Fatalf("eval: %s", got)
	}
	if got := c.fails("navigate", map[string]any{"url": "https://example.com"}); !strings.Contains(got, "not available in a GPUI app") {
		t.Fatalf("navigate: %s", got)
	}
	if got := c.fails("reload", nil); !strings.Contains(got, "not available in a GPUI app") {
		t.Fatalf("reload: %s", got)
	}
	if got := c.fails("emit", map[string]any{"event": "x"}); !strings.Contains(got, "GPUI app") || !strings.Contains(got, "invoke") {
		t.Fatalf("emit: %s", got)
	}
	if slices.Contains(b.Commands(), "eval") {
		t.Fatalf("no script may reach a GPUI app: %v", b.Commands())
	}
	c.ok("stop", nil)
	if listed(s, "invoke") {
		t.Fatal("invoke goes with the last Tauri or GPUI session")
	}
}

// gpuiApp returns the GPUI binary named by MCP_APP_DESKTOP_E2E_GPUI, or skips.
func gpuiApp(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MCP_APP_DESKTOP_E2E_GPUI")
	if v == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_GPUI to a GPUI binary built with gpui-mcp-app-desktop (e.g. codescape built with --features automation)")
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("GPUI app: %v", err)
	}
	return abs
}

var refOfTextbox = regexp.MustCompile(`(@\d+) t `)

// TestEndToEndGPUI launches a real GPUI app with its bridge (codescape: its
// data in a private profile named through CODESCAPE_PROFILE), types several
// lines into its textbox and reads them back, captures the window and stops.
func TestEndToEndGPUI(t *testing.T) {
	exe := gpuiApp(t)
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}

	out := c.ok("launch", map[string]any{"target": exe, "env": map[string]any{"CODESCAPE_PROFILE": "${MCP_APP_DESKTOP_PROFILE_DIR}"}})
	if !strings.Contains(out, "gpui app") || !strings.Contains(out, "bridge 127.0.0.1:") || !strings.Contains(out, "private profile: ") {
		t.Fatalf("launch: %s", out)
	}
	if !listed(s, "invoke") || listed(s, "emit") {
		t.Fatal("a GPUI session lists invoke, not emit")
	}
	if wins := c.ok("windows", nil); !strings.Contains(wins, "gpui://window/") {
		t.Fatalf("windows: %s", wins)
	}
	snap := c.ok("snapshot", nil)
	m := refOfTextbox.FindStringSubmatch(snap)
	if m == nil {
		t.Fatalf("no textbox in the snapshot:\n%s", snap)
	}
	text := "première ligne\nseconde ligne"
	c.ok("type", map[string]any{"target": m[1], "text": text})
	// wait_for text matches values too: the tools can tell the text is there.
	c.ok("wait_for", map[string]any{"text": "seconde ligne", "timeout_ms": 5000})
	// The rendered snapshot shows a named textbox by its name (all
	// frameworks), so the exact value, line break included, is read from the
	// snapshot's nodes.
	value := func() string {
		sess, err := s.mgr.Get("")
		if err != nil {
			t.Fatal(err)
		}
		w, err := sess.Window(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		res, err := w.Snapshot(ctx, engine.LevelFull, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range res.Refs {
			if n.Role == "textbox" {
				return n.Value
			}
		}
		t.Fatalf("no textbox in the snapshot: %+v", res.Nodes)
		return ""
	}
	if got := value(); got != text {
		t.Fatalf("textbox value: %q, want %q", got, text)
	}
	c.ok("press", map[string]any{"target": m[1], "key": "cmd+a"})
	c.ok("press", map[string]any{"key": "Backspace"})
	if got := value(); got != "" {
		t.Fatalf("select all + backspace left %q", got)
	}
	if shot := c.ok("screenshot", map[string]any{"format": "png", "save": true}); !strings.Contains(shot, "[image image/png") || !strings.Contains(shot, "saved to ") {
		t.Fatalf("screenshot: %s", shot)
	}
	if got := c.fails("eval", map[string]any{"expression": "1"}); !strings.Contains(got, "no JavaScript in a GPUI app") {
		t.Fatalf("eval: %s", got)
	}
	if logs := c.ok("logs", map[string]any{"contains": "bridge listening"}); !strings.Contains(logs, "bridge listening on 127.0.0.1:") {
		t.Fatalf("process output in logs: %s", logs)
	}
	// stop fails when a process survives; the app and its profile are gone.
	if stop := c.ok("stop", nil); !strings.Contains(stop, "session s1 closed") {
		t.Fatalf("stop: %s", stop)
	}
	pid, _ := strconv.Atoi(launchPID.FindStringSubmatch(out)[1])
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("pid %d still runs after stop", pid)
	}
	if _, err := os.Stat(launchProfile.FindStringSubmatch(out)[1]); !os.IsNotExist(err) {
		t.Fatalf("private profile left after stop: %v", err)
	}
}

// TestEndToEndGPUIDevCommand launches the same app as a dev command whose
// framework is not given: the bridge's hello makes it a GPUI session.
func TestEndToEndGPUIDevCommand(t *testing.T) {
	exe := gpuiApp(t)
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}
	out := c.ok("launch", map[string]any{"command": []any{exe}, "cwd": t.TempDir(), "env": map[string]any{"CODESCAPE_PROFILE": "${MCP_APP_DESKTOP_PROFILE_DIR}"}})
	if !strings.Contains(out, "gpui app") || !strings.Contains(out, "GPUI dev command") || !strings.Contains(out, " t ") {
		t.Fatalf("launch: %s", out)
	}
	if !listed(s, "invoke") || listed(s, "emit") {
		t.Fatal("a GPUI dev command lists invoke, not emit")
	}
	if st := c.ok("status", nil); !strings.Contains(st, " gpui command state=running") || strings.Contains(st, "inspector=") {
		t.Fatalf("status: %s", st)
	}
	c.ok("stop", nil)
}

var (
	launchPID     = regexp.MustCompile(`pid (\d+)`)
	launchProfile = regexp.MustCompile(`private profile: (\S+)`)
)
