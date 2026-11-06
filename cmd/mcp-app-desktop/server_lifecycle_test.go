package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestE2EServerExitStopsLaunchedApp drives the real stdio server like an
// agent host: launch an app, then close stdin as a host does on exit. The
// app must not outlive the server. Opt-in through MCP_APP_DESKTOP_E2E_TARGET
// and MCP_APP_DESKTOP_E2E_ENV ("A=1,B=@tmp").
func TestE2EServerExitStopsLaunchedApp(t *testing.T) {
	target := os.Getenv("MCP_APP_DESKTOP_E2E_TARGET")
	if target == "" {
		t.Skip("set MCP_APP_DESKTOP_E2E_TARGET to an Electron project or .app")
	}
	env := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("MCP_APP_DESKTOP_E2E_ENV"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if v == "@tmp" {
				dir, err := os.MkdirTemp("/tmp", "mcp-e2e-server-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				v = dir
			}
			env[k] = v
		}
	}
	exe := filepath.Join(t.TempDir(), "mcp-app-desktop")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	srv := exec.Command(exe, "mcp", "--artifacts", t.TempDir())
	stdin, err := srv.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	reader := bufio.NewReaderSize(stdout, 16<<20)
	send := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	type rpc struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	recv := func(id int) rpc {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var r rpc
			if json.Unmarshal([]byte(line), &r) == nil && r.ID == id {
				if len(r.Error) > 0 {
					t.Fatalf("rpc %d error: %s", id, r.Error)
				}
				return r
			}
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}})
	recv(1)
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "launch", "arguments": map[string]any{"target": target, "env": env}}})
	res := recv(2)
	var call struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type, Text string
		} `json:"content"`
	}
	if err := json.Unmarshal(res.Result, &call); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, c := range call.Content {
		text.WriteString(c.Text)
	}
	if call.IsError {
		t.Fatalf("launch failed: %s", text.String())
	}
	m := regexp.MustCompile(`pid (\d+)`).FindStringSubmatch(text.String())
	if m == nil {
		t.Fatalf("no pid in launch result: %s", text.String())
	}
	pid, _ := strconv.Atoi(m[1])
	profile := regexp.MustCompile(`private profile: (\S+)`).FindStringSubmatch(text.String())
	if profile == nil {
		t.Fatalf("no profile in launch result: %s", text.String())
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("app %d not alive: %v", pid, err)
	}
	t.Logf("app pid %d, profile %s; closing the host's stdin", pid, profile[1])

	// The host goes away.
	_ = stdin.Close()
	select {
	case err := <-exited:
		if err != nil && !strings.Contains(err.Error(), "exit status") {
			t.Logf("server exit: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("server did not exit after stdin closed")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil && len(processesMentioning(profile[1])) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("app %d outlived the server", pid)
	}
	if left := processesMentioning(profile[1]); len(left) > 0 {
		t.Fatalf("helpers outlived the server: %v", left)
	}
	if _, err := os.Stat(profile[1]); !os.IsNotExist(err) {
		t.Fatalf("profile not removed: %v", err)
	}
	_, _ = io.Copy(io.Discard, stdout)
}

func processesMentioning(s string) []int {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.Contains(strings.Join(f[1:], " "), s) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

var _ = fmt.Sprintf
