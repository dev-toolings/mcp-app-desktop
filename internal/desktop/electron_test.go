package desktop

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExpandPorts(t *testing.T) {
	pp := ports{CDP: 9222, Inspect: 9229, Bridge: 9333}
	got := expandPorts("--remote-debugging-port=${MCP_APP_DESKTOP_CDP_PORT} --inspect=127.0.0.1:$MCP_APP_DESKTOP_INSPECT_PORT", pp)
	want := "--remote-debugging-port=9222 --inspect=127.0.0.1:9229"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Scripts written for mcp-electron keep working.
	if got := expandPorts("--remote-debugging-port=$MCP_ELECTRON_CDP_PORT --inspect=${MCP_ELECTRON_INSPECT_PORT}", pp); got != "--remote-debugging-port=9222 --inspect=9229" {
		t.Fatalf("legacy names: %q", got)
	}
	if got := expandPorts("bridge=${MCP_APP_DESKTOP_BRIDGE_PORT}", pp); got != "bridge=9333" {
		t.Fatalf("bridge port: %q", got)
	}
}

func TestMergeEnvExportsPortsAndDropsRunAsNode(t *testing.T) {
	t.Setenv("ELECTRON_RUN_AS_NODE", "1")
	t.Setenv("KEEP_ME", "yes")
	t.Setenv("MCP_ELECTRON_CDP_PORT", "1111") // stale, from the developer's shell
	env := mergeEnv(map[string]string{"APP_DATA": "/tmp/x", "KEEP_ME": "override"}, ports{CDP: 9222, Inspect: 9229, Bridge: 9333, Token: "tok"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"APP_DATA=/tmp/x", "KEEP_ME=override", EnvCDPPort + "=9222", EnvInspectPort + "=9229", "MCP_ELECTRON_CDP_PORT=9222", "MCP_ELECTRON_INSPECT_PORT=9229", "MCP_APP_DESKTOP_BRIDGE_PORT=9333", "MCP_APP_DESKTOP_BRIDGE_TOKEN=tok"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "ELECTRON_RUN_AS_NODE") {
		t.Error("ELECTRON_RUN_AS_NODE must be dropped")
	}
	if strings.Count(joined, "KEEP_ME=") != 1 {
		t.Error("overridden variable must appear once")
	}
	if strings.Contains(joined, "=1111") {
		t.Error("an inherited port variable must be replaced, not kept")
	}
}

func TestAllocPortsGivesDistinctPortsAndAToken(t *testing.T) {
	pp, err := allocPorts(true)
	if err != nil {
		t.Fatal(err)
	}
	if pp.CDP == 0 || pp.Bridge == 0 || pp.Inspect == 0 || len(pp.Token) != 32 {
		t.Fatalf("%+v", pp)
	}
	if pp.CDP == pp.Bridge || pp.CDP == pp.Inspect || pp.Bridge == pp.Inspect {
		t.Fatalf("ports must differ: %+v", pp)
	}
	if pp, _ := allocPorts(false); pp.Inspect != 0 {
		t.Fatal("no inspector port unless asked")
	}
}

func TestFrameworkOf(t *testing.T) {
	if got := frameworkOf(versionInfo{Browser: "Chrome/130.0", UserAgent: "Mozilla/5.0 … Electron/33.0.0 …"}); got != FrameworkElectron {
		t.Fatalf("electron: %s", got)
	}
	if got := frameworkOf(versionInfo{Browser: "Edg/130.0.2849.68", UserAgent: "Mozilla/5.0 … Edg/130.0"}); got != FrameworkTauri {
		t.Fatalf("webview2: %s", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTauriProject(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src-tauri", "tauri.conf.json"), `{"productName": "Demo App", "identifier": "dev.demo"}`)
	writeFile(t, filepath.Join(root, "src-tauri", "Cargo.toml"), "[package]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(root, "package.json"), `{"name": "demo", "scripts": {"tauri": "tauri"}}`)
	// Without the plugin it fails at once with the setup, not after a build
	// and the startup timeout (Windows drives WebView2 and needs none).
	if _, err := Resolve(root); runtime.GOOS != "windows" && (err == nil || !strings.Contains(err.Error(), "does not include tauri-plugin-mcp-app-desktop") || !strings.Contains(err.Error(), "debug_assertions")) {
		t.Fatalf("a project without the plugin: %v", err)
	}
	writeFile(t, filepath.Join(root, "src-tauri", "Cargo.toml"), "[package]\nname = \"demo\"\n[dependencies]\ntauri-plugin-mcp-app-desktop = { path = \"../x\" }\n")
	writeFile(t, filepath.Join(root, "bun.lock"), "")
	for _, dir := range []string{root, filepath.Join(root, "src-tauri")} {
		tg, err := Resolve(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if tg.Framework != FrameworkTauri || tg.Name != "Demo App" || tg.Dir != root || strings.Join(tg.DevCommand, " ") != "bun run tauri dev" {
			t.Fatalf("%s: %+v", dir, tg)
		}
	}
	// Without a tauri script, the project's own CLI through its package manager.
	writeFile(t, filepath.Join(root, "package.json"), `{"name": "demo"}`)
	_ = os.Remove(filepath.Join(root, "bun.lock"))
	writeFile(t, filepath.Join(root, "pnpm-lock.yaml"), "")
	writeFile(t, filepath.Join(root, "node_modules", ".bin", "tauri"), "#!/bin/sh\n")
	tg, err := Resolve(root)
	if err != nil || strings.Join(tg.DevCommand, " ") != "pnpm exec tauri dev" {
		t.Fatalf("pnpm: %+v %v", tg, err)
	}
}

func TestResolveExecutableRecognisesTauri(t *testing.T) {
	dir := t.TempDir()
	tauriExe := filepath.Join(dir, "tauri-app")
	writeFile(t, tauriExe, strings.Repeat("x", 3<<20)+"window.__TAURI_INTERNALS__ = {}"+strings.Repeat("y", 1000))
	tg, err := Resolve(tauriExe)
	if err != nil || tg.Framework != FrameworkTauri || !tg.Packaged {
		t.Fatalf("tauri exe: %+v %v", tg, err)
	}
	other := filepath.Join(dir, "other-app")
	writeFile(t, other, "#!/bin/sh\necho hi\n")
	if tg, err := Resolve(other); err != nil || tg.Framework != FrameworkElectron {
		t.Fatalf("plain exe: %+v %v", tg, err)
	}
}

// chunkReader returns its data a few bytes at a time.
type chunkReader struct {
	data []byte
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.n, len(p), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestContainsStreamAcrossReads(t *testing.T) {
	data := []byte("aaaa__TAURI_INTERNALS__bbbb")
	for n := 1; n <= len(data); n++ {
		if !containsStream(&chunkReader{data: append([]byte(nil), data...), n: n}, tauriMarker) {
			t.Fatalf("missed the marker with reads of %d bytes", n)
		}
	}
	if containsStream(&chunkReader{data: []byte("__TAURI_INTERNAL_"), n: 3}, tauriMarker) {
		t.Fatal("false positive")
	}
}

func TestResolveProjectRequiresElectron(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(dir); err == nil || !strings.Contains(err.Error(), "package.json") {
		t.Fatalf("expected a package.json error, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","productName":"Demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dir); err == nil || !strings.Contains(err.Error(), "node_modules/electron") {
		t.Fatalf("expected a missing electron error, got %v", err)
	}
	bin := electronBinaryPathFor(dir)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tgt, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Name != "Demo" || tgt.Dir != dir || len(tgt.Args) != 1 || tgt.Args[0] != "." || tgt.Packaged {
		t.Fatalf("unexpected target %+v", tgt)
	}
}

func electronBinaryPathFor(dir string) string {
	// Mirror electronBinary's layout for the current OS.
	defer func() { _ = recover() }()
	candidates := []string{
		filepath.Join(dir, "node_modules", "electron", "dist", "Electron.app", "Contents", "MacOS", "Electron"),
		filepath.Join(dir, "node_modules", "electron", "dist", "electron.exe"),
		filepath.Join(dir, "node_modules", "electron", "dist", "electron"),
	}
	for _, c := range candidates {
		_ = os.MkdirAll(filepath.Dir(c), 0o755)
		_ = os.WriteFile(c, []byte("#!/bin/sh\n"), 0o755)
	}
	return electronBinary(dir)
}

func TestResolveBundleRejectsNonElectron(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Demo.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>Demo</string><key>CFBundleIdentifier</key><string>dev.demo</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(app); err == nil || !strings.Contains(err.Error(), "Electron Framework") {
		t.Fatalf("expected a non-Electron error, got %v", err)
	}
	if err := os.MkdirAll(filepath.Join(app, "Contents", "Frameworks", "Electron Framework.framework"), 0o755); err != nil {
		t.Fatal(err)
	}
	tgt, err := Resolve(app)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.BundleID != "dev.demo" || tgt.Name != "Demo" || !tgt.Packaged {
		t.Fatalf("unexpected target %+v", tgt)
	}
}

func TestRingKeepsLastLinesAndFinds(t *testing.T) {
	r := NewRing(3)
	w := r.Writer("stderr")
	_, _ = w.Write([]byte("one\ntwo\nDebugger listening on ws://127.0.0.1:9229/abc\nfour\n"))
	e := r.Entries()
	if len(e) != 3 || e[0].Text != "two" || e[2].Text != "four" {
		t.Fatalf("entries %+v", e)
	}
	if got := r.Find("Debugger listening on "); got != "ws://127.0.0.1:9229/abc" {
		t.Fatalf("find %q", got)
	}
	if !e[0].IsError() {
		t.Fatal("stderr lines are errors")
	}
}

func TestProbeCDPRejectsNodeInspector(t *testing.T) {
	// No server: probe must fail fast, not hang.
	if _, ok := probeCDP(t.Context(), "http://127.0.0.1:1"); ok {
		t.Fatal("probe of a closed port must fail")
	}
}

func TestParsePSReadsMemoryAndCPU(t *testing.T) {
	list := parsePS("  12   1 204800  3.5 /Apps/Demo.app/Contents/MacOS/Demo --flag a b\n 13  12 1024 0.0 helper\nbad line\n")
	if len(list) != 2 {
		t.Fatalf("entries %+v", list)
	}
	if list[0].pid != 12 || list[0].ppid != 1 || list[0].rssKB != 204800 || list[0].cpu != 3.5 || list[0].cmd != "/Apps/Demo.app/Contents/MacOS/Demo --flag a b" {
		t.Fatalf("entry %+v", list[0])
	}
}

func TestManagerAdmitCapsSessions(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.MaxSessions = 2
	for i := 0; i < 2; i++ {
		s := &Session{Info: Info{Name: "fake"}}
		if err := m.admit(); err != nil {
			t.Fatalf("session %d refused: %v", i, err)
		}
		if _, err := m.add(s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { close(s.stopWatch) })
	}
	err = m.admit()
	if err == nil || !strings.Contains(err.Error(), "2 sessions are already open") || !strings.Contains(err.Error(), "s1 (fake)") {
		t.Fatalf("expected a cap error naming the sessions, got %v", err)
	}
}

func TestIdleTimeoutStopsOwnedSession(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.IdleTimeout = 500 * time.Millisecond
	s := &Session{Info: Info{Name: "fake", Owned: true}, ring: NewRing(10)}
	if _, err := m.add(s); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.IdleStopIn <= 0 || st.IdleStopIn > 500*time.Millisecond {
		t.Fatalf("idle stop not announced: %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(m.List()) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if len(m.List()) != 0 {
		t.Fatal("idle session was not stopped")
	}
	if got := s.ring.Find("stopped after "); !strings.Contains(got, "idle timeout") {
		t.Fatalf("idle reason not logged: %q", got)
	}
}

func TestIdleTimeoutSparesDevCommandSession(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.IdleTimeout = 200 * time.Millisecond
	s := &Session{Info: Info{Name: "dev", Mode: "command", Owned: true}, ring: NewRing(10)}
	if _, err := m.add(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(s.ID) })
	if st := s.Status(); st.IdleStopIn != 0 {
		t.Fatalf("dev command session announced an idle stop: %+v", st)
	}
	time.Sleep(time.Second)
	if len(m.List()) != 1 {
		t.Fatal("dev command session was stopped for idleness")
	}
}

func TestStopDeletesArtifactsUnlessKept(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []bool{false, true} {
		s, err := m.add(&Session{Info: Info{Name: "fake"}})
		if err != nil {
			t.Fatal(err)
		}
		shot := filepath.Join(s.Dir, "screenshot.png")
		if err := os.WriteFile(shot, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		stop := m.Stop
		if keep {
			stop = m.StopKeepArtifacts
		}
		rep, err := stop(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, statErr := os.Stat(s.Dir)
		if keep {
			if statErr != nil || !rep.ArtifactsKept || rep.ArtifactsDir != s.Dir {
				t.Fatalf("keep: artifacts must stay, got %v %+v", statErr, rep)
			}
		} else if !os.IsNotExist(statErr) || rep.ArtifactsKept {
			t.Fatalf("stop: artifacts must be deleted, got %v %+v", statErr, rep)
		}
	}
}

func TestNewManagerSweepsOnlyStaleArtifacts(t *testing.T) {
	root := t.TempDir()
	stale, fresh := filepath.Join(root, "old-s1"), filepath.Join(root, "new-s1")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-StaleArtifactsAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh directory of a possibly live session removed: %v", err)
	}
}

func TestRefreshForgetsExitedDescendants(t *testing.T) {
	if testing.Short() {
		t.Skip("runs ps")
	}
	p := &ownedProc{pid: os.Getpid(), seen: map[int]string{
		// No macOS or Linux pid reaches this value: an exited process.
		99999999: "gone --user-data-dir=/tmp/x",
	}}
	p.refresh()
	if _, ok := p.seen[99999999]; ok {
		t.Fatal("an exited process must be forgotten")
	}
}
