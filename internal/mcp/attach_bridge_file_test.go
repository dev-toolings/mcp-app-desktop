package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// A dev script that opens the bridge itself writes its port and token to a
// private file. attach reads that file: the token never appears in the call,
// in its answer or in the trace.
func TestAttachReadsTheBridgeFile(t *testing.T) {
	const token = "tok-8f2c1e7a9b"
	bridge := tauritest.Start(t, token)
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}
	dir := t.TempDir()
	write := func(name string, mode os.FileMode, content any) string {
		t.Helper()
		raw, ok := content.(string)
		if !ok {
			b, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(b)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(raw), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	endpoint := "127.0.0.1:" + strconv.Itoa(bridge.Port())
	good := write("bridge.json", 0o600, map[string]string{"endpoint": endpoint, "token": token})

	refused := func(what string, args map[string]any, want string) {
		t.Helper()
		out, isErr := c.do("attach", args)
		if !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: want an error containing %q, got (error=%t) %s", what, want, isErr, out)
		}
		if strings.Contains(out, token) {
			t.Fatalf("%s: the refusal shows the token: %s", what, out)
		}
		if len(s.mgr.List()) != 0 {
			t.Fatalf("%s: a refused attach must open no session", what)
		}
	}
	refused("neither", map[string]any{}, "endpoint")
	refused("both", map[string]any{"bridge_file": good, "endpoint": endpoint}, "bridge_file alone")
	refused("file and token", map[string]any{"bridge_file": good, "token": token}, "bridge_file alone")
	refused("missing", map[string]any{"bridge_file": filepath.Join(dir, "absent.json")}, "is the app running")
	refused("directory", map[string]any{"bridge_file": dir}, "not a regular file")
	refused("not JSON", map[string]any{"bridge_file": write("raw.json", 0o600, token)}, "is not JSON")
	refused("no endpoint", map[string]any{"bridge_file": write("empty.json", 0o600, map[string]string{"token": token})}, "has no endpoint")
	if runtime.GOOS != "windows" {
		refused("readable by others", map[string]any{"bridge_file": write("open.json", 0o644, map[string]string{"endpoint": endpoint, "token": token})}, "0600")
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(good, link); err != nil {
			t.Fatal(err)
		}
		refused("symbolic link", map[string]any{"bridge_file": link}, "not a regular file")
	}
	refused("wrong token in the file", map[string]any{"bridge_file": write("stale.json", 0o600, map[string]string{"endpoint": endpoint, "token": "other"})}, "connection failed")

	out := c.ok("attach", map[string]any{"bridge_file": good})
	if !strings.Contains(out, "Attached Fake App") || !strings.Contains(out, "tauri app") {
		t.Fatalf("attach: %s", out)
	}
	if strings.Contains(out, token) {
		t.Fatalf("the answer shows the token: %s", out)
	}
	// The same file again is the same application: the session is reused.
	if out := c.ok("attach", map[string]any{"bridge_file": good}); !strings.Contains(out, "Already attached") {
		t.Fatalf("second attach: %s", out)
	}
}

func TestTraceArgsKeepNoSecret(t *testing.T) {
	got := traceArgs("attach", map[string]any{"endpoint": "52988", "token": "39ebdd085cd3f085", "name": "app"})
	if got["token"] != traceRedacted || got["endpoint"] != "52988" || got["name"] != "app" {
		t.Fatalf("attach args: %v", got)
	}
	got = traceArgs("launch", map[string]any{
		"command": []any{"bun", "run", "dev"},
		"env":     map[string]any{"PATH": "/usr/bin", "GITHUB_TOKEN": "ghp_x", "App_Secret": "s", "OPENAI_API_KEY": "sk-x", "ESCAPE_DEV_PORT": "5297", "DB_PASSWORD": "p"},
	})
	env, ok := got["env"].(map[string]any)
	if !ok {
		t.Fatalf("env: %v", got["env"])
	}
	for _, name := range []string{"GITHUB_TOKEN", "App_Secret", "OPENAI_API_KEY", "DB_PASSWORD"} {
		if env[name] != traceRedacted {
			t.Errorf("%s = %v, want it redacted", name, env[name])
		}
	}
	if env["PATH"] != "/usr/bin" || env["ESCAPE_DEV_PORT"] != "5297" {
		t.Errorf("ordinary variables must stay readable: %v", env)
	}
	// Another tool's map argument is not an environment.
	fields := map[string]any{"token": "kept: a form field, typed text is the caller's choice"}
	if got := traceArgs("fill_form", map[string]any{"fields": fields}); got["fields"].(map[string]any)["token"] != fields["token"] {
		t.Errorf("only env is filtered by variable name: %v", got)
	}
}

func TestStuckAfterFollowsALaunchStartupTimeout(t *testing.T) {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"startup_timeout_ms": 120000}
	if got := stuckAfter(20*time.Second, req); got != 120*time.Second+traceStuckMargin {
		t.Fatalf("stuckAfter = %s: a launch given two minutes is not stuck at 20 s", got)
	}
	req.Params.Arguments = map[string]any{"timeout_ms": 30000, "startup_timeout_ms": 120000}
	if got := stuckAfter(20*time.Second, req); got != 30*time.Second+traceStuckMargin {
		t.Fatalf("stuckAfter = %s: timeout_ms comes first", got)
	}
	req.Params.Arguments = map[string]any{}
	if got := stuckAfter(20*time.Second, req); got != 20*time.Second {
		t.Fatalf("stuckAfter = %s", got)
	}
}
