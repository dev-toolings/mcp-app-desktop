package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// TestEndToEndElectron drives a real Electron application through the tool
// handlers. Opt in with MCP_APP_DESKTOP_E2E_TARGET=<project dir|.app>; extra
// application env in MCP_APP_DESKTOP_E2E_ENV ("A=1,B=2"); an optional flow in
// MCP_APP_DESKTOP_E2E_FLOW.
func TestEndToEndElectron(t *testing.T) {
	target := os.Getenv("MCP_APP_DESKTOP_E2E_TARGET")
	if target == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_TARGET to an Electron project or .app")
	}
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	call := func(name string, args map[string]any) string {
		t.Helper()
		var def *toolDef
		for _, d := range s.tools() {
			if d.tool.Name == name {
				d := d
				def = &d
			}
		}
		if def == nil {
			t.Fatalf("no tool %s", name)
		}
		req := mcpgo.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := def.handler(s, ctx, req)
		if err != nil {
			t.Fatalf("%s: protocol error: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			switch v := c.(type) {
			case mcpgo.TextContent:
				sb.WriteString(v.Text)
			case mcpgo.ImageContent:
				sb.WriteString("[image " + v.MIMEType + " " + strconv.Itoa(len(v.Data)) + " b64 bytes]")
			}
		}
		out := sb.String()
		if res.IsError {
			t.Fatalf("%s failed: %s", name, out)
		}
		t.Logf("%s → %s", name, truncate(out, 1500))
		return out
	}

	env := map[string]any{}
	for _, kv := range strings.Split(os.Getenv("MCP_APP_DESKTOP_E2E_ENV"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	out := call("launch", map[string]any{"target": target, "env": env})
	if !strings.Contains(out, "Launched") {
		t.Fatalf("unexpected launch output: %s", out)
	}
	if !listed(s, "eval_main") || listed(s, "invoke") || listed(s, "emit") {
		t.Fatal("an Electron session lists eval_main, not invoke or emit")
	}
	call("status", nil)
	wins := call("windows", nil)
	if !strings.Contains(wins, "window 0") {
		t.Fatalf("no window listed: %s", wins)
	}
	snap := call("snapshot", map[string]any{"level": "content"})
	if !strings.Contains(snap, "[window 0]") {
		t.Fatalf("snapshot header missing: %s", snap)
	}
	title := call("eval", map[string]any{"expression": "document.title"})
	t.Logf("title: %s", title)
	mainOut := call("eval_main", map[string]any{"expression": "({name: app.getName(), windows: BrowserWindow.getAllWindows().length, userData: app.getPath('userData'), channels: ipcMain.eventNames().length})"})
	var mainInfo map[string]any
	if err := json.Unmarshal([]byte(mainOut), &mainInfo); err != nil {
		t.Fatalf("eval_main did not return JSON: %s", mainOut)
	}
	if mainInfo["windows"].(float64) < 1 {
		t.Fatalf("main reports no window: %s", mainOut)
	}
	shot := call("screenshot", map[string]any{"save": true, "name": "e2e"})
	if !strings.Contains(shot, "[image image/webp") {
		t.Fatalf("no image returned: %s", shot)
	}
	call("logs", map[string]any{"last": 20})
	call("press", map[string]any{"key": "Escape"})
	if flowFile := os.Getenv("MCP_APP_DESKTOP_E2E_FLOW"); flowFile != "" {
		abs, _ := filepath.Abs(flowFile)
		call("run_flow", map[string]any{"file": abs})
	}
	stop := call("stop", nil)
	if !strings.Contains(stop, "closed") {
		t.Fatalf("stop: %s", stop)
	}
	if listed(s, "eval_main") {
		t.Fatal("eval_main must be unlisted once no Electron session is open")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
