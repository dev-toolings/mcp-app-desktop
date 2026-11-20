package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProfilePlaceholdersAreDetectedAndExpanded(t *testing.T) {
	if wantsProfile([]string{"bun", "run", "dev"}, map[string]string{"PORT": "5198"}) {
		t.Fatal("a launch naming no placeholder must not get a profile")
	}
	if !wantsProfile([]string{"--config", `{"identifier":"com.example.app.${MCP_APP_DESKTOP_PROFILE_ID}"}`}, nil) {
		t.Fatal("the identifier placeholder in argv asks for a profile")
	}
	if !wantsProfile(nil, map[string]string{"APP_DATA": "$MCP_APP_DESKTOP_PROFILE_DIR/data"}) {
		t.Fatal("the directory placeholder in env asks for a profile")
	}
	p, err := newProfile(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if !profileIDPattern.MatchString(p.ID) {
		t.Fatalf("id %q is not of the generated shape", p.ID)
	}
	if info, err := os.Stat(p.Dir); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatalf("profile dir: %v %v", info, err)
	}
	got := p.expand("${MCP_APP_DESKTOP_PROFILE_DIR}/data com.example.app.$MCP_APP_DESKTOP_PROFILE_ID ${MCP_APP_DESKTOP_CDP_PORT}")
	if want := p.Dir + "/data com.example.app." + p.ID + " ${MCP_APP_DESKTOP_CDP_PORT}"; got != want {
		t.Fatalf("expand = %q, want %q", got, want)
	}
	other, err := newProfile(filepath.Dir(p.Dir))
	if err != nil || other.ID == p.ID || other.Dir == p.Dir {
		t.Fatalf("a second profile must be distinct: %+v %v", other, err)
	}
}

// useIdentifierRoot points the identifier data lookup at a directory of the
// test, never at the developer's real application data.
func useIdentifierRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := identifierDataRoots
	identifierDataRoots = func() []string { return []string{root, filepath.Join(root, "missing")} }
	t.Cleanup(func() { identifierDataRoots = prev })
	return root
}

func TestProfileRemoveDeletesOnlyWhatItsIdentifierLeft(t *testing.T) {
	root := useIdentifierRoot(t)
	p, err := newProfile(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := newProfile(filepath.Dir(p.Dir))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mine := []string{mk("com.example.app." + p.ID), mk("com.example.app." + p.ID + ".savedState")}
	plist := filepath.Join(root, "com.example.app."+p.ID+".plist")
	if err := os.WriteFile(plist, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine = append(mine, plist)
	kept := []string{
		mk("com.example.app"),                    // the developer's real profile
		mk("com.example.app." + other.ID),        // another launch's
		mk("com.example.app." + p.ID + "-suite"), // only a name that ends with the segment
		mk(p.ID),                                 // not an identifier segment: no dot before it
		mk("com.example.app." + p.ID + ".backup"),
	}
	// A link named after the identifier: the link goes, what it points to stays.
	target := mk("elsewhere")
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "com.linked."+p.ID)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		mine = append(mine, link)
	}
	kept = append(kept, target, other.Dir)

	removed, err := p.remove()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(mine)+1 {
		t.Fatalf("removed %v, want the profile dir and %v", removed, mine)
	}
	for _, path := range append(mine, p.Dir) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s must be removed (%v)", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(filepath.Join(path, "data.db")); err != nil && path != other.Dir {
			t.Errorf("%s must be left alone: %v", path, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s must be left alone: %v", path, err)
		}
	}
}

func TestIdentifierDataIgnoresASegmentItDidNotGenerate(t *testing.T) {
	root := useIdentifierRoot(t)
	for _, name := range []string{"com.example.app", "com.example.app.test", "com.example."} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"", "app", "test", "example.app", "mcp", "mcp12345", "mcpZZZZZZZZZZZZ", "../mcp0123456789ab"} {
		if got := identifierData(id); got != nil {
			t.Errorf("identifierData(%q) = %v: only a generated segment may select anything", id, got)
		}
	}
	// A profile adopted from a state file with a foreign id removes nothing
	// under the identifier roots.
	removed, err := (&profile{ID: "app"}).remove()
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "com.example.app")); err != nil {
		t.Fatal(err)
	}
}

