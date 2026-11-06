package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dev-toolings/mcp-app-desktop/plugin/skills"
)

const serverName = "mcp-app-desktop"

// legacyName is the name this server had while it drove Electron only.
// install and uninstall remove its registrations, skills and binary, so a
// host does not run both servers side by side.
const legacyName = "mcp-electron"

// installer plans and applies the registration of this server with the
// agent CLIs. Every step is printed; with dryRun nothing is written, copied,
// removed or registered, and the current state is reported instead.
type installer struct {
	dryRun bool
	out    io.Writer
	home   string
	// lookPath and run are swapped in tests.
	lookPath func(string) (string, error)
	run      func(name string, argv ...string) (string, error)
}

// agentHost is a CLI that can register MCP servers and read skills.
type agentHost struct {
	name, cli, skillDir string
	add, remove, get    []string
}

func (in *installer) hosts(exe string, withCodex bool) []agentHost {
	return in.hostsNamed(serverName, exe, withCodex)
}

func (in *installer) hostsNamed(name, exe string, withCodex bool) []agentHost {
	hosts := []agentHost{{
		name: "Claude Code", cli: "claude", skillDir: filepath.Join(in.home, ".claude", "skills", name),
		add:    []string{"mcp", "add", "--scope", "user", name, "--", exe, "mcp"},
		remove: []string{"mcp", "remove", "--scope", "user", name},
		get:    []string{"mcp", "get", name},
	}}
	if withCodex {
		hosts = append(hosts, agentHost{
			name: "Codex", cli: "codex", skillDir: filepath.Join(in.home, ".codex", "skills", name),
			add:    []string{"mcp", "add", name, "--", exe, "mcp"},
			remove: []string{"mcp", "remove", name},
			get:    []string{"mcp", "get", name},
		})
	}
	return hosts
}

// removeLegacy unregisters mcp-electron and removes its skill copies and
// its binary in destDir, so an agent does not run both servers. A failure
// is reported with the others and the new server is installed anyway.
func (in *installer) removeLegacy(destDir string, withCodex bool) []string {
	var failures []string
	for _, h := range in.hostsNamed(legacyName, "", withCodex) {
		if _, err := in.lookPath(h.cli); err == nil && in.registered(h) {
			in.say("\n%s: %s was renamed %s, removing the old registration", h.name, legacyName, serverName)
			if err := in.step(h.cli, h.remove...); err != nil {
				failures = append(failures, fmt.Sprintf("%s: remove %s: %v", h.name, legacyName, err))
			} else if !in.dryRun && in.registered(h) {
				failures = append(failures, fmt.Sprintf("%s: %s still registered after removal", h.name, legacyName))
			}
		}
		if fileExists(h.skillDir) {
			in.removePath(h.skillDir)
		}
	}
	if destDir != "" {
		if old := filepath.Join(destDir, legacyName); fileExists(old) {
			in.removePath(old)
		}
	}
	return failures
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	noCodex := fs.Bool("no-codex", false, "do not register with Codex even when its CLI is installed")
	codex := fs.Bool("codex", false, "register with Codex (default: when its CLI is on PATH)")
	dryRun := fs.Bool("dry-run", false, "print every step and the current state; write, copy, remove and register nothing")
	uninstall := fs.Bool("uninstall", false, "remove the registrations, the skill copies and the installed binary")
	dest := fs.String("dest", filepath.Join("~", ".local", "bin"), "directory receiving a copy of the binary that gets registered")
	bin := fs.String("bin", "", "register this executable as is instead of copying one to --dest")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mcp-app-desktop install [--dry-run] [--no-codex] [--dest DIR | --bin PATH] | install --uninstall [--dry-run]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	in := &installer{dryRun: *dryRun, out: os.Stdout, home: home, lookPath: exec.LookPath}
	in.run = func(name string, argv ...string) (string, error) {
		cmd := exec.Command(name, argv...)
		b, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}
	withCodex := *codex
	if !*noCodex && !withCodex {
		_, err := in.lookPath("codex")
		withCodex = err == nil
	}
	destDir := strings.Replace(*dest, "~", home, 1)
	if *uninstall {
		return in.uninstall(destDir, withCodex)
	}
	exe := *bin
	if exe == "" {
		src, err := os.Executable()
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(src); err == nil {
			src = resolved
		}
		exe = filepath.Join(destDir, serverName)
		if err := in.copyBinary(src, exe); err != nil {
			return err
		}
	} else if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	return in.installFrom(exe, destDir, withCodex)
}

