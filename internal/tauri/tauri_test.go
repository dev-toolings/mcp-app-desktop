package tauri_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestDialChecksTokenAndProtocol(t *testing.T) {
	b := tauritest.Start(t, "secret")
	if _, err := tauri.Dial(ctx(t), b.Addr(), "wrong"); err == nil || !strings.Contains(err.Error(), "wrong token") {
		t.Fatalf("wrong token: %v", err)
	}
	c, err := tauri.Dial(ctx(t), b.Addr(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Hello.AppName != "Fake App" || c.Hello.PID != 4242 {
		t.Fatalf("hello: %+v", c.Hello)
	}
	b.Protocol = 2
	if _, err := tauri.Dial(ctx(t), b.Addr(), "secret"); err == nil || !strings.Contains(err.Error(), "protocol 2") {
		t.Fatalf("protocol mismatch: %v", err)
	}
}

func TestDialRefusesNonLoopbackAndNothingListening(t *testing.T) {
	if _, err := tauri.Dial(ctx(t), "10.1.2.3:9333", ""); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback: %v", err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	if _, err := tauri.Dial(ctx(t), addr, ""); !errors.Is(err, tauri.ErrUnreachable) {
		t.Fatalf("closed port: %v", err)
	}
	if tauri.Probe(ctx(t), addr) {
		t.Fatal("probe of a closed port")
	}
}

func TestConcurrentCallsAndLostConnection(t *testing.T) {
	b := tauritest.Start(t, "")
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Call(ctx(t), "ping", nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx(t), "nope", nil); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command: %v", err)
	}
	b.DropConnections()
	deadline := time.Now().Add(2 * time.Second)
	for !c.Closed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !c.Closed() {
		t.Fatal("a dropped connection must be seen as closed")
	}
	if _, err := c.Call(ctx(t), "ping", nil); err == nil {
		t.Fatal("call on a closed connection must fail")
	}
	if c.Alive(100 * time.Millisecond) {
		t.Fatal("closed connection reported alive")
	}
}

func TestAppInstallsTheAgentAndSnapshots(t *testing.T) {
	b := tauritest.Start(t, "")
	b.Labels = []string{"main", "settings"}
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	app := tauri.NewApp(c, nil)
	wins, err := app.Windows(ctx(t))
	if err != nil || len(wins) != 2 || wins[0].Label != "main" || wins[0].Index != 0 || wins[1].Index != 1 {
		t.Fatalf("windows: %+v %v", wins, err)
	}
	if _, err := app.Window(ctx(t), 5); err == nil || !strings.Contains(err.Error(), "0 (main), 1 (settings)") {
		t.Fatalf("missing window: %v", err)
	}
	w, err := app.Window(ctx(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Snapshot(ctx(t), engine.LevelContent, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.InteractiveCount != 1 || res.Refs["@1"].Name != "OK" {
		t.Fatalf("snapshot: %+v", res)
	}
	if refs := w.Refs(); refs["@1"].Role != "button" {
		t.Fatalf("ref table: %+v", refs)
	}
	installs := 0
	for _, cmd := range b.Commands() {
		if cmd == "eval" {
			installs++
		}
	}
	// One run answered "install", one install, one run again.
	if installs != 3 {
		t.Fatalf("expected the agent to be installed once on demand, saw %d evals", installs)
	}
	if url, title, err := w.Info(ctx(t)); err != nil || url != "tauri://localhost/settings" || title != "Fake" {
		t.Fatalf("info: %q %q %v", url, title, err)
	}
}

func TestIndicesStayWithTheirWindow(t *testing.T) {
	b := tauritest.Start(t, "")
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	app := tauri.NewApp(c, nil)
	b.Labels = []string{"main", "second"}
	if _, err := app.Windows(ctx(t)); err != nil {
		t.Fatal(err)
	}
	b.Labels = []string{"second"} // main closed
	wins, err := app.Windows(ctx(t))
	if err != nil || len(wins) != 1 || wins[0].Index != 1 {
		t.Fatalf("a window keeps its index when another closes: %+v %v", wins, err)
	}
}

func TestInvokeAndEmitScripts(t *testing.T) {
	s, err := tauri.InvokeScript("greet", map[string]any{"name": "a\"b"})
	if err != nil || !strings.Contains(s, `I.invoke("greet", {"name":"a\"b"})`) {
		t.Fatalf("invoke script: %s %v", s, err)
	}
	s, err = tauri.EmitScript("evt", map[string]any{"n": 1})
	if err != nil || !strings.Contains(s, `'plugin:event|emit', { event: "evt", payload: {"n":1} }`) {
		t.Fatalf("emit script: %s %v", s, err)
	}
	if _, err := tauri.InvokeScript("x", map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unserialisable args must fail")
	}
}

func TestLogPollerReadsEveryWindow(t *testing.T) {
	b := tauritest.Start(t, "")
	b.Labels = []string{"main", "second"}
	b.Eval = nil
	c, err := tauri.Dial(ctx(t), b.Addr(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	app := tauri.NewApp(c, nil)
	p := tauri.NewLogPoller()
	if entries := p.Poll(ctx(t), app); len(entries) != 0 {
		t.Fatalf("no entries expected: %+v", entries)
	}
	var drains int
	for _, cmd := range b.Commands() {
		if cmd == "eval" {
			drains++
		}
	}
	if drains != 2 {
		t.Fatalf("one drain per window, saw %d", drains)
	}
	_ = json.Valid
}
