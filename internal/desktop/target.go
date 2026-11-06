// Package desktop owns the desktop applications an agent drives, Electron
// or Tauri: it recognises what a target is, launches it (in isolation for
// Electron) or attaches to it, follows the process tree, keeps its output,
// reconnects when a development loop restarts it, and guarantees that
// everything it started is stopped.
package desktop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"howett.net/plist"
)

// Target is a resolved launch target.
type Target struct {
	// Framework is FrameworkElectron or FrameworkTauri.
	Framework string `json:"framework"`
	// DevCommand is set for a Tauri project: the app is started through
	// its dev loop (tauri dev), in Dir. Executable is then empty.
	DevCommand []string `json:"dev_command,omitempty"`
	// Executable is the program started (the Electron binary, or the
	// Tauri app's own executable).
	Executable string `json:"executable"`
	// Args precede the caller's arguments ("." for an Electron project).
	Args []string `json:"args,omitempty"`
	// Dir is the working directory (the project for an Electron project).
	Dir string `json:"dir,omitempty"`
	// Bundle is the .app bundle, when there is one.
	Bundle   string `json:"bundle,omitempty"`
	BundleID string `json:"bundle_id,omitempty"`
	Name     string `json:"name"`
	// Evidence explains how the target was recognised as Electron.
	Evidence string `json:"evidence"`
	// Packaged is true for a built application (a .app or a bare binary),
	// false for a project directory run through the electron binary.
	Packaged bool `json:"packaged"`
}

// UsageError is a caller mistake (bad path, wrong kind of target).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Resolve identifies what to start for a path: an Electron project directory
// (package.json + node_modules/electron), a Tauri project (src-tauri with
// its tauri.conf), a .app bundle built with Electron or Tauri, or an
// executable of either (inside a bundle or bare).
func Resolve(path string) (*Target, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, &UsageError{fmt.Sprintf("target %s: %v", path, err)}
	}
	if st.IsDir() && !strings.HasSuffix(abs, ".app") {
		return resolveProject(abs)
	}
	if st.IsDir() {
		return resolveBundle(abs)
	}
	if b := enclosingBundle(abs); b != "" {
		t, err := resolveBundle(b)
		if err == nil {
			t.Executable = abs
		}
		return t, err
	}
	if st.Mode()&0o111 == 0 && runtime.GOOS != "windows" {
		return nil, &UsageError{abs + " is not executable"}
	}
	if isTauriBinary(abs) {
		return &Target{Framework: FrameworkTauri, Executable: abs, Name: filepath.Base(abs), Evidence: "Tauri runtime scripts in the executable", Packaged: true}, nil
	}
	return &Target{Framework: FrameworkElectron, Executable: abs, Name: filepath.Base(abs), Evidence: "bare executable without Tauri markers (assumed Electron: it must accept --remote-debugging-port)", Packaged: true}, nil
}

func resolveProject(dir string) (*Target, error) {
	if exe := electronBinary(dir); exe == "" {
		if t, ok, err := resolveTauriProject(dir); ok || err != nil {
			return t, err
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, &UsageError{"a directory target must be an Electron project (package.json with node_modules/electron), a Tauri project (src-tauri/tauri.conf.json) or a .app bundle"}
	}
	var pkg struct {
		Name        string `json:"name"`
		ProductName string `json:"productName"`
		Main        string `json:"main"`
	}
	_ = json.Unmarshal(raw, &pkg)
	exe := electronBinary(dir)
	if exe == "" {
		return nil, &UsageError{"no Electron binary under node_modules/electron/dist: install the project's dependencies first (bun install)"}
	}
	name := pkg.ProductName
	if name == "" {
		name = pkg.Name
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	if pkg.Main != "" {
		if _, err := os.Stat(filepath.Join(dir, pkg.Main)); err != nil {
			return nil, &UsageError{fmt.Sprintf("package.json main %q does not exist: build the project first (its dev or build script)", pkg.Main)}
		}
	}
	return &Target{Framework: FrameworkElectron, Executable: exe, Args: []string{"."}, Dir: dir, Name: name, Evidence: "package.json with node_modules/electron"}, nil
}

// tauriConfigNames are the configuration files of a Tauri 2 app.
var tauriConfigNames = []string{"tauri.conf.json", "tauri.conf.json5", "Tauri.toml"}

// tauriRoot returns the project root and its src-tauri directory when dir is
// either of them.
func tauriRoot(dir string) (root, srcTauri string, ok bool) {
	has := func(d string) bool {
		for _, n := range tauriConfigNames {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return true
			}
		}
		return false
	}
	if has(filepath.Join(dir, "src-tauri")) {
		return dir, filepath.Join(dir, "src-tauri"), true
	}
	if has(dir) {
		if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
			parent := filepath.Dir(dir)
			if _, err := os.Stat(filepath.Join(parent, "package.json")); err == nil {
				return parent, dir, true
			}
			return dir, dir, true
		}
	}
	return "", "", false
}

