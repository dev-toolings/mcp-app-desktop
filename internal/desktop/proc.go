package desktop

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ownedProc is a started process group plus every descendant seen while it
// ran. Electron apps spawn helpers (GPU, renderers, utility processes) and
// sometimes detached services that outlive the group; they are found through
// the descendant record and the launch marker (the private profile path).
type ownedProc struct {
	pid int
	cmd *exec.Cmd
	// adopted is a dev command started by an earlier server: no exec.Cmd,
	// its exit is noticed by polling rootCmd.
	adopted bool
	rootCmd string
	ring    *Ring
	marker  string
	done    chan struct{}
	err     error

	mu   sync.Mutex
	seen map[int]string // pid → command line at first sighting
	stop chan struct{}
	once sync.Once
}

// startOwned starts exe in its own process group, with stdout and stderr
// flowing into ring.
func startOwned(exe string, args, env []string, dir string, ring *Ring, marker string) (*ownedProc, error) {
	return startOwnedTo(exe, args, env, dir, ring.Writer("stdout"), ring.Writer("stderr"), ring, marker)
}

// startOwnedTo is startOwned with the outputs given (files for a dev command).
func startOwnedTo(exe string, args, env []string, dir string, stdout, stderr io.Writer, ring *Ring, marker string) (*ownedProc, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env, cmd.Dir = env, dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", exe, err)
	}
	p := &ownedProc{pid: cmd.Process.Pid, cmd: cmd, ring: ring, marker: marker, done: make(chan struct{}), seen: map[int]string{}, stop: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	go p.watch()
	return p, nil
}

// adoptOwned follows a process group an earlier server started.
func adoptOwned(pid int, rootCmd string, ring *Ring) *ownedProc {
	p := &ownedProc{pid: pid, adopted: true, rootCmd: rootCmd, ring: ring, done: make(chan struct{}), seen: map[int]string{}, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for rootCommand(pid) == rootCmd {
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
		}
		p.err = fmt.Errorf("process %d exited", pid)
		close(p.done)
	}()
	go p.watch()
	return p
}

// release stops following the process without signalling it.
func (p *ownedProc) release() { p.once.Do(func() { close(p.stop) }) }

