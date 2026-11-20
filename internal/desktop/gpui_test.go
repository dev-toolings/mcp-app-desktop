package desktop

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/tauri/tauritest"
)

func needCargo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not on PATH: GPUI projects are read with cargo metadata")
	}
}

// gpuiPackage writes a binary Cargo package whose [dependencies] and extra
// sections are given.
func gpuiPackage(t *testing.T, dir, name, deps, extra string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "src", "main.rs"), "fn main() {}\n")
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \""+name+"\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n"+deps+"\n"+extra)
}

func TestResolveGPUIProject(t *testing.T) {
	needCargo(t)
	const gpui = "gpui = { package = \"gpui-pre\", version = \"0.3\" }\n"
	cases := []struct {
		name, deps, extra string
		cmd               string // expected dev command; "" when an error is expected
		err               string
	}{
		{"no bridge", gpui, "", "", "does not include gpui-mcp-app-desktop"},
		{"always on", gpui + "gpui-mcp-app-desktop = \"0.1\"\n", "", "cargo run", ""},
		{"automation feature", gpui + "gpui-mcp-app-desktop = { version = \"0.1\", optional = true }\n",
			"[features]\nhot-reload = []\nautomation = [\"dep:gpui-mcp-app-desktop\"]\n", "cargo run --features automation", ""},
		// A renamed dependency, turned on through another feature.
		{"feature chain", gpui + "bridge = { package = \"gpui-mcp-app-desktop\", version = \"0.1\", optional = true }\n",
			"[features]\nui-tests = [\"bridge-on\"]\nbridge-on = [\"dep:bridge\"]\n", "cargo run --features bridge-on", ""},
		// No feature names it: cargo's implicit feature of the optional dependency.
		{"implicit feature", gpui + "gpui-mcp-app-desktop = { version = \"0.1\", optional = true }\n",
			"[features]\nother = []\n", "cargo run --features gpui-mcp-app-desktop", ""},
		{"metadata", gpui + "gpui-mcp-app-desktop = { version = \"0.1\", optional = true }\n",
			"[features]\nautomation = [\"dep:gpui-mcp-app-desktop\"]\n\n[package.metadata.mcp-app-desktop]\ndev-command = [\"make\", \"run\"]\n", "make run", ""},
		{"bad metadata", gpui + "gpui-mcp-app-desktop = \"0.1\"\n",
			"[package.metadata.mcp-app-desktop]\ndev-command = \"make run\"\n", "", "must be a non-empty array of strings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			gpuiPackage(t, dir, "demo-gpui", c.deps, c.extra)
			tg, err := Resolve(dir)
			if c.err != "" {
				var ue *UsageError
				if err == nil || !strings.Contains(err.Error(), c.err) || !errors.As(err, &ue) {
					t.Fatalf("want a usage error with %q, got %+v %v", c.err, tg, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tg.Framework != FrameworkGPUI || tg.Name != "demo-gpui" || tg.Dir != dir || strings.Join(tg.DevCommand, " ") != c.cmd || tg.Executable != "" {
				t.Fatalf("got %+v", tg)
			}
		})
	}
}

func TestResolveGPUIWorkspaceMember(t *testing.T) {
	needCargo(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"app\", \"lib\"]\nresolver = \"2\"\n\n[workspace.dependencies]\ngpui = { package = \"gpui-pre\", version = \"0.3\" }\n\n[workspace.metadata.mcp-app-desktop]\ndev-command = [\"make\", \"run-app\"]\n")
	gpuiPackage(t, filepath.Join(root, "app"), "the-app", "gpui.workspace = true\ngpui-mcp-app-desktop = \"0.1\"\n", "")
	gpuiPackage(t, filepath.Join(root, "lib"), "the-lib", "gpui.workspace = true\n", "")
	tg, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	// The member with the bridge, run from its directory, with the
	// workspace's dev command.
	if tg.Framework != FrameworkGPUI || tg.Name != "the-app" || !samePath(tg.Dir, filepath.Join(root, "app")) || strings.Join(tg.DevCommand, " ") != "make run-app" {
		t.Fatalf("got %+v", tg)
	}
}

// A workspace root that names no GPUI crate itself: its member does.
func TestResolveGPUIWorkspaceWithoutGPUIAtRoot(t *testing.T) {
	needCargo(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"app\"]\nresolver = \"2\"\n")
	gpuiPackage(t, filepath.Join(root, "app"), "the-app", "gpui = { package = \"gpui-pre\", version = \"0.3\" }\ngpui-mcp-app-desktop = \"0.1\"\n", "")
	tg, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Framework != FrameworkGPUI || tg.Name != "the-app" || !samePath(tg.Dir, filepath.Join(root, "app")) {
		t.Fatalf("got %+v", tg)
	}
}

func TestResolveRustProjectWithoutGPUI(t *testing.T) {
	needCargo(t)
	dir := t.TempDir()
	// Mentions gpui in a comment only: cargo is asked, finds no GPUI crate.
	gpuiPackage(t, dir, "plain", "# not gpui\nserde = \"1\"\n", "")
	if _, err := Resolve(dir); err == nil || !strings.Contains(err.Error(), "a GPUI project (Cargo.toml with gpui-mcp-app-desktop)") {
		t.Fatalf("a Rust project that is no GPUI app: %v", err)
	}
}

func TestResolveGPUIExecutableAndBundle(t *testing.T) {
	dir := t.TempDir()
	// The markers far apart and past the first read: a real binary is tens of MiB.
	exe := filepath.Join(dir, "gpui-app")
	writeFile(t, exe, strings.Repeat("x", 3<<20)+"[gpui-mcp-app-desktop"+strings.Repeat("z", 2<<20)+"MCP_APP_DESKTOP_BRIDGE_PORT"+strings.Repeat("y", 1000))
	tg, err := Resolve(exe)
	if err != nil || tg.Framework != FrameworkGPUI || !tg.Packaged || tg.Executable != exe || len(tg.DevCommand) != 0 {
		t.Fatalf("gpui exe: %+v %v", tg, err)
	}
	// The crate name alone (a GPUI app built without the bridge feature
	// does not even have that) is not enough.
	half := filepath.Join(dir, "half")
	writeFile(t, half, "gpui-mcp-app-desktop only")
	if tg, err := Resolve(half); err != nil || tg.Framework != FrameworkElectron {
		t.Fatalf("one marker: %+v %v", tg, err)
	}
	// A Tauri binary that also reads the bridge variable stays Tauri.
	tauriExe := filepath.Join(dir, "tauri-app")
	writeFile(t, tauriExe, "__TAURI_INTERNALS__ MCP_APP_DESKTOP_BRIDGE_PORT tauri-plugin-mcp-app-desktop")
	if tg, err := Resolve(tauriExe); err != nil || tg.Framework != FrameworkTauri {
		t.Fatalf("tauri exe: %+v %v", tg, err)
	}

	app := filepath.Join(dir, "Demo.app")
	writeFile(t, filepath.Join(app, "Contents", "Info.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>demo</string>
<key>CFBundleIdentifier</key><string>dev.demo.gpui</string>
<key>CFBundleName</key><string>Demo GPUI</string>
</dict></plist>`)
	writeFile(t, filepath.Join(app, "Contents", "MacOS", "demo"), "gpui-mcp-app-desktop\x00MCP_APP_DESKTOP_BRIDGE_PORT")
	tg, err = Resolve(app)
	if err != nil || tg.Framework != FrameworkGPUI || tg.Name != "Demo GPUI" || tg.Bundle != app {
		t.Fatalf("gpui bundle: %+v %v", tg, err)
	}
	writeFile(t, filepath.Join(app, "Contents", "MacOS", "demo"), "nothing known")
	if _, err := Resolve(app); err == nil || !strings.Contains(err.Error(), "gpui_mcp_app_desktop::init") {
		t.Fatalf("unknown bundle: %v", err)
	}
}

func TestScanMarkersAcrossReads(t *testing.T) {
	data := []byte("aa-gpui-mcp-app-desktop-bb-MCP_APP_DESKTOP_BRIDGE_PORT-cc")
	for n := 1; n <= len(data); n++ {
		found := scanMarkers(&chunkReader{data: append([]byte(nil), data...), n: n}, gpuiMarkers...)
		if !found[0] || !found[1] {
			t.Fatalf("missed a marker with reads of %d bytes: %v", n, found)
		}
	}
	if found := scanMarkers(&chunkReader{data: []byte("gpui-mcp-app-deskto"), n: 4}, gpuiMarkers...); found[0] || found[1] {
		t.Fatalf("false positive: %v", found)
	}
}

func TestAttachGPUIBridge(t *testing.T) {
	b := tauritest.StartGPUI(t, "tok")
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := m.Attach(ctx, AttachSpec{Endpoint: strconv.Itoa(b.Port()), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.Framework != FrameworkGPUI || st.Name != "Fake GPUI" || st.PID != 4343 {
		t.Fatalf("status: %+v", st)
	}
	if fw := m.Frameworks(); !fw[FrameworkGPUI] || fw[FrameworkTauri] {
		t.Fatalf("frameworks: %v", fw)
	}
	// Logs poll no console: no eval reaches the app.
	_ = s.Logs()
	for _, c := range b.Commands() {
		if c == "eval" {
			t.Fatalf("a script was sent to a GPUI app: %v", b.Commands())
		}
	}
}

func TestCommandNoteAndNotReachedNameGPUI(t *testing.T) {
	if n := commandNote(FrameworkGPUI, false); !strings.Contains(n, "gpui-mcp-app-desktop") || !strings.Contains(n, "no JavaScript") {
		t.Fatalf("note: %s", n)
	}
	s := &Session{Info: Info{Name: "demo", Framework: FrameworkGPUI}, ring: NewRing(10), bridgeCandidate: "127.0.0.1:1"}
	if err := s.notReachedError(time.Second); !strings.Contains(err.Error(), "gpui_mcp_app_desktop::init(cx)") || !strings.Contains(err.Error(), "--features automation") {
		t.Fatalf("not reached: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[dependencies]\ngpui-mcp-app-desktop = \"0.1\"\n")
	if !mentionsGPUIBridge(dir) || mentionsGPUIBridge(t.TempDir()) {
		t.Fatal("mentionsGPUIBridge")
	}
}
