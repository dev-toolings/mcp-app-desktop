// Package tauritest is a fake tauri-plugin-mcp-app-desktop bridge for tests
// that must not open a window: it speaks the protocol and answers the
// server's scripts with canned values (it runs no JavaScript).
package tauritest

import (
	"bufio"
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Bridge is a fake bridge listening on a loopback port.
type Bridge struct {
	Token    string
	Protocol int
	// Labels are the webviews it reports (default: main).
	Labels []string
	// Snapshot is the agent's answer to a snapshot (JSON).
	Snapshot string
	// Eval answers scripts the fake does not recognise (JSON string value
	// the webview would return, e.g. `"\"42\""`); nil answers `"{}"`.
	Eval func(label, script string) string

	l         net.Listener
	mu        sync.Mutex
	installed map[string]bool
	conns     []net.Conn
	Calls     []string // commands received, in order
}

// Start listens and serves until the test ends.
func Start(t *testing.T, token string) *Bridge {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{Token: token, Protocol: 1, Labels: []string{"main"}, l: l, installed: map[string]bool{},
		Snapshot: `{"nodes":[{"role":"heading","name":"Fake","level":1},{"role":"button","name":"OK","ref":"@1"}],"stats":{"total_nodes":2,"filtered_nodes":2,"interactive_count":1},"warnings":[],"url":"tauri://localhost","title":"Fake"}`}
	go b.serve()
	t.Cleanup(b.Close)
	return b
}

// Addr is 127.0.0.1:PORT.
func (b *Bridge) Addr() string { return b.l.Addr().String() }

// Port is the listening port.
func (b *Bridge) Port() int { return b.l.Addr().(*net.TCPAddr).Port }

// Close stops listening and drops every connection (the app went away).
func (b *Bridge) Close() {
	_ = b.l.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
}

// DropConnections closes the open connections but keeps listening (a
// dev loop restart on the same port).
func (b *Bridge) DropConnections() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.conns = nil
	b.installed = map[string]bool{}
}

// Commands returns the commands received so far.
func (b *Bridge) Commands() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.Calls...)
}

func (b *Bridge) serve() {
	for {
		c, err := b.l.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, c)
		b.mu.Unlock()
		go b.handle(c)
	}
}

var runOp = regexp.MustCompile(`A\.run\("([a-zA-Z]+)"`)

func (b *Bridge) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 1<<20)
	var wmu sync.Mutex
	authed := b.Token == ""
	write := func(v any) {
		data, _ := json.Marshal(v)
		wmu.Lock()
		_, _ = c.Write(append(data, '\n'))
		wmu.Unlock()
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req map[string]any
		if json.Unmarshal(line, &req) != nil {
			write(map[string]any{"id": 0, "ok": false, "error": "malformed request"})
			continue
		}
		id := req["id"]
		cmd, _ := req["cmd"].(string)
		b.mu.Lock()
		b.Calls = append(b.Calls, cmd)
		b.mu.Unlock()
		fail := func(msg string) { write(map[string]any{"id": id, "ok": false, "error": msg}) }
		ok := func(res any) { write(map[string]any{"id": id, "ok": true, "result": res}) }
		if !authed && cmd != "hello" {
			fail("authentication required: send hello with the token first")
			return
		}
		switch cmd {
		case "hello":
			if tok, _ := req["token"].(string); b.Token != "" && tok != b.Token {
				fail("authentication failed: wrong token")
				return
			}
			authed = true
			ok(map[string]any{"protocol": b.Protocol, "plugin_version": "0.0.0-fake", "tauri_version": "2.0.0", "app_name": "Fake App", "app_version": "1.0.0", "identifier": "dev.fake", "pid": 4242, "os": "macos"})
		case "ping":
			ok(map[string]any{})
		case "windows":
			var list []map[string]any
			for _, l := range b.Labels {
				list = append(list, map[string]any{"label": l, "url": "tauri://localhost/" + l, "title": "Fake " + l, "visible": true})
			}
			ok(list)
		case "eval":
			label, _ := req["label"].(string)
			script, _ := req["script"].(string)
			ok(map[string]any{"value": b.answer(label, script)})
		case "screenshot":
			fail("screenshot is not supported by the fake bridge")
		default:
			fail("unknown command " + cmd)
		}
	}
}

func jsonString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// answer returns the JSON text of the value a script evaluates to.
func (b *Bridge) answer(label, script string) string {
	switch {
	case strings.Contains(script, "page agent for the Tauri bridge"):
		b.mu.Lock()
		b.installed[label] = true
		b.mu.Unlock()
		return jsonString("installed")
	case strings.Contains(script, "document.readyState===\"complete\""):
		return jsonString("true")
	case strings.Contains(script, ".drain("):
		return jsonString(`{"boot":"b1","seq":0,"entries":[]}`)
	}
	if m := runOp.FindStringSubmatch(script); m != nil {
		b.mu.Lock()
		installed := b.installed[label]
		b.mu.Unlock()
		if !installed {
			return jsonString(`{"__install":true}`)
		}
		switch m[1] {
		case "snapshot":
			return jsonString(b.Snapshot)
		case "info":
			return jsonString(`{"url":"tauri://localhost/` + label + `","title":"Fake","ready":"complete","boot":"b1","w":800,"h":600,"dpr":2,"scroll":0,"mutated":1,"now":` + strconv.Itoa(1000) + `}`)
		}
		return jsonString(`{}`)
	}
	if b.Eval != nil {
		return b.Eval(label, script)
	}
	return jsonString(`{}`)
}
