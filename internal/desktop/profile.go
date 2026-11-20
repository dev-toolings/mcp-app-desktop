package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// A launch that names one of these placeholders in its argv or env gets a
// private profile: a directory of its own and an identifier segment of its
// own, both removed by stop. Electron targets have --user-data-dir; these are
// for what has no such switch: a Tauri app, which keeps its data under its
// bundle identifier, and any dev command whose app reads its data directory
// from its own variable.
//
//	command: ["bun","run","tauri","dev","--config","{\"identifier\":\"com.example.app.${MCP_APP_DESKTOP_PROFILE_ID}\"}"]
//	env:     {"APP_DATA_DIR": "${MCP_APP_DESKTOP_PROFILE_DIR}/data"}
//
// Nothing is created for a launch that names neither.
const (
	EnvProfileDir = "MCP_APP_DESKTOP_PROFILE_DIR"
	EnvProfileID  = "MCP_APP_DESKTOP_PROFILE_ID"
)

// profileIDPattern is the only shape removeIdentifierData accepts: the prefix
// and 12 random hexadecimal digits, so a name ending with it can only come
// from an app launched with it.
var profileIDPattern = regexp.MustCompile(`^mcp[0-9a-f]{12}$`)

// profile is the private profile of one launched application.
type profile struct {
	// Dir is the private directory (0700).
	Dir string
	// ID is the identifier segment, valid in a bundle identifier and in a
	// file name.
	ID string
}

// wantsProfile reports whether a launch names a profile placeholder.
func wantsProfile(argv []string, env map[string]string) bool {
	names := func(s string) bool { return strings.Contains(s, EnvProfileDir) || strings.Contains(s, EnvProfileID) }
	for _, a := range argv {
		if names(a) {
			return true
		}
	}
	for _, v := range env {
		if names(v) {
			return true
		}
	}
	return false
}

// newProfile creates a private profile under root.
func newProfile(root string) (*profile, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("create private profile: %w", err)
	}
	id := "mcp" + hex.EncodeToString(buf)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create private profile: %w", err)
	}
	dir := filepath.Join(root, id)
	// Mkdir, not MkdirAll: a directory already there is not ours to reuse.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create private profile: %w", err)
	}
	return &profile{Dir: dir, ID: id}, nil
}

// expand substitutes ${NAME} and $NAME for the profile placeholders in s.
func (p *profile) expand(s string) string {
	if p == nil {
		return s
	}
	return strings.NewReplacer(
		"${"+EnvProfileDir+"}", p.Dir, "$"+EnvProfileDir, p.Dir,
		"${"+EnvProfileID+"}", p.ID, "$"+EnvProfileID, p.ID,
	).Replace(s)
}

// profilesDir holds the private profiles, next to the dev commands' state: a
// dev command outlives the server that launched it, and so does its profile.
func (m *Manager) profilesDir() string { return filepath.Join(m.devDir(), "profiles") }

// recordedProfile is the profile a dev command's state file names, or nil
// when it names none. The file is read back from disk: only an id of the
// generated shape and the directory this server would have created for it
// are accepted, so a damaged or edited state can never point a removal
// anywhere else.
func (m *Manager) recordedProfile(st *devState) *profile {
	if st == nil || !profileIDPattern.MatchString(st.ProfileID) || st.ProfileDir != filepath.Join(m.profilesDir(), st.ProfileID) {
		return nil
	}
	return &profile{Dir: st.ProfileDir, ID: st.ProfileID}
}

// identifierDataRoots lists the directories where the operating system and
// Tauri keep an app's data under its bundle identifier. A variable, for the
// tests.
var identifierDataRoots = func() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		lib := filepath.Join(home, "Library")
		return []string{
			filepath.Join(lib, "Application Support"), filepath.Join(lib, "Caches"), filepath.Join(lib, "WebKit"),
			filepath.Join(lib, "Logs"), filepath.Join(lib, "HTTPStorages"), filepath.Join(lib, "Saved Application State"),
			filepath.Join(lib, "Preferences"),
		}
	case "windows":
		var roots []string
		for _, k := range []string{"APPDATA", "LOCALAPPDATA"} {
			if v := os.Getenv(k); v != "" {
				roots = append(roots, v)
			}
		}
		return roots
	default:
		xdg := func(env, fallback string) string {
			if v := os.Getenv(env); v != "" {
				return v
			}
			return filepath.Join(home, fallback)
		}
		return []string{xdg("XDG_DATA_HOME", ".local/share"), xdg("XDG_CONFIG_HOME", ".config"), xdg("XDG_CACHE_HOME", ".cache")}
	}
}

// identifierDataSuffixes are what follows the identifier in an entry of
// identifierDataRoots: nothing, or the suffix the system adds.
var identifierDataSuffixes = []string{"", ".savedState", ".plist", ".binarycookies"}

// remove deletes the private directory and what an app launched with the
// identifier segment left under its bundle identifier. It returns what it
// removed; an entry it could not remove is reported, never hidden.
func (p *profile) remove() (removed []string, err error) {
	var errs []error
	var paths []string
	if p.Dir != "" {
		paths = append(paths, p.Dir)
	}
	for _, path := range append(paths, identifierData(p.ID)...) {
		var rmErr error
		// A process that just exited may still hold a file for a moment
		// (Windows; an indexer on macOS).
		for i := 0; i < 5; i++ {
			if rmErr = removeTree(path); rmErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if rmErr != nil {
			errs = append(errs, rmErr)
			continue
		}
		removed = append(removed, path)
	}
	return removed, errors.Join(errs...)
}

// removeTree deletes path and what it holds, including directories the app
// left without write or search permission for their owner (a Python
// environment unpacked read-only, a Go module cache): os.RemoveAll stops on
// those with « permission denied ». Only when it does are the directories
// under path given back to their owner, then removal is tried again. Nothing
// outside path is touched: a symbolic link is removed, never followed.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return err
	}
	// WalkDir hands a directory over before it reads it, so the chmod is in
	// time for its own listing. What cannot be changed is left to RemoveAll,
	// whose error names it.
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// identifierData lists the entries of identifierDataRoots named after a
// bundle identifier that ends with the segment id ("com.example.app.<id>",
// plus the system's suffixes). Only direct children are considered, and only
// for an id of the generated shape: a real identifier never matches.
func identifierData(id string) []string {
	if !profileIDPattern.MatchString(id) {
		return nil
	}
	var out []string
	for _, root := range identifierDataRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			for _, suffix := range identifierDataSuffixes {
				if strings.HasSuffix(e.Name(), "."+id+suffix) {
					out = append(out, filepath.Join(root, e.Name()))
					break
				}
			}
		}
	}
	return out
}