func TestElectronTargetRefusesProfilePlaceholders(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "app")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = m.Launch(context.Background(), LaunchSpec{Target: exe, Env: map[string]string{"DATA": "${MCP_APP_DESKTOP_PROFILE_DIR}"}})
	if _, ok := err.(*UsageError); !ok || !strings.Contains(err.Error(), "--user-data-dir") {
		t.Fatalf("an Electron target must refuse the placeholder instead of passing it unexpanded: %v", err)
	}
	if entries, _ := os.ReadDir(m.profilesDir()); len(entries) != 0 {
		t.Fatalf("no profile may be created for a refused launch: %v", entries)
	}
}

func TestCommandNotFoundLeavesNoProfile(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Launch(context.Background(), LaunchSpec{Command: []string{"no-such-program-mcp-app-desktop", "${MCP_APP_DESKTOP_PROFILE_ID}"}, Dir: t.TempDir()})
	if _, ok := err.(*UsageError); !ok {
		t.Fatalf("want a usage error, got %v", err)
	}
	if entries, _ := os.ReadDir(m.profilesDir()); len(entries) != 0 {
		t.Fatalf("a launch that did not start must leave no profile: %v", entries)
	}
}

// A dev command that ended without stop leaves its state file and its
// profile. The next launch of the same command removes that profile once no
// process of the command remains, and never follows a state file that names
// another directory.
func TestStaleDevStateGivesItsProfileBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dev commands are not adopted on Windows")
	}
	idRoot := useIdentifierRoot(t)
	m, err := NewManager(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	setProcessGroup(dead)
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{Command: []string{"no-such-program-mcp-app-desktop", "${MCP_APP_DESKTOP_PROFILE_ID}"}, Dir: t.TempDir()}
	statePath := m.devStatePath(devKey(spec.Dir, spec.Command, spec.Inspect))
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	leave := func(dir, id string) {
		t.Helper()
		st := &devState{Name: "dev", Dir: spec.Dir, Command: spec.Command, PID: dead.Process.Pid, PGID: dead.Process.Pid, RootCmd: "gone", CDPPort: 9222, ProfileDir: dir, ProfileID: id}
		if err := writeDevState(statePath, st); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Launch(context.Background(), spec); err == nil {
			t.Fatal("the program does not exist: the launch must fail after the stale state is handled")
		}
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatalf("the stale state must be removed (%v)", err)
		}
	}

	p, err := newProfile(m.profilesDir())
	if err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(idRoot, "com.example.app."+p.ID)
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	leave(p.Dir, p.ID)
	for _, path := range []string{p.Dir, left} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s must go with its dead command (%v)", path, err)
		}
	}

	// A state file that names a directory this server never created.
	foreign := filepath.Join(t.TempDir(), "mcp0123456789ab")
	keep := filepath.Join(idRoot, "com.example.app.mcp0123456789ab")
	for _, dir := range []string{foreign, keep} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	leave(foreign, "mcp0123456789ab")
	for _, path := range []string{foreign, keep} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is not this server's profile and must be left alone: %v", path, err)
		}
	}
}

// TestE2ETauriPrivateProfile launches the fixture through a command that
// names both placeholders, as a tauri dev command given a private identifier
// and data directory would be. The profile survives the server with the
// command, the server that adopts it knows it, and stop removes it together
// with what the app left under its private identifier, nothing else.
func TestE2ETauriPrivateProfile(t *testing.T) {
	exe := tauriFixture(t)
	idRoot := useIdentifierRoot(t)
	real := filepath.Join(idRoot, "com.example.fixture")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sessions")
	// What a Tauri app does with its identifier and its data variable.
	// The shell stays the root of the command, as a dev loop does: a command
	// that replaces itself (exec) is not recognised by the next server.
	script := `mkdir -p "$ID_ROOT/com.example.fixture.$1" "$FIXTURE_DATA" && echo x > "$FIXTURE_DATA/state" && "` + exe + `"`
	spec := LaunchSpec{
		Command:        []string{"sh", "-c", script, "sh", "${MCP_APP_DESKTOP_PROFILE_ID}"},
		Env:            map[string]string{"ID_ROOT": idRoot, "FIXTURE_DATA": "${MCP_APP_DESKTOP_PROFILE_DIR}/data"},
		Dir:            t.TempDir(),
		StartupTimeout: time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_, _ = m.Stop(sess.ID)
		}
	}()
	if sess.Profile == "" || !profileIDPattern.MatchString(sess.ProfileID) || filepath.Dir(sess.Profile) != m.profilesDir() {
		t.Fatalf("profile %q id %q", sess.Profile, sess.ProfileID)
	}
	private := filepath.Join(idRoot, "com.example.fixture."+sess.ProfileID)
	state := filepath.Join(sess.Profile, "data", "state")
	for _, path := range []string{private, state} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the app did not get its profile: %v", err)
		}
	}

	// The server goes away; the next one adopts the app with its profile.
	dir, id, pid := sess.Profile, sess.ProfileID, sess.PID
	m.Close()
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("closing the server must leave the profile of a running app: %v", err)
	}
	m2, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := m2.Launch(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	sess, m = adopted, m2
	if adopted.PID != pid || adopted.Profile != dir || adopted.ProfileID != id {
		t.Fatalf("adopted %+v, want pid %d profile %s id %s", adopted.Info, pid, dir, id)
	}
	if entries, _ := os.ReadDir(m2.profilesDir()); len(entries) != 1 {
		t.Fatalf("adoption must not create a second profile: %v", entries)
	}

	rep, err := m2.Stop(adopted.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopped = true
	if !rep.Terminated || len(rep.ProfileRemoved) != 2 || rep.Note != "" {
		t.Fatalf("stop report %+v", rep)
	}
	for _, path := range []string{dir, private} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s must be removed by stop (%v)", path, err)
		}
	}
	if _, err := os.Stat(real); err != nil {
		t.Fatalf("the real profile must be left alone: %v", err)
	}
}