func (p *ownedProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// exitError describes how the process ended, once it has.
func (p *ownedProc) exitError() string {
	if !p.exited() {
		return ""
	}
	if p.err == nil {
		return "exit status 0"
	}
	return p.err.Error()
}

// procRefreshInterval paces the `ps` scans that record the process tree;
// terminate and Usage refresh on demand, so a slow pace costs nothing.
const procRefreshInterval = 5 * time.Second

func (p *ownedProc) watch() {
	t := time.NewTicker(procRefreshInterval)
	defer t.Stop()
	for {
		p.refresh()
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

type psEntry struct {
	pid, ppid int
	rssKB     int64
	cpu       float64
	cmd       string
}

func ps() []psEntry {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,%cpu=,command=").Output()
	if err != nil {
		return nil
	}
	return parsePS(string(out))
}

// parsePS reads `ps -axo pid=,ppid=,rss=,%cpu=,command=` output.
func parsePS(out string) []psEntry {
	var list []psEntry
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.ParseInt(f[2], 10, 64)
		cpu, e4 := strconv.ParseFloat(f[3], 64)
		if e1 != nil || e2 != nil {
			continue
		}
		if e3 != nil {
			rss = 0
		}
		if e4 != nil {
			cpu = 0
		}
		list = append(list, psEntry{pid, ppid, rss, cpu, strings.Join(f[4:], " ")})
	}
	return list
}

// Usage sums the resident memory and CPU share of a live process tree.
type Usage struct {
	Processes  int     `json:"processes"`
	RSSBytes   int64   `json:"rss_bytes"`
	CPUPercent float64 `json:"cpu_percent"`
}

// usage measures the root and every live descendant now (one ps scan).
func (p *ownedProc) usage() Usage {
	list := ps()
	children := map[int][]psEntry{}
	byPID := map[int]psEntry{}
	for _, e := range list {
		children[e.ppid] = append(children[e.ppid], e)
		byPID[e.pid] = e
	}
	var u Usage
	queue := []int{p.pid}
	seen := map[int]bool{}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		if e, ok := byPID[c]; ok {
			u.Processes++
			u.RSSBytes += e.rssKB * 1024
			u.CPUPercent += e.cpu
		}
		for _, e := range children[c] {
			queue = append(queue, e.pid)
		}
	}
	// Helpers that re-parented themselves (the marker) still belong to the app.
	if p.marker != "" {
		for _, e := range list {
			if !seen[e.pid] && strings.Contains(e.cmd, p.marker) {
				seen[e.pid] = true
				u.Processes++
				u.RSSBytes += e.rssKB * 1024
				u.CPUPercent += e.cpu
			}
		}
	}
	return u
}

// refresh records the current descendant tree and returns it (root included).
// Recorded processes that have exited are forgotten: an app that keeps
// spawning short-lived commands would otherwise grow the record for the life
// of the session, and a dead pid can only come back recycled by a stranger.
func (p *ownedProc) refresh() map[int]bool {
	list := ps()
	children := map[int][]psEntry{}
	live := make(map[int]string, len(list))
	for _, e := range list {
		children[e.ppid] = append(children[e.ppid], e)
		live[e.pid] = e.cmd
	}
	tree := map[int]bool{p.pid: true}
	queue := []int{p.pid}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(list) > 0 {
		for pid, cmd := range p.seen {
			if c, ok := live[pid]; !ok || c != cmd {
				delete(p.seen, pid)
			}
		}
	}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, e := range children[c] {
			if !tree[e.pid] {
				tree[e.pid] = true
				queue = append(queue, e.pid)
				if _, ok := p.seen[e.pid]; !ok {
					p.seen[e.pid] = e.cmd
				}
			}
		}
	}
	if p.marker != "" {
		for _, e := range list {
			if strings.Contains(e.cmd, p.marker) {
				if _, ok := p.seen[e.pid]; !ok {
					p.seen[e.pid] = e.cmd
				}
			}
		}
	}
	return tree
}

// descendants returns the pids recorded so far (root excluded).
func (p *ownedProc) descendants() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.seen))
	for pid := range p.seen {
		out = append(out, pid)
	}
	return out
}

// terminateGrace bounds the SIGTERM phase of terminate before SIGKILL.
var terminateGrace = 10 * time.Second

// terminate stops the group, then every recorded descendant still running
// with the same command line (so a recycled pid is never signalled). It
// returns the pids that survived SIGKILL.
//
// Descendants are stopped top-down: SIGTERM goes only to the topmost live
// processes, and a child is signalled once its parent has gone. A service
// that stops its own workers otherwise sees them die under it, cannot
// confirm its shutdown, and is SIGKILLed holding its locks.
func (p *ownedProc) terminate() []int {
	p.refresh()
	alivePoll := p.adopted && !p.exited()
	p.release()
	if p.cmd != nil || alivePoll {
		signalGroup(p.pid, false)
	}
	p.mu.Lock()
	seen := make(map[int]string, len(p.seen))
	for k, v := range p.seen {
		seen[k] = v
	}
	p.mu.Unlock()
	// alive maps each live tracked pid to its parent pid.
	alive := func() map[int]int {
		out := map[int]int{}
		for _, e := range ps() {
			if cmd, ok := seen[e.pid]; ok && cmd == e.cmd {
				out[e.pid] = e.ppid
			}
		}
		if p.cmd != nil && !p.exited() || p.adopted && rootCommand(p.pid) == p.rootCmd {
			out[p.pid] = 0
		}
		return out
	}
	signalled := map[int]bool{}
	for deadline := time.Now().Add(terminateGrace); ; {
		live := alive()
		if len(live) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		for pid, ppid := range live {
			if _, parentLive := live[ppid]; !parentLive && !signalled[pid] {
				signalled[pid] = true
				signalPID(pid, false)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	for pid := range alive() {
		signalPID(pid, true)
	}
	if p.cmd != nil || alivePoll {
		signalGroup(p.pid, true)
	}
	time.Sleep(300 * time.Millisecond)
	var survivors []int
	for pid := range alive() {
		survivors = append(survivors, pid)
	}
	return survivors
}
