package mcp

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// toolsTestPage has per-row buttons shown only on hover, several matches of
// one selector, and a button whose name a regex locator targets.
const toolsTestPage = `<!doctype html><title>tools</title><style>
	.row { display:flex; gap:8px; padding:8px; width:320px; }
	.row .del { visibility:hidden; }
	.row:hover .del { visibility:visible; }
	.row .menu { display:none; }
	.row:hover .menu { display:inline-block; }
</style><body>
	<h1 id="title">Tools</h1>
	<div class="row" id="r1"><span>First</span><button class="del" onclick="window.clicked='del'">Delete</button></div>
	<div class="row" id="r3"><span>Third</span><button class="menu" onclick="window.clicked='menu'">Menu</button></div>
	<div class="item">a</div><div class="item">b</div><div class="item">c</div>
	<button onclick="window.clicked='mode'">Mode : Auto</button>
</body>`

// startHeadlessChrome launches a private headless Chrome and returns its
// debugging endpoint; it is killed (by its own pid) when the test ends.
func startHeadlessChrome(t *testing.T) string {
	t.Helper()
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("no Chrome/Chromium installed")
	}
	dir, err := os.MkdirTemp("", "mcp-app-desktop-tools-chrome-*")
	if err != nil {
		t.Fatal(err)
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).Delete("no-startup-window").UserDataDir(dir)
	ws, err := l.Launch()
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("launch chrome: %v", err)
	}
	t.Cleanup(func() {
		l.Kill()
		l.Cleanup()
		_ = os.RemoveAll(dir)
	})
	u, err := url.Parse(ws)
	if err != nil {
		t.Fatal(err)
	}
	return "http://" + u.Host
}

// startNode runs a plain Node process with an inspector (stand-in for the
// Electron main process) and returns its endpoint.
func startNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--inspect=127.0.0.1:0", "-e", "setInterval(() => {}, 1 << 30)")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	found := make(chan string, 1)
	go func() {
		re := regexp.MustCompile(`ws://(127\.0\.0\.1:\d+)/`)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				found <- "http://" + m[1]
				break
			}
		}
		for sc.Scan() {
		}
	}()
	select {
	case ep := <-found:
		return ep
	case <-time.After(10 * time.Second):
		t.Fatal("node inspector did not start")
	}
	return ""
}

// TestToolsAgainstHeadlessChrome drives the tool handlers on an attached
// headless Chrome (renderer) and a Node inspector (main process).
func TestToolsAgainstHeadlessChrome(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Chrome and Node")
	}
	endpoint := startHeadlessChrome(t)
	inspector := startNode(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(toolsTestPage))
	}))
	defer site.Close()

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
	fails := func(name string, args map[string]any, want string) {
		t.Helper()
		out, isErr := call(name, args)
		if !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s %v: want an error containing %q, got (error=%t) %s", name, args, want, isErr, out)
		}
	}

	ok("attach", map[string]any{"endpoint": endpoint, "inspector_endpoint": inspector})
	ok("navigate", map[string]any{"url": site.URL})
	clicked := func(want string) {
		t.Helper()
		if got := ok("eval", map[string]any{"expression": "window.clicked"}); got != want {
			t.Fatalf("window.clicked = %q, want %q", got, want)
		}
		ok("eval", map[string]any{"expression": "window.clicked = ''"})
	}
	away := func() { ok("hover", map[string]any{"target": "#title", "snapshot": "none"}) }

	// 3. Hover-only buttons.
	away()
	out := ok("click", map[string]any{"target": "#r1 .del", "snapshot": "none"})
	if !strings.Contains(out, "revealed by hovering") {
		t.Fatalf("click summary does not mention the hover: %s", out)
	}
	clicked("del")
	away()
	fails("click", map[string]any{"target": "#r1 .del", "hover_first": false, "timeout_ms": 500, "snapshot": "none"}, "state=visible")
	fails("click", map[string]any{"target": "#r3 .menu", "timeout_ms": 500, "snapshot": "none"}, "visible")
	away()
	out = ok("click", map[string]any{"target": "#r3 .menu", "hover_first": true, "snapshot": "none"})
	if !strings.Contains(out, "hidden by display") {
		t.Fatalf("click summary: %s", out)
	}
	clicked("menu")

	// 4. hidden with several visible matches.
	fails("wait_for", map[string]any{"target": ".item", "state": "hidden", "timeout_ms": 400}, "3 matching element(s) still visible")
	ok("eval", map[string]any{"expression": "document.querySelectorAll('.item').forEach(e => e.remove())"})
	ok("wait_for", map[string]any{"target": ".item", "state": "hidden", "timeout_ms": 1000})

	// 5. Regex name.
	ok("click", map[string]any{"target": `getByRole("button", { name: /^Mode : / })`, "snapshot": "none"})
	clicked("mode")
	ok("wait_for", map[string]any{"target": "role=button name=/^mode : auto$/i", "timeout_ms": 1000})

	// 2. Renderer eval timeout.
	fails("eval", map[string]any{"expression": "new Promise(() => {})", "timeout_ms": 300}, "timeout_ms=300")
	if got := ok("eval", map[string]any{"expression": "new Promise(r => setTimeout(() => r('slow'), 400))", "timeout_ms": 3000}); got != "slow" {
		t.Fatalf("slow eval: %q", got)
	}

	// 1. Main-process eval of IIFEs and async functions.
	if got := ok("eval_main", map[string]any{"expression": "(()=>{const {join}=require('path'); globalThis.stub = join('a','b'); return 'stubbed'})()"}); got != "stubbed" {
		t.Fatalf("eval_main iife: %q", got)
	}
	if got := ok("eval_main", map[string]any{"expression": "(async()=>{ await new Promise(r => setTimeout(r, 10)); return JSON.stringify({stub: globalThis.stub}) })()"}); got != `{"stub":"a/b"}` {
		t.Fatalf("eval_main async iife: %q", got)
	}
	fails("eval_main", map[string]any{"expression": "(async () => { throw new Error('nope') })()"}, "nope")

	ok("stop", map[string]any{})
}
