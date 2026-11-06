package mcp

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
)

func TestFrameworkToolsFollowTheSessions(t *testing.T) {
	bridge := tauritest.Start(t, "tok")
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := s.Build()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := toolCaller{t: t, s: s, ctx: ctx}

	for _, name := range []string{"launch", "attach", "snapshot", "click", "eval", "run_flow", "trace"} {
		if srv.GetTool(name) == nil {
			t.Fatalf("common tool %s must always be listed", name)
		}
	}
	for _, name := range []string{"eval_main", "invoke", "emit"} {
		if srv.GetTool(name) != nil {
			t.Fatalf("%s must wait for a session of its framework", name)
		}
	}
	out := c.ok("attach", map[string]any{"endpoint": strconv.Itoa(bridge.Port()), "token": "tok"})
	if !strings.Contains(out, "Attached Fake App") || !strings.Contains(out, "tauri app") || !strings.Contains(out, "@1 b OK") {
		t.Fatalf("attach: %s", out)
	}
	if srv.GetTool("invoke") == nil || srv.GetTool("emit") == nil || srv.GetTool("eval_main") != nil {
		t.Fatal("a Tauri session lists invoke and emit only")
	}

	// The app restarts on the same port (a dev loop rebuild): the session
	// follows it and the tools keep working.
	bridge.DropConnections()
	var snap string
	var isErr bool
	for i := 0; i < 40; i++ {
		if snap, isErr = c.do("snapshot", nil); !isErr {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if isErr || !strings.Contains(snap, "@1 b OK") {
		t.Fatalf("snapshot after a restart: %s", snap)
	}
	if st := c.ok("status", nil); !strings.Contains(st, "restarts=1") {
		t.Fatalf("restart not counted: %s", st)
	}

	c.ok("stop", nil)
	if srv.GetTool("invoke") != nil || srv.GetTool("emit") != nil {
		t.Fatal("Tauri tools must go with the last Tauri session")
	}
}

func TestStaticToolsListsEverything(t *testing.T) {
	s, err := New(Options{ArtifactsRoot: t.TempDir(), Version: "test", StaticTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := s.Build()
	for _, name := range s.Tools() {
		if srv.GetTool(name) == nil {
			t.Errorf("static mode must list %s", name)
		}
	}
	// Without a session of its framework, a framework tool explains itself.
	c := toolCaller{t: t, s: s, ctx: context.Background()}
	if got := c.fails("invoke", map[string]any{"command": "x"}); !strings.Contains(got, "no session") && !strings.Contains(got, "launch") {
		t.Fatalf("invoke without a session: %s", got)
	}
}
