package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
)

// gpuiSetup is how a GPUI app is made drivable, for error messages.
const gpuiSetup = `  Cargo.toml: ` + tauri.GPUIBridgeCrate + ` = { git = "https://github.com/dev-toolings/mcp-app-desktop", optional = true }
              [features] automation = ["dep:` + tauri.GPUIBridgeCrate + `"]
  main.rs:    #[cfg(feature = "automation")] gpui_mcp_app_desktop::init(cx);   (in the Application::run closure)
  elements:   give what must be driven an .id(...) and a .role(...) (and an .aria_label(...) when its text is not its name): only those appear in a snapshot
Optionally: [package.metadata.mcp-app-desktop] dev-command = ["make", "run"] (the command launch runs; default cargo run --features automation)`

// cargoPackage is the part of `cargo metadata` read here.
type cargoPackage struct {
	Name         string              `json:"name"`
	ManifestPath string              `json:"manifest_path"`
	Features     map[string][]string `json:"features"`
	Dependencies []struct {
		Name     string  `json:"name"`
		Rename   *string `json:"rename"`
		Optional bool    `json:"optional"`
		Kind     *string `json:"kind"`
	} `json:"dependencies"`
	Metadata json.RawMessage `json:"metadata"`
}

type cargoMetadata struct {
	Packages []cargoPackage  `json:"packages"`
	Metadata json.RawMessage `json:"metadata"` // [workspace.metadata]
}

// cargoMetadataTimeout bounds `cargo metadata --no-deps`, which reads the
// manifests only (no network, no build): well under a second normally.
const cargoMetadataTimeout = 30 * time.Second

