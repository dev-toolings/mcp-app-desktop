package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A dev command is the developer's working app: it outlives the server that
// launched it. Its output goes to files instead of pipes (a pipe closed with
// the server would kill it on its next write), and a state file written at
// launch lets the next server adopt it instead of starting a second copy.
// Only an explicit stop terminates it.

// devState is what a later server needs to adopt a running dev command.
type devState struct {
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Command  []string `json:"command"`
	Inspect  bool     `json:"inspect"`
	PID      int      `json:"pid"`
	PGID     int      `json:"pgid"`
	RootCmd  string   `json:"root_cmd"`
	CDPPort  int      `json:"cdp_port"`
	InspPort int      `json:"inspect_port,omitempty"`
	// A Tauri dev command is reached through its bridge (token included:
	// the file is private to the user, 0600).
	BridgePort int       `json:"bridge_port,omitempty"`
	Token      string    `json:"bridge_token,omitempty"`
	Framework  string    `json:"framework,omitempty"`
	Backend    string    `json:"backend,omitempty"`
	Started    time.Time `json:"started"`
	StdoutLog  string    `json:"stdout_log"`
	StderrLog  string    `json:"stderr_log"`
}

// devKey identifies a dev command by its directory and unexpanded argv.
func devKey(dir string, command []string, inspect bool) string {
	h := sha256.New()
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	fmt.Fprintf(h, "%s\x00%t", abs, inspect)
	for _, a := range command {
		fmt.Fprintf(h, "\x00%s", a)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// devDir holds the state and log files, next to the sessions' artifacts.
func (m *Manager) devDir() string { return filepath.Join(filepath.Dir(m.root), "dev") }

func (m *Manager) devStatePath(key string) string {
	return filepath.Join(m.devDir(), key+".json")
}

func readDevState(path string) (*devState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st devState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.PID <= 0 || st.CDPPort <= 0 {
		return nil, errors.New("incomplete dev state")
	}
	return &st, nil
}

// writeDevState replaces the file atomically: a reader never sees half of it.
func writeDevState(path string, st *devState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeDevState(path string, st *devState) {
	_ = os.Remove(path)
	if st != nil {
		_ = os.Remove(st.StdoutLog)
		_ = os.Remove(st.StderrLog)
	}
}

// rootCommand reads the command line of pid, "" when it is gone.
func rootCommand(pid int) string {
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// stillOurs is true while the recorded root runs: same pid, same process
// group and same command line, so a recycled pid is never adopted.
func (st *devState) stillOurs() bool {
	if st.RootCmd == "" || rootCommand(st.PID) != st.RootCmd {
		return false
	}
	pgid, err := processGroupOf(st.PID)
	return err == nil && pgid == st.PGID
}

// fileTail copies what is appended to a log file into the ring.
type fileTail struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// tailFile follows path from offset; a negative offset starts that many
// bytes before the current end (the last lines of an adopted app), at the
// next line start so no cut line is shown. The start position is fixed
// before tailFile returns: a line written right after is never skipped (the
// goroutine used to open the file late and measure the end after the new
// line, 2026-10-24).
func tailFile(path string, offset int64, w io.Writer) *fileTail {
	t := &fileTail{stop: make(chan struct{}), done: make(chan struct{})}
	f, skipPartial := openTail(path, offset)
	go func() {
		defer close(t.done)
		defer func() {
			if f != nil {
				f.Close()
			}
		}()
		buf := make([]byte, 64*1024)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			if f == nil {
				// Not there yet: it is new, so all of it is fresh output.
				f, skipPartial = openTail(path, 0)
			}
			for f != nil {
				n, err := f.Read(buf)
				chunk := buf[:n]
				if skipPartial && n > 0 {
					if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
						chunk, skipPartial = chunk[i+1:], false
					} else {
						chunk = nil
					}
				}
				if len(chunk) > 0 {
					_, _ = w.Write(chunk)
				}
				if err != nil || n == 0 {
					break
				}
			}
			select {
			case <-t.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return t
}

// openTail opens path at offset (negative: from the end). skipPartial is
// true when it starts in the middle of a line.
func openTail(path string, offset int64) (*os.File, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	if offset < 0 {
		end := int64(0)
		if info, err := f.Stat(); err == nil {
			end = info.Size()
		}
		offset = max(end+offset, 0)
	}
	// Inside the file, the line is cut unless the byte before ends one.
	cut := false
	if offset > 0 {
		prev := make([]byte, 1)
		cut = true
		if n, _ := f.ReadAt(prev, offset-1); n == 1 && prev[0] == '\n' {
			cut = false
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, false
	}
	return f, cut
}

func (t *fileTail) close() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	<-t.done
}

// openDevLogs creates (truncates) the two output files of a new launch.
func openDevLogs(dir, key string) (stdout, stderr *os.File, err error) {
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	stdout, err = os.OpenFile(filepath.Join(dir, key+".stdout.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	stderr, err = os.OpenFile(filepath.Join(dir, key+".stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}