// resolveTauriProject recognises a Tauri project and the dev command that
// runs it (tauri dev builds the Rust side and starts the frontend server).
func resolveTauriProject(dir string) (*Target, bool, error) {
	root, srcTauri, ok := tauriRoot(dir)
	if !ok {
		return nil, false, nil
	}
	name := filepath.Base(root)
	if raw, err := os.ReadFile(filepath.Join(srcTauri, "tauri.conf.json")); err == nil {
		var conf struct {
			ProductName string `json:"productName"`
		}
		if json.Unmarshal(raw, &conf) == nil && conf.ProductName != "" {
			name = conf.ProductName
		}
	}
	if err := requireBridgePlugin(srcTauri); err != nil {
		return nil, true, err
	}
	cmd, err := tauriDevCommand(root)
	if err != nil {
		return nil, true, err
	}
	return &Target{Framework: FrameworkTauri, DevCommand: cmd, Dir: root, Name: name, Evidence: "Tauri project (" + filepath.Base(srcTauri) + " with its tauri.conf)"}, true, nil
}

// bridgePluginCrate is the plugin a Tauri app needs to be driven where its
// webview has no DevTools protocol.
const bridgePluginCrate = "tauri-plugin-mcp-app-desktop"

// requireBridgePlugin fails at once, instead of after a whole tauri dev
// build and the startup timeout, when the project's Cargo.toml does not
// mention the plugin. Windows drives WebView2 over CDP and needs none; an
// unreadable manifest is not judged.
func requireBridgePlugin(srcTauri string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(srcTauri, "Cargo.toml"))
	if err != nil || bytes.Contains(raw, []byte(bridgePluginCrate)) {
		return nil
	}
	return &UsageError{fmt.Sprintf(`Tauri project %s does not include %s, which drives its webview on this OS (WebKit has no DevTools protocol). Add it to the debug builds, then launch again:
  %s: %s = { git = "https://github.com/dev-toolings/mcp-app-desktop" }
  main.rs/lib.rs: #[cfg(debug_assertions)] { builder = builder.plugin(tauri_plugin_mcp_app_desktop::init()); }
A dev command that already loads it another way can be launched with command.`, filepath.Dir(srcTauri), bridgePluginCrate, filepath.Join(srcTauri, "Cargo.toml"), bridgePluginCrate)}
}

// tauriDevCommand picks how the project runs tauri dev: its package.json
// "tauri" script, the CLI installed in node_modules, or cargo tauri.
func tauriDevCommand(root string) ([]string, error) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	hasPkg := false
	if raw, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		hasPkg = json.Unmarshal(raw, &pkg) == nil
	}
	pm := packageManager(root)
	if hasPkg && pkg.Scripts["tauri"] != "" {
		return []string{pm, "run", "tauri", "dev"}, nil
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules", ".bin", "tauri")); err == nil {
		x := map[string][]string{"bun": {"bun", "x"}, "pnpm": {"pnpm", "exec"}, "yarn": {"yarn"}, "npm": {"npx"}}[pm]
		return append(append([]string{}, x...), "tauri", "dev"), nil
	}
	if _, err := exec.LookPath("cargo-tauri"); err == nil {
		return []string{"cargo", "tauri", "dev"}, nil
	}
	return nil, &UsageError{fmt.Sprintf("Tauri project %s: no way to run tauri dev (no \"tauri\" script in package.json, no node_modules/.bin/tauri, no cargo-tauri): install @tauri-apps/cli, or pass command", root)}
}