func (in *installer) say(format string, a ...any) { fmt.Fprintf(in.out, format+"\n", a...) }

// step prints a command and runs it unless dry-running.
func (in *installer) step(name string, argv ...string) error {
	in.say("$ %s %s", name, strings.Join(argv, " "))
	if in.dryRun {
		return nil
	}
	out, err := in.run(name, argv...)
	if out != "" {
		in.say("  %s", strings.ReplaceAll(out, "\n", "\n  "))
	}
	return err
}

// registered reports whether a host already knows the server.
func (in *installer) registered(h agentHost) bool {
	_, err := in.run(h.cli, h.get...)
	return err == nil
}

func (in *installer) copyBinary(src, dst string) error {
	if same, _ := sameFile(src, dst); same {
		in.say("binary already at %s", dst)
		return nil
	}
	in.say("copy %s → %s", src, dst)
	if in.dryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

func (in *installer) install(exe string, withCodex bool) error {
	return in.installFrom(exe, filepath.Dir(exe), withCodex)
}

func (in *installer) installFrom(exe, destDir string, withCodex bool) error {
	if in.dryRun {
		in.say("dry run: nothing is written, copied or registered")
	}
	failures := in.removeLegacy(destDir, withCodex)
	for _, h := range in.hosts(exe, withCodex) {
		in.say("\n%s", h.name)
		if _, err := in.lookPath(h.cli); err != nil {
			cfg, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{serverName: map[string]any{"command": exe, "args": []string{"mcp"}}}}, "", "  ")
			in.say("%q is not on PATH; add this to its MCP configuration by hand:\n%s", h.cli, cfg)
			failures = append(failures, h.name+" CLI not found")
			continue
		}
		if in.registered(h) {
			in.say("already registered: replacing the entry")
			if err := in.step(h.cli, h.remove...); err != nil {
				failures = append(failures, fmt.Sprintf("%s: remove old entry: %v", h.name, err))
				continue
			}
		}
		if err := in.step(h.cli, h.add...); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", h.name, err))
			continue
		}
		if err := in.writeSkill(h.skillDir); err != nil {
			failures = append(failures, fmt.Sprintf("%s skill: %v", h.name, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	if !in.dryRun {
		in.say("\n%s registered as %q. Restart the agent, then ask it to launch an Electron or Tauri app.", serverName, exe)
	}
	return nil
}

func (in *installer) uninstall(destDir string, withCodex bool) error {
	if in.dryRun {
		in.say("dry run: nothing is removed or unregistered")
	}
	var failures []string
	for _, h := range in.hosts("", withCodex) {
		in.say("\n%s", h.name)
		if _, err := in.lookPath(h.cli); err != nil {
			in.say("%q is not on PATH: remove %q from its MCP configuration by hand", h.cli, serverName)
		} else if !in.registered(h) {
			in.say("not registered")
		} else if err := in.step(h.cli, h.remove...); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", h.name, err))
		} else if in.registered(h) {
			failures = append(failures, h.name+": still registered after removal")
		}
		in.removePath(h.skillDir)
	}
	in.say("")
	in.removePath(filepath.Join(destDir, serverName))
	failures = append(failures, in.removeLegacy(destDir, withCodex)...)
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (in *installer) removePath(path string) {
	if _, err := os.Lstat(path); err != nil {
		in.say("absent %s", path)
		return
	}
	in.say("remove %s", path)
	if in.dryRun {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		in.say("  %v", err)
	}
}

func (in *installer) writeSkill(dir string) error {
	files := skills.Files()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(dir, filepath.FromSlash(n))
		verb := "write"
		if current, err := os.ReadFile(path); err == nil && string(current) == files[n] {
			verb = "unchanged"
		}
		in.say("%s %s", verb, path)
		if in.dryRun || verb == "unchanged" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(files[n]), 0o644); err != nil {
			return err
		}
	}
	return nil
}