// TestE2ETauriTargetPrivateProfile is the same for a built app started
// directly: the placeholder in env becomes a directory removed by stop.
func TestE2ETauriTargetPrivateProfile(t *testing.T) {
	exe := tauriFixture(t)
	useIdentifierRoot(t)
	m, err := NewManager(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := m.Launch(ctx, LaunchSpec{Target: exe, Env: map[string]string{"FIXTURE_DATA": "${MCP_APP_DESKTOP_PROFILE_DIR}"}, StartupTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	dir := sess.Profile
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || sess.ProfileID == "" || strings.Contains(sess.Note, "keeps its own data") {
		_, _ = m.Stop(sess.ID)
		t.Fatalf("profile %q (%v), note %q", dir, err, sess.Note)
	}
	rep, err := m.Stop(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(rep.ProfileRemoved) != 1 {
		t.Fatalf("stop must remove the profile: %v, report %+v", err, rep)
	}
}

// An app may leave directories its owner can no longer write or even list (a
// Python environment unpacked read-only): stop still removes its profile.
func TestProfileRemovalTakesBackReadOnlyDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not block removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root removes anything: the refusal cannot be reproduced")
	}
	p, err := newProfile(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(p.Dir, "data", "runtime", "python-env", "bin")
	sealed := filepath.Join(p.Dir, "data", "sealed", "inner")
	for _, dir := range []string{locked, sealed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	// Outside the profile, reached by a link inside it: it must survive.
	outside := filepath.Join(t.TempDir(), "kept")
	if err := os.WriteFile(outside, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.Dir, "data", "link")); err != nil {
		t.Fatal(err)
	}
	// A directory outside, read-only, reached by a link in the locked directory: the first
	// os.RemoveAll cannot remove that link, so the repair walk meets it, and must neither
	// change the directory's permissions nor empty it.
	outsideDir := filepath.Join(t.TempDir(), "kept-dir")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "file"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outsideDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outsideDir, 0o700) })
	if err := os.Symlink(outsideDir, filepath.Join(locked, "dir-link")); err != nil {
		t.Fatal(err)
	}
	// Read and search only, then nothing at all on the parent of another.
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(sealed), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Dir(sealed), 0o700)
		_ = os.Chmod(locked, 0o700)
	})
	if err := os.RemoveAll(p.Dir); err == nil {
		t.Fatal("the fixture must be one os.RemoveAll refuses")
	}
	removed, err := p.remove()
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(removed) == 0 || removed[0] != p.Dir {
		t.Fatalf("removed = %v, want %s first", removed, p.Dir)
	}
	if _, err := os.Stat(p.Dir); !os.IsNotExist(err) {
		t.Fatalf("the profile is still there: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "kept" {
		t.Fatalf("a file outside the profile was touched: %q %v", b, err)
	}
	if fi, err := os.Stat(outsideDir); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("a directory outside the profile was changed: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(outsideDir, "file")); err != nil || string(b) != "kept" {
		t.Fatalf("a directory outside the profile was emptied: %q %v", b, err)
	}
}
