package mcp

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// tauriFixture returns the fixture binary named by MCP_APP_DESKTOP_E2E_TAURI
// ("1": the debug build of examples/tauri-fixture), or skips.
func tauriFixture(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MCP_APP_DESKTOP_E2E_TAURI")
	if v == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_TAURI to the tauri-fixture binary (or 1 for examples/tauri-fixture/src-tauri/target/debug/tauri-fixture); build it with cargo build in examples/tauri-fixture/src-tauri")
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

// toolCaller calls tool handlers the way the MCP server does.
type toolCaller struct {
	t   *testing.T
	s   *Server
	ctx context.Context
}

func (c toolCaller) do(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	var def *toolDef
	for _, d := range c.s.tools() {
		if d.tool.Name == name {
			d := d
			def = &d
		}
	}
	if def == nil {
		c.t.Fatalf("no tool %s", name)
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := def.handler(c.s, c.ctx, req)
	if err != nil {
		c.t.Fatalf("%s: protocol error: %v", name, err)
	}
	var sb strings.Builder
	for _, ct := range res.Content {
		switch v := ct.(type) {
		case mcpgo.TextContent:
			sb.WriteString(v.Text)
		case mcpgo.ImageContent:
			sb.WriteString("[image " + v.MIMEType + " " + strconv.Itoa(len(v.Data)) + " b64 bytes]")
		}
	}
	c.t.Logf("%s %v → %s", name, args, truncate(sb.String(), 1200))
	return sb.String(), res.IsError
}

func (c toolCaller) ok(name string, args map[string]any) string {
	c.t.Helper()
	out, isErr := c.do(name, args)
	if isErr {
		c.t.Fatalf("%s failed: %s", name, out)
	}
	return out
}

func (c toolCaller) fails(name string, args map[string]any) string {
	c.t.Helper()
	out, isErr := c.do(name, args)
	if !isErr {
		c.t.Fatalf("%s should have failed: %s", name, out)
	}
	return out
}

func (c toolCaller) eval(expr string) string {
	c.t.Helper()
	return strings.TrimSpace(c.ok("eval", map[string]any{"expression": expr}))
}

func listed(s *Server, name string) bool {
	s.dynMu.Lock()
	srv := s.srv
	s.dynMu.Unlock()
	return srv.GetTool(name) != nil
}

// TestEndToEndTauri drives the Tauri fixture through its bridge plugin with
// every tool family, and checks that the tool list follows the sessions.
func TestEndToEndTauri(t *testing.T) {
	exe := tauriFixture(t)
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}

	if listed(s, "invoke") || listed(s, "emit") || listed(s, "eval_main") {
		t.Fatal("framework tools must not be listed without a session")
	}
	out := c.ok("launch", map[string]any{"target": exe})
	if !strings.Contains(out, "Launched Tauri Fixture as session") || !strings.Contains(out, "tauri app") || !strings.Contains(out, "bridge 127.0.0.1:") {
		t.Fatalf("launch: %s", out)
	}
	if !listed(s, "invoke") || !listed(s, "emit") || listed(s, "eval_main") {
		t.Fatal("a Tauri session lists invoke and emit, not eval_main")
	}
	if st := c.ok("status", nil); !strings.Contains(st, " tauri target state=running") || !strings.Contains(st, "bridge=127.0.0.1:") || !strings.Contains(st, "[main]") {
		t.Fatalf("status: %s", st)
	}

	snap := c.ok("snapshot", nil)
	for _, want := range []string{"h1 Tauri Fixture", "t Name", "t Email", "s Color", "@7 c ✗ Subscribe", "@3 s Color", "@4 option ✓ Red", "b Greet", "b Fail", "t Search", "b Open second window"} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot lacks %q:\n%s", want, snap)
		}
	}

	// Form, IPC through the page, and the IPC log.
	c.ok("type", map[string]any{"target": `getByLabel("Name")`, "text": "Ada"})
	diff := c.ok("click", map[string]any{"target": `getByRole("button", { name: "Greet" })`})
	if !strings.HasPrefix(diff, "clicked") {
		t.Fatalf("click: %s", diff)
	}
	c.ok("wait_for", map[string]any{"text": "Hello, Ada!", "timeout_ms": 5000})
	if logs := c.ok("logs", map[string]any{"source": "ipc"}); !strings.Contains(logs, "invoke greet") || !strings.Contains(logs, `"name":"Ada"`) {
		t.Fatalf("ipc log: %s", logs)
	}
	if logs := c.ok("logs", map[string]any{"source": "console"}); !strings.Contains(logs, "greeted Hello, Ada!") {
		t.Fatalf("console log: %s", logs)
	}
	c.ok("click", map[string]any{"target": "#fail"})
	time.Sleep(300 * time.Millisecond)
	if logs := c.ok("logs", map[string]any{"errors": true}); !strings.Contains(logs, "fail rejected") || !strings.Contains(logs, "boom") {
		t.Fatalf("error logs: %s", logs)
	}

	// Tauri tools.
	if got := c.ok("invoke", map[string]any{"command": "greet", "args": map[string]any{"name": "Bob"}}); !strings.Contains(got, "Hello, Bob!") {
		t.Fatalf("invoke: %s", got)
	}
	if got := c.fails("invoke", map[string]any{"command": "fail"}); !strings.Contains(got, "boom") {
		t.Fatalf("invoke fail: %s", got)
	}
	if got := c.ok("emit", map[string]any{"event": "fixture-ping", "payload": `{"n": 1}`}); !strings.Contains(got, "emitted fixture-ping") {
		t.Fatalf("emit: %s", got)
	}
	if got := c.fails("eval_main", map[string]any{"expression": "1"}); !strings.Contains(got, "Electron") {
		t.Fatalf("eval_main on Tauri: %s", got)
	}

	// Controls.
	c.ok("select", map[string]any{"target": `getByLabel("Color")`, "values": []any{"Blue"}})
	if v := c.eval("document.getElementById('color').value"); v != "blue" {
		t.Fatalf("select: %q", v)
	}
	c.ok("click", map[string]any{"target": `getByRole("checkbox", { name: "Subscribe" })`})
	if v := c.eval("document.getElementById('subscribe').checked"); v != "true" {
		t.Fatalf("checkbox: %q", v)
	}
	c.ok("type", map[string]any{"target": `getByRole("textbox", { name: "Search" })`, "text": "abc"})
	if v := c.eval("document.getElementById('live').textContent"); v != "abc" {
		t.Fatalf("input event: %q", v)
	}
	c.ok("type", map[string]any{"target": "#search", "text": "d", "append": true})
	if v := c.eval("document.getElementById('search').value"); v != "abcd" {
		t.Fatalf("append: %q", v)
	}
	c.ok("press", map[string]any{"target": "#search", "key": "Backspace"})
	if v := c.eval("document.getElementById('search').value"); v != "abc" {
		t.Fatalf("backspace: %q", v)
	}
	c.ok("press", map[string]any{"target": "#search", "key": "cmd+a"})
	c.ok("press", map[string]any{"key": "Backspace"})
	if v := c.eval("document.getElementById('search').value"); v != "" {
		t.Fatalf("select all + backspace: %q", v)
	}
	c.ok("press", map[string]any{"target": "#name", "key": "Tab"})
	if v := c.eval("document.activeElement.id"); v != "email" {
		t.Fatalf("tab moved focus to %q", v)
	}

	// A hover-only button (opacity 0 until :hover).
	if got := c.fails("click", map[string]any{"target": "#rows li:nth-child(2) .delete", "hover_first": false, "timeout_ms": 500}); !strings.Contains(got, "opacity") {
		t.Fatalf("hover_first false on a transparent button: %s", got)
	}
	if got := c.ok("click", map[string]any{"target": "#rows li:nth-child(2) .delete"}); !strings.Contains(got, "hidden by opacity") {
		t.Fatalf("hover-only click: %s", got)
	}
	if v := c.eval("document.querySelectorAll('#rows li').length"); v != "2" {
		t.Fatalf("row not deleted: %q", v)
	}
	// Ambiguity fails at once, like CDP.
	if got := c.fails("click", map[string]any{"target": `getByRole("button", { name: "Delete" })`}); !strings.Contains(got, "matched 2 elements") {
		t.Fatalf("ambiguous target: %s", got)
	}

	// Scrolling and an element below the fold.
	c.ok("scroll", map[string]any{"target": "#bottom"})
	c.ok("click", map[string]any{"target": `getByRole("button", { name: "Bottom" })`})
	if v := c.eval("document.getElementById('bottom-count').textContent"); v != "1" {
		t.Fatalf("bottom clicks: %q", v)
	}
	c.ok("scroll", map[string]any{"dy": -5000})
	c.ok("wheel", map[string]any{"x": 100, "y": 100, "dy": 300, "steps": 3, "interval_ms": 10})
	if v := c.eval("window.scrollY > 0"); v != "true" {
		t.Fatalf("wheel did not scroll: %q", v)
	}

	// eval: statements, values the native bridge cannot carry, errors,
	// `this`, async.
	if v := c.eval("const a = 2; a * 21"); v != "42" {
		t.Fatalf("statements: %q", v)
	}
	if v := c.eval("new Date(0)"); !strings.Contains(v, "1970") {
		t.Fatalf("a Date must come back serialised (and not crash the app): %q", v)
	}
	if v := c.eval("({a: [1, 2], b: NaN, c: undefined})"); v != `{"a":[1,2],"b":"NaN"}` {
		t.Fatalf("object: %q", v)
	}
	if v := c.eval("await new Promise(r => setTimeout(() => r('late'), 50))"); v != "late" {
		t.Fatalf("await: %q", v)
	}
	if got := c.fails("eval", map[string]any{"expression": "throw new Error('nope')"}); !strings.Contains(got, "nope") {
		t.Fatalf("throw: %s", got)
	}
	if got := c.ok("eval", map[string]any{"expression": "this.tagName", "target": "h1"}); strings.TrimSpace(got) != "H1" {
		t.Fatalf("this: %q", got)
	}
	if got := c.fails("eval", map[string]any{"expression": "await new Promise(() => {})", "timeout_ms": 300}); !strings.Contains(got, "no result within") {
		t.Fatalf("eval timeout: %s", got)
	}

	// JavaScript dialogs follow the session policy.
	if v := c.eval("confirm('sure?')"); v != "true" {
		t.Fatalf("confirm accepted: %q", v)
	}
	c.ok("dialog", map[string]any{"action": "dismiss"})
	if v := c.eval("confirm('sure?')"); v != "false" {
		t.Fatalf("confirm dismissed: %q", v)
	}
	c.ok("dialog", map[string]any{"action": "accept"})

	// Screenshots.
	if shot := c.ok("screenshot", nil); !strings.Contains(shot, "[image image/jpeg") || !strings.Contains(shot, "no webp encoder") {
		t.Fatalf("screenshot: %s", shot)
	}
	if shot := c.ok("screenshot", map[string]any{"target": "h1", "format": "png", "save": true, "name": "title"}); !strings.Contains(shot, "[image image/png") || !strings.Contains(shot, "title.png") {
		t.Fatalf("element screenshot: %s", shot)
	}
	c.fails("screenshot", map[string]any{"full_page": true})

	// A second window gets its own index.
	c.ok("scroll", map[string]any{"target": "#open-second"})
	c.ok("click", map[string]any{"target": "#open-second"})
	var wins string
	for i := 0; i < 50; i++ {
		if wins = c.ok("windows", nil); strings.Contains(wins, "[second]") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(wins, "window 0 [main]") || !strings.Contains(wins, "window 1 [second]") {
		t.Fatalf("windows: %s", wins)
	}
	if snap := c.ok("snapshot", map[string]any{"window": 1}); !strings.Contains(snap, "Second window") {
		t.Fatalf("second window snapshot: %s", snap)
	}

	// A flow, then a reload (the agent is installed again).
	c.ok("run_flow", map[string]any{"flow": "- tapOn:\n    id: name\n- eraseText: 10\n- inputText: Zoe\n- tapOn: Greet\n- assertVisible: Hello, Zoe!\n- evalJs: document.getElementById('out').textContent\n"})
	if r := c.ok("reload", nil); !strings.Contains(r, "reloaded") || !strings.Contains(r, "Tauri Fixture") {
		t.Fatalf("reload: %s", r)
	}
	if v := c.eval("document.getElementById('out').textContent"); v != "" {
		t.Fatalf("reload kept the page: %q", v)
	}

	stop := c.ok("stop", nil)
	if !strings.Contains(stop, "closed") {
		t.Fatalf("stop: %s", stop)
	}
	if listed(s, "invoke") {
		t.Fatal("invoke must be unlisted once no Tauri session is open")
	}
}