// packageManager follows the project's lockfile (bun when it has none and
// bun is installed).
func packageManager(root string) string {
	for _, c := range []struct{ file, pm string }{{"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"}} {
		if _, err := os.Stat(filepath.Join(root, c.file)); err == nil {
			return c.pm
		}
	}
	if _, err := exec.LookPath("bun"); err == nil {
		return "bun"
	}
	return "npm"
}

// tauriMarker is part of the initialisation scripts every Tauri 2 binary
// embeds.
var tauriMarker = []byte("__TAURI_INTERNALS__")

// isTauriBinary reports whether an executable embeds the Tauri runtime.
func isTauriBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return containsStream(bufio.NewReaderSize(f, 1<<20), tauriMarker)
}

// containsStream searches r for needle without reading it whole.
func containsStream(r io.Reader, needle []byte) bool {
	buf := make([]byte, 1<<20)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(carry, buf[:n]...)
			if bytes.Contains(chunk, needle) {
				return true
			}
			keep := min(len(needle)-1, len(chunk))
			carry = append([]byte(nil), chunk[len(chunk)-keep:]...)
		}
		if err != nil {
			return false
		}
	}
}

func electronBinary(dir string) string {
	var p string
	switch runtime.GOOS {
	case "darwin":
		p = filepath.Join(dir, "node_modules", "electron", "dist", "Electron.app", "Contents", "MacOS", "Electron")
	case "windows":
		p = filepath.Join(dir, "node_modules", "electron", "dist", "electron.exe")
	default:
		p = filepath.Join(dir, "node_modules", "electron", "dist", "electron")
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func enclosingBundle(exe string) string {
	for d := filepath.Dir(exe); d != "/" && d != "."; d = filepath.Dir(d) {
		if strings.HasSuffix(d, ".app") {
			return d
		}
	}
	return ""
}

func resolveBundle(app string) (*Target, error) {
	f, err := os.Open(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return nil, &UsageError{app + " has no Contents/Info.plist"}
	}
	defer f.Close()
	var info struct {
		Exe  string `plist:"CFBundleExecutable"`
		ID   string `plist:"CFBundleIdentifier"`
		Name string `plist:"CFBundleName"`
	}
	if err := plist.NewDecoder(f).Decode(&info); err != nil {
		return nil, fmt.Errorf("%s: Info.plist: %w", app, err)
	}
	if info.Exe == "" {
		return nil, &UsageError{app + " declares no CFBundleExecutable"}
	}
	t := &Target{Executable: filepath.Join(app, "Contents", "MacOS", info.Exe), Bundle: app, BundleID: info.ID, Name: info.Name, Packaged: true}
	if t.Name == "" {
		t.Name = strings.TrimSuffix(filepath.Base(app), ".app")
	}
	if _, err := os.Stat(filepath.Join(app, "Contents", "Frameworks", "Electron Framework.framework")); err == nil {
		t.Framework, t.Evidence = FrameworkElectron, "Contents/Frameworks/Electron Framework.framework"
		return t, nil
	}
	if isTauriBinary(t.Executable) {
		t.Framework, t.Evidence = FrameworkTauri, "Tauri runtime scripts in Contents/MacOS/"+info.Exe
		return t, nil
	}
	return nil, &UsageError{app + " is neither an Electron application (no Contents/Frameworks/Electron Framework.framework) nor a Tauri one (no Tauri runtime in its executable)"}
}
