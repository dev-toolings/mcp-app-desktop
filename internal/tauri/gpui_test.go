package tauri_test

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
)

func gpuiApp(t *testing.T, b *tauritest.Bridge) (*tauri.App, driver.Window) {
	t.Helper()
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if !c.GPUI() || c.Hello.Framework != tauri.FrameworkGPUI || c.BridgeCrate() != tauri.GPUIBridgeCrate {
		t.Fatalf("hello of a GPUI bridge: %+v", c.Hello)
	}
	app := tauri.NewApp(c, nil)
	w, err := app.Window(ctx(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	return app, w
}

func TestGPUIRunsAgentOperationsNatively(t *testing.T) {
	b := tauritest.StartGPUI(t, "")
	var clicks atomic.Int32
	b.Agent = func(label, op string, args map[string]any) any {
		if label != "4294967297" {
			return map[string]any{"error": "wrong label " + label, "code": "error"}
		}
		switch op {
		case "click":
			// Not there for the first two tries: the server waits, as for Tauri.
			if clicks.Add(1) < 3 {
				return map[string]any{"error": "getByRole matched no element", "code": "notfound"}
			}
			return map[string]any{"note": ""}
		case "hover":
			return map[string]any{"error": "CSS selector \".x\": a GPUI window has no CSS", "code": "invalid"}
		}
		return map[string]any{}
	}
	app, w := gpuiApp(t, b)
	app.WaitFirstPaint(ctx(t), 5*time.Second)

	res, err := w.Snapshot(ctx(t), engine.LevelContent, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Refs["@1"].Name != "OK" {
		t.Fatalf("snapshot through the agent command: %+v", res.Nodes)
	}
	if _, err := w.Click(ctx(t), `getByRole("button", { name: "OK" })`, driver.ClickOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("a notfound answer must be retried until it succeeds: %v", err)
	}
	if n := clicks.Load(); n != 3 {
		t.Fatalf("click attempts: %d", n)
	}
	start := time.Now()
	err = w.Hover(ctx(t), ".x", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no CSS") || time.Since(start) > 2*time.Second {
		t.Fatalf("an invalid answer fails at once with the agent's message: %v after %s", err, time.Since(start))
	}
	if u, _, err := w.Info(ctx(t)); err != nil || u != "gpui://window/4294967297" {
		t.Fatalf("info: %q %v", u, err)
	}
	if slices.Contains(b.Commands(), "eval") {
		t.Fatalf("no script may be sent to a GPUI app: %v", b.Commands())
	}
	if ops := b.Ops(); !slices.Contains(ops, "snapshot") || !slices.Contains(ops, "click") {
		t.Fatalf("agent operations: %v", ops)
	}
}

func TestGPUIRefusesJavaScriptAndNavigation(t *testing.T) {
	b := tauritest.StartGPUI(t, "")
	b.CommandNames = []string{"state", "open_session"}
	app, w := gpuiApp(t, b)

	_, err := w.Eval(ctx(t), "document.title", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "no JavaScript in a GPUI app") || !strings.Contains(err.Error(), "registered: state, open_session") {
		t.Fatalf("eval: %v", err)
	}
	for name, err := range map[string]error{
		"navigate": w.Navigate(ctx(t), "https://example.com", "load"),
		"history":  w.History(ctx(t), -1, "load"),
		"reload":   w.Reload(ctx(t), false),
		"upload":   w.Upload(ctx(t), "@1", []string{"/nonexistent"}, time.Second),
	} {
		if err == nil || !strings.Contains(err.Error(), "GPUI app") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := w.Screenshot(ctx(t), driver.ShotOptions{FullPage: true}); err == nil || !strings.Contains(err.Error(), "GPUI bridge") {
		t.Fatalf("full_page: %v", err)
	}
	if got := tauri.NewLogPoller().Poll(ctx(t), app); got != nil {
		t.Fatalf("a GPUI app has no console to poll: %v", got)
	}
	if slices.Contains(b.Commands(), "eval") {
		t.Fatalf("no script may be sent to a GPUI app: %v", b.Commands())
	}
}

func TestGPUIInvokeCallsRegisteredCommands(t *testing.T) {
	b := tauritest.StartGPUI(t, "")
	b.CommandNames = []string{"echo"}
	b.Invoke = func(label, command string, args any) (any, error) {
		if command != "echo" {
			return nil, errors.New(`no command "` + command + `" registered by the app (known: echo)`)
		}
		return map[string]any{"label": label, "args": args}, nil
	}
	app, _ := gpuiApp(t, b)
	raw, err := app.Invoke(ctx(t), "4294967297", "echo", map[string]any{"n": 1})
	if err != nil || string(raw) != `{"args":{"n":1},"label":"4294967297"}` {
		t.Fatalf("invoke: %s %v", raw, err)
	}
	if _, err := app.Invoke(ctx(t), "", "nope", nil); err == nil || !strings.Contains(err.Error(), "known: echo") {
		t.Fatalf("unknown command: %v", err)
	}
	names, err := app.Commands(ctx(t))
	if err != nil || len(names) != 1 || names[0] != "echo" {
		t.Fatalf("commands: %v %v", names, err)
	}
}

func TestTauriAppIsNotGPUI(t *testing.T) {
	b := tauritest.Start(t, "")
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	app := tauri.NewApp(c, nil)
	if c.GPUI() || app.GPUI() || c.BridgeCrate() != tauri.TauriPluginCrate {
		t.Fatalf("a Tauri hello names no framework: %+v", c.Hello)
	}
	if _, err := app.Invoke(ctx(t), "main", "greet", nil); err == nil {
		t.Fatal("bridge invoke is GPUI only")
	}
}