// TestEndToEndTauriAttach attaches to a fixture started by hand with a
// bridge port and token: stop detaches and leaves it running.
func TestEndToEndTauriAttach(t *testing.T) {
	exe := tauriFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "MCP_APP_DESKTOP_BRIDGE_PORT="+strconv.Itoa(port), "MCP_APP_DESKTOP_BRIDGE_TOKEN=s3cret")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}
	var out string
	var isErr bool
	for i := 0; i < 100; i++ {
		if out, isErr = c.do("attach", map[string]any{"endpoint": strconv.Itoa(port), "token": "wrong"}); isErr && strings.Contains(out, "token") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !isErr || !strings.Contains(out, "bridge listens") {
		t.Fatalf("a wrong token must be refused with a hint: %s", out)
	}
	out = c.ok("attach", map[string]any{"endpoint": strconv.Itoa(port), "token": "s3cret"})
	if !strings.Contains(out, "Attached Tauri Fixture") || !strings.Contains(out, "tauri app") {
		t.Fatalf("attach: %s", out)
	}
	if again := c.ok("attach", map[string]any{"endpoint": "127.0.0.1:" + strconv.Itoa(port), "token": "s3cret"}); !strings.Contains(again, "Already attached") {
		t.Fatalf("second attach must reuse the session: %s", again)
	}
	c.ok("snapshot", nil)
	c.ok("stop", nil)
	if cmd.ProcessState != nil {
		t.Fatal("stop must leave an attached app running")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the attached app is gone after stop: %v", err)
	}
}