// readCargoMetadata asks cargo for the workspace dir belongs to. Cargo, not
// a TOML reader, so workspace inheritance, renamed dependencies and implicit
// features are read as cargo itself understands them.
func readCargoMetadata(dir string) (*cargoMetadata, error) {
	cargo, err := execLookPath("cargo")
	if err != nil {
		return nil, fmt.Errorf("cargo is not on PATH (install Rust, or pass command): %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cargoMetadataTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cargo, "metadata", "--no-deps", "--format-version", "1", "--offline")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cargo metadata in %s did not answer within %s", dir, cargoMetadataTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("cargo metadata in %s: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var meta cargoMetadata
	if err := json.Unmarshal(out, &meta); err != nil {
		return nil, fmt.Errorf("cargo metadata in %s: %w", dir, err)
	}
	return &meta, nil
}

// bridgeAlias is the name under which pkg depends on gpui-mcp-app-desktop
// (its rename, if any), and whether that dependency is optional; ok is false
// when it does not depend on it (dev-dependencies do not count).
func (pkg *cargoPackage) bridgeAlias() (alias string, optional, ok bool) {
	for _, d := range pkg.Dependencies {
		if d.Name != tauri.GPUIBridgeCrate || (d.Kind != nil && *d.Kind == "dev") {
			continue
		}
		alias = d.Name
		if d.Rename != nil && *d.Rename != "" {
			alias = *d.Rename
		}
		return alias, d.Optional, true
	}
	return "", false, false
}

// usesGPUI tells whether pkg depends on a GPUI crate (gpui, gpui-pre, a kit
// built on it), to tell a GPUI app without the bridge from any Rust project.
func (pkg *cargoPackage) usesGPUI() bool {
	for _, d := range pkg.Dependencies {
		if d.Name != tauri.GPUIBridgeCrate && strings.HasPrefix(d.Name, "gpui") {
			return true
		}
	}
	return false
}

// bridgeFeature picks the feature that turns the optional bridge dependency
// on: "automation" when it does, else the first such feature by name.
func (pkg *cargoPackage) bridgeFeature(alias string) (string, bool) {
	var enabling []string
	for name := range pkg.Features {
		if featureEnables(pkg.Features, name, alias, map[string]bool{}) {
			enabling = append(enabling, name)
		}
	}
	if len(enabling) == 0 {
		return "", false
	}
	sort.Strings(enabling)
	for _, f := range enabling {
		if f == "automation" {
			return f, true
		}
	}
	return enabling[0], true
}

// featureEnables follows a feature's list: dep:alias, alias and alias/feat
// enable the dependency (alias?/feat does not), another feature may.
func featureEnables(features map[string][]string, name, alias string, seen map[string]bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	for _, v := range features[name] {
		switch {
		case v == "dep:"+alias, v == alias, strings.HasPrefix(v, alias+"/"):
			return true
		case strings.Contains(v, "/") || strings.HasPrefix(v, "dep:"):
			continue
		case featureEnables(features, v, alias, seen):
			return true
		}
	}
	return false
}

// devCommandMetadata reads dev-command from a [package.metadata.mcp-app-desktop]
// or [workspace.metadata.mcp-app-desktop] table; nil when there is none.
func devCommandMetadata(raw json.RawMessage, where string) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m struct {
		Section *struct {
			DevCommand json.RawMessage `json:"dev-command"`
		} `json:"mcp-app-desktop"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Section == nil || len(m.Section.DevCommand) == 0 {
		return nil, nil
	}
	var cmd []string
	if err := json.Unmarshal(m.Section.DevCommand, &cmd); err != nil || len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
		return nil, &UsageError{fmt.Sprintf("%s: [%s.metadata.mcp-app-desktop] dev-command must be a non-empty array of strings, e.g. [\"make\", \"run\"] (got %s)", where, strings.SplitN(where, " ", 2)[0], m.Section.DevCommand)}
	}
	return cmd, nil
}

// resolveGPUIProject recognises a Cargo project whose app embeds
// gpui-mcp-app-desktop and the command that runs it with the bridge: the
// project's dev-command metadata, else cargo run (with the feature turning
// the bridge on when it is optional). ok is false for a directory that is not
// a GPUI project at all.
func resolveGPUIProject(dir string) (*Target, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	// Not a Cargo project, or a package that names no GPUI crate: running cargo would only be
	// slower. A workspace root is always read: its members may use GPUI without it saying so.
	if err != nil || (!bytes.Contains(raw, []byte("gpui")) && !bytes.Contains(raw, []byte("[workspace]"))) {
		return nil, false, nil
	}
	meta, err := readCargoMetadata(dir)
	if err != nil {
		return nil, true, &UsageError{fmt.Sprintf("GPUI project %s: %v", dir, err)}
	}
	var withBridge []*cargoPackage
	var here *cargoPackage
	gpuiApp := false
	for i := range meta.Packages {
		pkg := &meta.Packages[i]
		if _, _, ok := pkg.bridgeAlias(); ok {
			withBridge = append(withBridge, pkg)
			// Cargo reports resolved paths (/private/var/… for /var/… on macOS).
			if samePath(filepath.Dir(pkg.ManifestPath), dir) {
				here = pkg
			}
		}
		gpuiApp = gpuiApp || pkg.usesGPUI()
	}
	if len(withBridge) == 0 {
		if !gpuiApp {
			return nil, false, nil
		}
		return nil, true, &UsageError{fmt.Sprintf("GPUI project %s does not include %s, which lets mcp-app-desktop read and drive its windows. Add it to a debug build, then launch again:\n%s\nA dev command that already starts it with the bridge can be launched with command.", dir, tauri.GPUIBridgeCrate, gpuiSetup)}
	}
	pkg := here
	if pkg == nil {
		if len(withBridge) > 1 {
			names := make([]string, 0, len(withBridge))
			for _, p := range withBridge {
				names = append(names, p.Name+" ("+filepath.Dir(p.ManifestPath)+")")
			}
			return nil, true, &UsageError{fmt.Sprintf("workspace %s has several packages with %s: %s. Launch the directory of the app to run", dir, tauri.GPUIBridgeCrate, strings.Join(names, ", "))}
		}
		pkg = withBridge[0]
	}
	pkgDir := filepath.Dir(pkg.ManifestPath)
	if pkg == here {
		pkgDir = dir // as the caller spelled it
	}
	t := &Target{Framework: FrameworkGPUI, Dir: pkgDir, Name: pkg.Name}
	cmd, err := devCommandMetadata(pkg.Metadata, "package "+pkg.Name)
	if err == nil && cmd == nil {
		cmd, err = devCommandMetadata(meta.Metadata, "workspace of "+pkg.Name)
	}
	if err != nil {
		return nil, true, err
	}
	if cmd != nil {
		t.DevCommand, t.Evidence = cmd, "GPUI project with "+tauri.GPUIBridgeCrate+" (dev-command from [metadata.mcp-app-desktop])"
		return t, true, nil
	}
	alias, optional, _ := pkg.bridgeAlias()
	if !optional {
		t.DevCommand, t.Evidence = []string{"cargo", "run"}, "GPUI project with "+tauri.GPUIBridgeCrate
		return t, true, nil
	}
	feature, ok := pkg.bridgeFeature(alias)
	if !ok {
		return nil, true, &UsageError{fmt.Sprintf("GPUI project %s depends on %s as optional, but no feature turns it on: add one to [features] (automation = [\"dep:%s\"]), or set [package.metadata.mcp-app-desktop] dev-command", pkgDir, tauri.GPUIBridgeCrate, alias)}
	}
	t.DevCommand = []string{"cargo", "run", "--features", feature}
	t.Evidence = "GPUI project with " + tauri.GPUIBridgeCrate + " (feature " + feature + ")"
	return t, true, nil
}

// samePath tells whether a and b name the same directory, symlinks resolved.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// mentionsGPUIBridge tells, from the manifest text alone, whether dir is a
// GPUI project with the bridge: enough to size a dev command's startup
// timeout (a cold cargo build takes minutes).
func mentionsGPUIBridge(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	return err == nil && bytes.Contains(raw, []byte(tauri.GPUIBridgeCrate))
}

// Markers of a binary built with gpui-mcp-app-desktop: the crate's name
// (its log prefix) and the variable it reads. Its « bridge listening » line
// is no marker: the name is interpolated, so the line is not contiguous in
// the binary.
var gpuiMarkers = [][]byte{[]byte(tauri.GPUIBridgeCrate), []byte(tauri.EnvBridgePort)}

// binaryFramework recognises a Tauri or a GPUI (with its bridge) executable
// in one pass over the file; "" when it is neither.
func binaryFramework(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	found := scanMarkers(f, append([][]byte{tauriMarker}, gpuiMarkers...)...)
	switch {
	case found[0]:
		return FrameworkTauri
	case found[1] && found[2]:
		return FrameworkGPUI
	}
	return ""
}

// gpuiBridgeHint tells what a GPUI app needs for its bridge to open.
const gpuiBridgeHint = "the app must call gpui_mcp_app_desktop::init(cx) (" + tauri.GPUIBridgeCrate + ", in a debug build, e.g. cargo run --features automation) and inherit $" + tauri.EnvBridgePort + " / $" + tauri.EnvBridgeToken + " from its environment"
