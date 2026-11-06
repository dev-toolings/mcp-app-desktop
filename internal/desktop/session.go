package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver/cdpwin"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
	"github.com/go-rod/rod"
)

// State of a session's application.
type State string

const (
	StateRunning    State = "running"
	StateRestarting State = "restarting" // the debugging endpoint stopped answering; waiting for it to come back
	StateExited     State = "exited"     // the owned process ended
	StateStopped    State = "stopped"
)

// keepPaintingArgs are Chromium switches for an Electron app driven in the
// background: a window covered by other windows then stays "visible" and
// keeps running requestAnimationFrame and timers.
var keepPaintingArgs = []string{"--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding"}

// LaunchSpec describes what to start.
type LaunchSpec struct {
	// Target is an Electron project directory, a .app bundle or an Electron
	// executable. Ignored when Command is set.
	Target string
	// Command starts the app through a development command (["bun","run",
	// "dev"]); it must hand the ports in MCP_APP_DESKTOP_CDP_PORT and
	// MCP_APP_DESKTOP_INSPECT_PORT (env or ${…} placeholders) to electron.
	Command []string
	// Args are extra application arguments (Target launches only).
	Args []string
	Env  map[string]string
	// Dir is the working directory (defaults to the project or target dir).
	Dir string
	// Isolate gives a Target launch a private --user-data-dir (default true).
	Isolate bool
	// Inspect opens the main process's Node inspector (default true).
	Inspect        bool
	StartupTimeout time.Duration
	// Name overrides the display name.
	Name string
	// Framework is a hint for a Command launch (FrameworkElectron,
	// FrameworkTauri, or empty: whichever answers first). Target launches
	// recognise their framework themselves.
	Framework string
}

// AttachSpec selects a running application.
type AttachSpec struct {
	// Endpoint is the remote debugging endpoint (http://127.0.0.1:PORT) of
	// an Electron app, or the bridge port of a Tauri app (127.0.0.1:PORT).
	Endpoint string
	// Inspector is the main process inspector endpoint, when known.
	Inspector string
	// Token authenticates to a Tauri bridge started with
	// MCP_APP_DESKTOP_BRIDGE_TOKEN.
	Token string
	Name  string
}

// Frameworks a session can drive.
const (
	FrameworkElectron = "electron"
	FrameworkTauri    = "tauri"
)

// Info is the identity of a session: what the agent reads back.
type Info struct {
	ID   string `json:"session_id"`
	Name string `json:"name"`
	// Framework is FrameworkElectron or FrameworkTauri.
	Framework string `json:"framework"`
	// Backend is how windows are driven: driver.BackendCDP (Electron,
	// Tauri on Windows) or driver.BackendBridge (Tauri bridge plugin).
	Backend string `json:"backend"`
	// Mode is "target" (electron binary started here), "command" (dev
	// command started here) or "attach".
	Mode string `json:"mode"`
	// Owned is true when the session started the application and will stop it.
	Owned bool `json:"owned"`
	// Dir holds this session's artifacts (screenshots, flow outputs).
	Dir     string    `json:"artifacts_dir"`
	Started time.Time `json:"started"`
	// Endpoint is the CDP endpoint (http://127.0.0.1:PORT) or the bridge
	// address (127.0.0.1:PORT), per Backend.
	Endpoint  string `json:"endpoint"`
	Inspector string `json:"inspector_endpoint,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Profile   string `json:"profile_dir,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Session is one driven application.
type Session struct {
	Info

	// lastUse is the last tool or flow access (Manager.Get); idleAfter and
	// onIdle stop an owned app nobody uses.
	lastUse   time.Time
	idleAfter time.Duration
	onIdle    func()
	idleFired bool
	ticks     int
	usage     Usage
	usageAt   time.Time

	mu      sync.Mutex
	browser *engine.Browser
	main    *MainClient
	// Tauri bridge (Backend bridge): the connection, the token the app was
	// started with, and what its webviews logged.
	bridge     *tauri.App
	token      string
	logPoller  *tauri.LogPoller
	bridgeLogs []LogEntry
	// Endpoints offered to a launched app before it is known which one
	// answers (a dev command may start Electron or Tauri).
	cdpCandidate    string
	bridgeCandidate string
	proc            *ownedProc
	ring            *Ring
	root            string // private temp root (profile), removed on stop
	state           State
	since           time.Time // when the current state began
	generation      int
	restarts        []time.Time
	lastErr         string
	stopped         bool
	dialogs         *engine.DialogAutoPolicy
	hubbed          map[string]bool
	stopWatch       chan struct{}
	actions         actionGate
	// A dev command keeps a state file (devPath) so the next server can
	// adopt it; its output is followed from log files (tails).
	dev     *devState
	devPath string
	tails   []*fileTail
}

// actionGate serialises UI actions on one session. Unlike a mutex, a caller
// waits for it with a deadline and learns who holds it: an action stuck on a
// window that stopped answering used to queue every later call of the
// session behind it, up to the 3-minute ceiling, with no word of why.
type actionGate struct {
	once   sync.Once
	slot   chan struct{}
	mu     sync.Mutex
	holder string
	since  time.Time
}

func (g *actionGate) ch() chan struct{} {
	g.once.Do(func() { g.slot = make(chan struct{}, 1) })
	return g.slot
}

// BusyError answers a call that waited too long for another action of the
// same session. Other sessions, and the tools that do not take the gate
// (status, logs, windows, snapshot, screenshot, eval), are not affected.
type BusyError struct {
	Session string
	Tool    string
	For     time.Duration
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("session %s is busy: %s has been running for %s (a hidden window, a page that stopped answering, or a long wait). "+
		"Other sessions are not affected, and snapshot, screenshot, eval, status and trace answer meanwhile. Retry once it ends",
		e.Session, e.Tool, e.For.Round(time.Second))
}

// AcquireActions takes the session's action gate for tool, waiting at most
// wait (and never past ctx). release must be called exactly once.
func (s *Session) AcquireActions(ctx context.Context, tool string, wait time.Duration) (release func(), err error) {
	g := &s.actions
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.ch() <- struct{}{}:
	case <-timer.C:
		return nil, s.busy()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, s.busy()
		}
		return nil, ctx.Err()
	}
	g.mu.Lock()
	g.holder, g.since = tool, time.Now()
	g.mu.Unlock()
	var released sync.Once
	return func() {
		released.Do(func() {
			g.mu.Lock()
			g.holder, g.since = "", time.Time{}
			g.mu.Unlock()
			<-g.ch()
		})
	}, nil
}

func (s *Session) busy() error {
	g := &s.actions
	g.mu.Lock()
	defer g.mu.Unlock()
	e := &BusyError{Session: s.ID, Tool: g.holder}
	if e.Tool == "" {
		// Released between the timeout and this read: the caller may retry now.
		e.Tool = "another action"
	} else {
		e.For = time.Since(g.since)
	}
	return e
}

// Busy reports the action holding the gate, for status ("" when idle).
func (s *Session) Busy() (tool string, since time.Time) {
	g := &s.actions
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holder, g.since
}

// Status is the agent-facing view of a session.
type Status struct {
	Info
	State      State       `json:"state"`
	StateSince time.Time   `json:"state_since"`
	Generation int         `json:"generation"`
	Restarts   []time.Time `json:"restarts,omitempty"`
	LastError  string      `json:"last_error,omitempty"`
	MainProc   bool        `json:"main_inspector_connected"`
	Descendant int         `json:"descendant_processes,omitempty"`
	// Usage is the live process tree (owned sessions only; measured at
	// most once per second).
	Usage *Usage `json:"usage,omitempty"`
	// IdleFor is the time since the last tool access; IdleStopIn is the
	// remaining time before an unused owned app is stopped (0: never).
	IdleFor    time.Duration `json:"idle_for"`
	IdleStopIn time.Duration `json:"idle_stop_in,omitempty"`
}

// Status snapshots the session state without touching the application.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Info: s.Info, State: s.state, StateSince: s.since, Generation: s.generation, Restarts: append([]time.Time(nil), s.restarts...), LastError: s.lastErr, MainProc: s.main != nil}
	if s.proc != nil {
		st.Descendant = len(s.proc.descendants())
		if !s.stopped && s.state != StateExited {
			if time.Since(s.usageAt) > time.Second {
				s.usage, s.usageAt = s.proc.usage(), time.Now()
			}
			u := s.usage
			st.Usage = &u
		}
	}
	st.IdleFor = time.Since(s.lastUse)
	if s.idleAfter > 0 && !s.stopped {
		st.IdleStopIn = max(s.idleAfter-st.IdleFor, 0)
	}
	return st
}

// Touch records a use of the session, deferring the idle stop.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastUse = time.Now()
	s.mu.Unlock()
}

// State returns the current state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Browser returns the live connection, reconnecting once if the endpoint
// came back since the last probe. It fails with the session state when the
// application is restarting, exited or unreachable.
func (s *Session) Browser(ctx context.Context) (*engine.Browser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("session %s is stopped", s.ID)
	}
	if s.Backend == driver.BackendBridge {
		return nil, fmt.Errorf("%s is a Tauri app driven through its bridge plugin (%s): it has no DevTools Protocol endpoint", s.Name, s.Endpoint)
	}
	if s.browser != nil && s.browser.Alive(2*time.Second) {
		return s.browser, nil
	}
	// A dead connection to an endpoint that still answers (Electron relaunched on the same port, a dropped websocket) was kept:
	// every tool then failed with « use of closed network connection » while status said running (2026-10-22). Drop it so
	// the tick reconnects.
	if s.browser != nil {
		s.detachLocked()
	}
	s.tickLocked(ctx)
	if s.browser != nil {
		return s.browser, nil
	}
	return nil, s.unavailableLocked()
}

func (s *Session) unavailableLocked() error {
	switch s.state {
	case StateRestarting:
		return fmt.Errorf("%s is restarting since %s (its %s endpoint %s stopped answering: a dev loop recompile, a crash or a relaunch). Retry in a moment, or read logs for the cause", s.Name, s.since.Format("15:04:05"), s.Backend, s.Endpoint)
	case StateExited:
		tail := ""
		if s.ring != nil {
			tail = "\nlast output:\n" + s.ring.Tail(15)
		}
		return fmt.Errorf("%s exited (%s)%s", s.Name, s.lastErr, tail)
	case StateStopped:
		return fmt.Errorf("session %s is stopped", s.ID)
	}
	return fmt.Errorf("%s is not reachable at %s: %s", s.Name, s.Endpoint, s.lastErr)
}

// Main returns the main process inspector client, if connected.
func (s *Session) Main() *MainClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.main
}

// Dialogs returns the JavaScript dialog policy applied to every window.
func (s *Session) Dialogs() *engine.DialogAutoPolicy { return s.dialogs }

// App returns the live connection as a driver.App, reconnecting like
// Browser when needed.
func (s *Session) App(ctx context.Context) (driver.App, error) {
	if s.Backend == driver.BackendBridge {
		return s.bridgeApp(ctx)
	}
	b, err := s.Browser(ctx)
	if err != nil {
		return nil, err
	}
	return cdpwin.New(b, s.preparePage), nil
}

// bridgeApp returns the live bridge connection, reconnecting once when the
// app came back since the last probe.
func (s *Session) bridgeApp(ctx context.Context) (*tauri.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fmt.Errorf("session %s is stopped", s.ID)
	}
	if s.bridge != nil && !s.bridge.Client().Closed() {
		return s.bridge, nil
	}
	s.tickLocked(ctx)
	if s.bridge != nil {
		return s.bridge, nil
	}
	return nil, s.unavailableLocked()
}

// TauriApp returns the bridge connection of a Tauri session (nil, error for
// a CDP session).
func (s *Session) TauriApp(ctx context.Context) (*tauri.App, error) {
	if s.Backend != driver.BackendBridge {
		return nil, fmt.Errorf("%s is not driven through the Tauri bridge", s.Name)
	}
	return s.bridgeApp(ctx)
}

// dialogPolicy reads the JavaScript dialog policy at call time.
func (s *Session) dialogPolicy() (bool, string) { return s.dialogs.Snapshot() }

// Window returns one window of the application, bound to ctx.
func (s *Session) Window(ctx context.Context, index int) (driver.Window, error) {
	app, err := s.App(ctx)
	if err != nil {
		return nil, err
	}
	return app.Window(ctx, index)
}

// Page returns the page for a window index through a live connection.
func (s *Session) Page(ctx context.Context, window int) (*engine.Browser, *rod.Page, error) {
	b, err := s.Browser(ctx)
	if err != nil {
		return nil, nil, err
	}
	page, err := b.Page(window)
	if err != nil {
		return nil, nil, err
	}
	s.preparePage(b, page)
	// The caller's context bounds every CDP call on the returned page, so a
	// cancelled tool call or an interrupted run-flow stops waiting at once;
	// the event hubs stay on the session-scoped page.
	return b, page.Context(ctx), nil
}

// preparePage starts the event hub (console, exceptions, network) and the
// dialog handler on a page the first time it is used, and keeps the page
// running at full rate while its window is hidden (engine.KeepActive). The
// set of prepared pages is reset with every new connection, so a page of an
// app the dev loop restarted is prepared again.
func (s *Session) preparePage(b *engine.Browser, page *rod.Page) {
	key := string(page.TargetID)
	s.mu.Lock()
	if s.hubbed == nil {
		s.hubbed = map[string]bool{}
	}
	done := s.hubbed[key]
	s.hubbed[key] = true
	s.mu.Unlock()
	if done {
		return
	}
	_, _ = engine.StartEventHub(page)
	engine.StartDialogAutoHandler(page, s.dialogs)
	if err := engine.KeepActive(page, keepActiveTimeout); err != nil && s.ring != nil {
		s.ring.Add("mcp-app-desktop", fmt.Sprintf("window %s may be throttled while hidden: %v", key, err))
	}
}

// keepActiveTimeout bounds the CDP calls that keep a page active.
const keepActiveTimeout = 3 * time.Second

func (s *Session) ensureHubsLocked() {
	if s.browser == nil {
		return
	}
	wins, err := s.browser.Windows()
	if err != nil {
		return
	}
	b := s.browser
	s.mu.Unlock()
	for _, w := range wins {
		if page, err := b.Page(w.Index); err == nil {
			s.preparePage(b, page)
		}
	}
	s.mu.Lock()
}

// tickLocked is one step of the watch loop: it notices an exited process, a
// vanished endpoint, and reconnects when the endpoint answers again.
func (s *Session) tickLocked(ctx context.Context) {
	if s.stopped {
		return
	}
	if s.proc != nil && s.proc.exited() && s.state != StateExited {
		s.setStateLocked(StateExited)
		s.lastErr = s.proc.exitError()
		s.detachLocked()
		return
	}
	if s.state == StateExited {
		return
	}
	if s.Backend == driver.BackendBridge {
		s.tickBridgeLocked(ctx)
		return
	}
	_, ok := probeCDP(ctx, s.Endpoint)
	if !ok {
		if s.state == StateRunning {
			s.setStateLocked(StateRestarting)
			s.detachLocked()
		}
		return
	}
	if s.browser == nil {
		b, err := engine.Connect(ctx, s.Endpoint, 30*time.Second)
		if err != nil {
			s.lastErr = err.Error()
			return
		}
		s.browser = b
		s.recordReconnectLocked()
		s.lastErr = ""
		s.setStateLocked(StateRunning)
	}
	if s.main == nil && s.Inspector != "" {
		if m, err := ConnectMain(ctx, s.Inspector); err == nil {
			s.main = m
		}
	}
	s.ensureHubsLocked()
}

// tickBridgeLocked follows a Tauri app through its bridge: a closed
// connection means the app is gone (a dev loop rebuild, a crash), and a
// bridge answering again on the same port is the app back.
func (s *Session) tickBridgeLocked(ctx context.Context) {
	if s.bridge != nil && !s.bridge.Client().Closed() {
		return
	}
	if s.bridge != nil {
		s.detachLocked()
	}
	if !tauri.Probe(ctx, s.Endpoint) {
		if s.state == StateRunning {
			s.setStateLocked(StateRestarting)
		}
		return
	}
	c, err := tauri.Dial(ctx, s.Endpoint, s.token)
	if err != nil {
		s.lastErr = err.Error()
		if s.state == StateRunning {
			s.setStateLocked(StateRestarting)
		}
		return
	}
	s.bridge = tauri.NewApp(c, s.dialogPolicy)
	if c.Hello.PID > 0 && !s.Owned {
		s.PID = c.Hello.PID
	}
	s.recordReconnectLocked()
	s.lastErr = ""
	s.setStateLocked(StateRunning)
}

// recordReconnectLocked counts a new connection. Every connection after the
// first follows a lost one: a restart the probe saw (state restarting), or
// one fast enough to happen between two probes, noticed by Browser as a dead
// connection to an endpoint that answers again. Both count, so the restart
// list and the generation agree (status read the last restart of an empty
// list and panicked, 2026-10-24).
func (s *Session) recordReconnectLocked() {
	if s.generation > 0 {
		s.restarts = append(s.restarts, time.Now())
		if len(s.restarts) > maxRestartsKept {
			s.restarts = append(s.restarts[:0:0], s.restarts[len(s.restarts)-maxRestartsKept:]...)
		}
	}
	s.generation++
}

// maxRestartsKept bounds the restart times a session remembers; Generation
// still counts every connection.
const maxRestartsKept = 50

func (s *Session) setStateLocked(st State) {
	if s.state != st {
		s.state = st
		s.since = time.Now()
	}
}

// detachLocked drops the CDP, inspector and bridge connections (the app may
// be gone).
func (s *Session) detachLocked() {
	if s.bridge != nil {
		s.bridge.Client().Close()
		s.bridge = nil
	}
	if s.browser != nil {
		s.browser.Close()
		s.browser = nil
	}
	if s.main != nil {
		s.main.Close()
		s.main = nil
	}
	s.hubbed = nil
}

func (s *Session) watch() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopWatch:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s.mu.Lock()
		s.ticks++
		// A healthy app is probed every third tick; a restarting one every
		// tick so it is picked up quickly.
		if s.state != StateRunning || s.ticks%3 == 0 || (s.proc != nil && s.proc.exited()) {
			s.tickLocked(ctx)
		}
		idle := s.idleAfter > 0 && !s.idleFired && !s.stopped && time.Since(s.lastUse) > s.idleAfter
		if idle {
			s.idleFired = true
		}
		if idle && s.ring != nil {
			s.ring.Add("mcp-app-desktop", fmt.Sprintf("stopped after %s without any tool access (idle timeout, --idle-timeout)", s.idleAfter))
		}
		onIdle := s.onIdle
		var pollApp *tauri.App
		if s.bridge != nil && s.state == StateRunning && s.ticks%2 == 0 {
			pollApp = s.bridge
		}
		s.mu.Unlock()
		cancel()
		if pollApp != nil {
			s.pollBridgeLogs(pollApp)
		}
		if idle && onIdle != nil {
			go onIdle()
			return
		}
	}
}

// maxBridgeLogs bounds what a Tauri session keeps of its webviews' logs.
const maxBridgeLogs = 3000

// pollBridgeLogs drains the webviews' log buffers into the session.
func (s *Session) pollBridgeLogs(app *tauri.App) {
	s.mu.Lock()
	if s.logPoller == nil {
		s.logPoller = tauri.NewLogPoller()
	}
	poller := s.logPoller
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries := poller.Poll(ctx, app)
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		s.bridgeLogs = append(s.bridgeLogs, LogEntry{Time: e.Time, Source: e.Source, Level: e.Level, Text: e.Text, Location: e.Location, Window: e.Window})
	}
	if n := len(s.bridgeLogs); n > maxBridgeLogs {
		s.bridgeLogs = append(s.bridgeLogs[:0:0], s.bridgeLogs[n-maxBridgeLogs:]...)
	}
}

// Logs merges renderer console messages, exceptions and failed requests of
// every window (a Tauri app's console, exceptions and IPC calls), the main
// process console, and the process output, oldest first.
func (s *Session) Logs() []LogEntry {
	var out []LogEntry
	s.mu.Lock()
	b, m, ring, bridge := s.browser, s.main, s.ring, s.bridge
	s.mu.Unlock()
	if bridge != nil && !bridge.Client().Closed() {
		s.pollBridgeLogs(bridge)
	}
	s.mu.Lock()
	out = append(out, s.bridgeLogs...)
	s.mu.Unlock()
	if ring != nil {
		out = append(out, ring.Entries()...)
	}
	if m != nil {
		out = append(out, m.Logs()...)
	}
	if b != nil {
		if wins, err := b.Windows(); err == nil {
			for _, w := range wins {
				page, err := b.Page(w.Index)
				if err != nil {
					continue
				}
				hub := engine.HubForPage(page)
				if hub == nil || hub.Observer() == nil {
					continue
				}
				for _, e := range hub.Observer().Drain(0) {
					if le, ok := logFromEvent(e, w.Index); ok {
						out = append(out, le)
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func logFromEvent(e engine.ObserverEvent, window int) (LogEntry, bool) {
	t := time.UnixMilli(e.TS)
	switch e.Kind {
	case engine.KindConsole:
		return LogEntry{Time: t, Source: "console", Level: e.Level, Text: e.Text, Location: e.Source, Window: window}, true
	case engine.KindError:
		return LogEntry{Time: t, Source: "exception", Level: "error", Text: e.Text, Location: e.Source, Window: window}, true
	case engine.KindNet:
		if e.Failed == "" && e.Status < 400 {
			return LogEntry{}, false
		}
		text := fmt.Sprintf("%s %s → %d", e.Method, engine.ElideDataURL(e.URL), e.Status)
		if e.Failed != "" {
			text = fmt.Sprintf("%s %s failed: %s", e.Method, engine.ElideDataURL(e.URL), e.Failed)
		}
		return LogEntry{Time: t, Source: "network", Level: "error", Text: text, Window: window}, true
	case engine.KindPage:
		text := e.Event
		if e.URL != "" {
			text += " " + e.URL
		}
		return LogEntry{Time: t, Source: "page", Level: "info", Text: text, Window: window}, true
	}
	return LogEntry{}, false
}

// Manager holds the sessions of one server.
type Manager struct {
	root string
	// MaxSessions caps the open sessions (launched or attached); 0 means
	// DefaultMaxSessions. Every session is a whole Electron process tree.
	MaxSessions int
	// IdleTimeout stops a target launch nobody has used for that long
	// (tools, flows); dev command sessions are exempt. 0 means
	// DefaultIdleTimeout; negative disables.
	IdleTimeout time.Duration

	// OnChange runs after a session was added or removed (the MCP server
	// adapts its tool list to the frameworks open). It must not block.
	OnChange func()

	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
}

// Frameworks returns the frameworks of the open sessions.
func (m *Manager) Frameworks() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, s := range m.sessions {
		if s.Framework != "" {
			out[s.Framework] = true
		}
	}
	return out
}

func (m *Manager) changed() {
	if f := m.OnChange; f != nil {
		f()
	}
}

const (
	DefaultMaxSessions = 3
	DefaultIdleTimeout = 30 * time.Minute
)

func (m *Manager) maxSessions() int {
	if m.MaxSessions > 0 {
		return m.MaxSessions
	}
	return DefaultMaxSessions
}

func (m *Manager) idleTimeout() time.Duration {
	switch {
	case m.IdleTimeout < 0:
		return 0
	case m.IdleTimeout == 0:
		return DefaultIdleTimeout
	}
	return m.IdleTimeout
}

// admit reserves a slot for a new session or explains which to stop.
func (m *Manager) admit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) < m.maxSessions() {
		return nil
	}
	ids := make([]string, 0, len(m.sessions))
	for id, s := range m.sessions {
		ids = append(ids, fmt.Sprintf("%s (%s)", id, s.Name))
	}
	sort.Strings(ids)
	return &UsageError{fmt.Sprintf("%d sessions are already open (%s); each one is a whole Electron process tree, so stop one first (limit %d, --max-sessions)", len(ids), strings.Join(ids, ", "), m.maxSessions())}
}

// NewManager creates a manager whose artifacts live under root (created).
func NewManager(root string) (*Manager, error) {
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil || cache == "" {
			cache = os.TempDir()
		}
		root = filepath.Join(cache, "mcp-app-desktop", "sessions")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	sweepStaleArtifacts(root, time.Now().Add(-StaleArtifactsAge))
	return &Manager{root: root, sessions: map[string]*Session{}}, nil
}

// Get returns a session by id; an empty id selects the only session.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		if len(m.sessions) == 1 {
			for _, s := range m.sessions {
				s.Touch()
				return s, nil
			}
		}
		if len(m.sessions) == 0 {
			return nil, &UsageError{"no application session: call launch or attach first"}
		}
		return nil, &UsageError{"several sessions are open: pass session"}
	}
	s, ok := m.sessions[id]
	if !ok {
		return nil, &UsageError{fmt.Sprintf("unknown session %q", id)}
	}
	s.Touch()
	return s, nil
}

// List returns the open sessions.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) add(s *Session) (*Session, error) {
	added, _, err := m.register(s, false)
	return added, err
}

// addUnique registers s, unless an open session already drives the same
// endpoint: that one is returned with reused true and s is left unregistered.
func (m *Manager) addUnique(s *Session) (*Session, bool, error) {
	return m.register(s, true)
}

func (m *Manager) register(s *Session, unique bool) (*Session, bool, error) {
	m.mu.Lock()
	if unique {
		if prev := m.sessionAtLocked(normalizeEndpoint(s.Endpoint)); prev != nil {
			m.mu.Unlock()
			return prev, true, nil
		}
	}
	m.seq++
	s.ID = "s" + strconv.Itoa(m.seq)
	m.sessions[s.ID] = s
	m.mu.Unlock()
	s.Dir = filepath.Join(m.root, time.Now().Format("20060102-150405")+"-"+s.ID)
	s.Started = time.Now()
	s.since = s.Started
	s.lastUse = s.Started
	s.state = StateRunning
	s.stopWatch = make(chan struct{})
	// A dev command is the developer's working app, kept open between
	// tool calls on purpose: only isolated target launches, the copies an
	// agent may forget, are stopped when idle.
	if s.Owned && s.Mode != "command" {
		if idle := m.idleTimeout(); idle > 0 {
			s.idleAfter = idle
			id := s.ID
			s.onIdle = func() { _, _ = m.Stop(id) }
		}
	}
	if s.dialogs == nil {
		s.dialogs = &engine.DialogAutoPolicy{Accept: true}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, false, err
	}
	go s.watch()
	m.changed()
	return s, false, nil
}

const (
	defaultTargetStartup  = 60 * time.Second
	defaultCommandStartup = 180 * time.Second
	// A Tauri dev command compiles the Rust side first: minutes on a cold
	// cache.
	defaultTauriCommandStartup = 15 * time.Minute
)

// tempRoot is where private profiles live. macOS limits Unix socket paths to
// 104 bytes and apps put sockets in their profile, so /tmp beats $TMPDIR.
func tempRoot() string {
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}
	return os.TempDir()
}

// Launch starts an application and attaches to it. On any failure, whatever
// was started is stopped before returning.
func (m *Manager) Launch(ctx context.Context, spec LaunchSpec) (*Session, error) {
	if len(spec.Command) > 0 {
		return m.launchCommand(ctx, spec)
	}
	if spec.Target == "" {
		return nil, &UsageError{"give target (an Electron or Tauri project, a .app or an executable) or command (a dev command)"}
	}
	t, err := Resolve(spec.Target)
	if err != nil {
		return nil, err
	}
	if t.Framework == FrameworkTauri {
		if len(t.DevCommand) > 0 {
			// A Tauri project runs through its dev loop: tauri dev builds
			// the Rust side and serves the frontend.
			spec.Command, spec.Framework = t.DevCommand, FrameworkTauri
			spec.Dir = firstNonEmpty(spec.Dir, t.Dir)
			spec.Name = firstNonEmpty(spec.Name, t.Name)
			return m.launchCommand(ctx, spec)
		}
		return m.launchTauri(ctx, spec, t)
	}
	if err := m.admit(); err != nil {
		return nil, err
	}
	timeout := spec.StartupTimeout
	if timeout <= 0 {
		timeout = defaultTargetStartup
	}
	cdpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	inspPort := 0
	if spec.Inspect {
		if inspPort, err = freePort(); err != nil {
			return nil, err
		}
	}
	s := &Session{Info: Info{Name: firstNonEmpty(spec.Name, t.Name), Framework: FrameworkElectron, Backend: driver.BackendCDP, Mode: "target", Owned: true, Endpoint: loopbackEndpoint(cdpPort)}, ring: NewRing(3000)}
	s.cdpCandidate = s.Endpoint
	args := []string{"--remote-debugging-port=" + strconv.Itoa(cdpPort)}
	if spec.Inspect {
		args = append(args, "--inspect=127.0.0.1:"+strconv.Itoa(inspPort))
		s.Inspector = loopbackEndpoint(inspPort)
	}
	marker := ""
	if spec.Isolate && !hasUserDataDir(spec.Args) {
		root, err := os.MkdirTemp(tempRoot(), "mcp-app-desktop-")
		if err != nil {
			return nil, fmt.Errorf("create private profile: %w", err)
		}
		profile := filepath.Join(root, "profile")
		if err := os.Mkdir(profile, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("create private profile: %w", err)
		}
		s.root, s.Profile, marker = root, profile, root
		args = append(args, "--user-data-dir="+profile)
	}
	args = append(args, keepPaintingArgs...)
	args = append(args, t.Args...)
	args = append(args, spec.Args...)
	dir := spec.Dir
	if dir == "" {
		dir = t.Dir
	}
	if dir == "" {
		dir = filepath.Dir(t.Executable)
	}
	env := mergeEnv(spec.Env, ports{CDP: cdpPort, Inspect: inspPort})
	p, err := startOwned(t.Executable, args, env, dir, s.ring, marker)
	if err != nil {
		if s.root != "" {
			_ = os.RemoveAll(s.root)
		}
		return nil, err
	}
	s.proc, s.PID = p, p.pid
	if err := s.waitReady(ctx, timeout); err != nil {
		s.stop(false)
		return nil, err
	}
	if t.Packaged {
		s.Note = "packaged app: a production build may ignore --inspect (EnableNodeCliInspectArguments fuse); eval_main then stays unavailable"
	}
	return m.add(s)
}

// tauriNote tells what isolation means for a Tauri app.
const tauriNote = "Tauri app: it keeps its own data directories (isolate applies to Electron only); pass the app's own data-dir variable in env to isolate it"

// launchTauri starts a built Tauri app (its executable or .app). It is
// driven through its bridge plugin, or through WebView2's DevTools Protocol
// on Windows.
func (m *Manager) launchTauri(ctx context.Context, spec LaunchSpec, t *Target) (*Session, error) {
	if err := m.admit(); err != nil {
		return nil, err
	}
	timeout := spec.StartupTimeout
	if timeout <= 0 {
		timeout = defaultTargetStartup
	}
	pp, err := allocPorts(false)
	if err != nil {
		return nil, err
	}
	s := &Session{Info: Info{Name: firstNonEmpty(spec.Name, t.Name), Framework: FrameworkTauri, Mode: "target", Owned: true}, ring: NewRing(3000)}
	s.offer(pp)
	args := append(append([]string{}, t.Args...), spec.Args...)
	dir := spec.Dir
	if dir == "" {
		dir = t.Dir
	}
	if dir == "" {
		dir = filepath.Dir(t.Executable)
	}
	p, err := startOwned(t.Executable, args, mergeEnv(spec.Env, pp), dir, s.ring, "")
	if err != nil {
		return nil, err
	}
	s.proc, s.PID = p, p.pid
	if err := s.waitReady(ctx, timeout); err != nil {
		s.stop(false)
		return nil, err
	}
	if spec.Name == "" && t.Bundle == "" && s.bridge != nil && s.bridge.Client().Hello.AppName != "" {
		s.Name = s.bridge.Client().Hello.AppName // its productName, not the file name
	}
	s.Note = tauriNote
	return m.add(s)
}

// offer records the endpoints a launched app was given; waitReady settles
// on the one that answers.
func (s *Session) offer(pp ports) {
	if pp.CDP > 0 {
		s.cdpCandidate = loopbackEndpoint(pp.CDP)
	}
	if pp.Inspect > 0 {
		s.Inspector = loopbackEndpoint(pp.Inspect)
	}
	if pp.Bridge > 0 {
		s.bridgeCandidate = bridgeAddr(pp.Bridge)
		s.token = pp.Token
	}
	s.dialogs = &engine.DialogAutoPolicy{Accept: true}
}

// bridgeAddr is the loopback address of a Tauri bridge.
func bridgeAddr(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func hasUserDataDir(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--user-data-dir") {
			return true
		}
	}
	return false
}

func (m *Manager) launchCommand(ctx context.Context, spec LaunchSpec) (*Session, error) {
	framework := spec.Framework
	if framework == "" {
		if _, _, ok := tauriRoot(firstNonEmpty(spec.Dir, ".")); ok {
			framework = FrameworkTauri
		}
	}
	timeout := spec.StartupTimeout
	if timeout <= 0 {
		timeout = defaultCommandStartup
		if framework == FrameworkTauri {
			timeout = defaultTauriCommandStartup
		}
	}
	if err := m.admit(); err != nil {
		return nil, err
	}
	pp, err := allocPorts(spec.Inspect && framework != FrameworkTauri)
	if err != nil {
		return nil, err
	}
	argv := make([]string, len(spec.Command))
	for i, a := range spec.Command {
		argv[i] = expandPorts(a, pp)
	}
	extra := map[string]string{}
	for k, v := range spec.Env {
		extra[k] = expandPorts(v, pp)
	}
	exe, err := lookPath(argv[0], spec.Dir)
	if err != nil {
		return nil, &UsageError{fmt.Sprintf("command %q not found: %v", argv[0], err)}
	}
	key := devKey(spec.Dir, spec.Command, spec.Inspect)
	statePath := m.devStatePath(key)
	if prev, err := readDevState(statePath); err == nil {
		if prev.stillOurs() {
			return m.adoptCommand(ctx, prev, statePath, timeout)
		}
		removeDevState(statePath, prev)
	}
	s := &Session{Info: Info{Name: firstNonEmpty(spec.Name, filepath.Base(argv[0])+" "+strings.Join(argv[1:], " ")), Framework: framework, Mode: "command", Owned: true}, ring: NewRing(5000)}
	s.offer(pp)
	// Files, not pipes: the app must survive this server, and a closed pipe
	// would kill it on its next write.
	stdout, stderr, err := openDevLogs(m.devDir(), key)
	if err != nil {
		return nil, fmt.Errorf("dev command logs: %w", err)
	}
	p, err := startOwnedTo(exe, argv[1:], mergeEnv(extra, pp), spec.Dir, stdout, stderr, s.ring, "")
	stdout.Close()
	stderr.Close()
	if err != nil {
		return nil, err
	}
	s.proc, s.PID = p, p.pid
	s.tails = []*fileTail{tailFile(stdout.Name(), 0, s.ring.Writer("stdout")), tailFile(stderr.Name(), 0, s.ring.Writer("stderr"))}
	s.dev = &devState{Name: s.Name, Dir: spec.Dir, Command: spec.Command, Inspect: spec.Inspect, PID: p.pid, PGID: p.pid, RootCmd: rootCommand(p.pid), CDPPort: pp.CDP, InspPort: pp.Inspect, BridgePort: pp.Bridge, Token: pp.Token, Started: time.Now(), StdoutLog: stdout.Name(), StderrLog: stderr.Name()}
	s.devPath = statePath
	// Written before the wait (a first Tauri build takes minutes: a server
	// that dies meanwhile must let the next one adopt the app), and again
	// once the framework is known.
	s.writeDev()
	if err := s.waitReady(ctx, timeout); err != nil {
		s.stop(false)
		return nil, err
	}
	s.dev.Framework, s.dev.Backend = s.Framework, s.Backend
	s.writeDev()
	s.Note = commandNote(s.Framework, spec.Inspect)
	return m.add(s)
}

func (s *Session) writeDev() {
	if s.dev == nil || s.dev.RootCmd == "" {
		return
	}
	if err := writeDevState(s.devPath, s.dev); err != nil {
		s.ring.Add("mcp-app-desktop", "dev state not written, the app will not be adopted by a later server: "+err.Error())
	}
}

// commandNote explains a dev command session once its framework is known.
func commandNote(framework string, inspect bool) string {
	const tail = "; the session re-attaches automatically after every restart; it keeps running when this server exits, and the next launch of the same command adopts it (stop ends it)"
	if framework == FrameworkTauri {
		return "Tauri dev command: driven through tauri-plugin-mcp-app-desktop, which reads $" + tauri.EnvBridgePort + " and $" + tauri.EnvBridgeToken + " from the environment" + tail
	}
	insp := ""
	if inspect {
		insp = " and --inspect=127.0.0.1:$" + EnvInspectPort
	}
	return "dev command: the script must start electron with --remote-debugging-port=$" + EnvCDPPort + insp + tail
}

// adoptCommand takes over a dev command an earlier server started and left
// running, instead of starting a second copy.
func (m *Manager) adoptCommand(ctx context.Context, prev *devState, statePath string, timeout time.Duration) (*Session, error) {
	// Launched a second time by this same server: the session it has is the
	// one to keep using, not a second session on the same app.
	m.mu.Lock()
	open := m.sessionAtLocked(normalizeEndpoint(loopbackEndpoint(prev.CDPPort)))
	if open == nil && prev.BridgePort > 0 {
		open = m.sessionAtLocked(normalizeEndpoint(bridgeAddr(prev.BridgePort)))
	}
	m.mu.Unlock()
	if open != nil {
		open.Touch()
		return open, nil
	}
	s := &Session{Info: Info{Name: prev.Name, Framework: prev.Framework, Mode: "command", Owned: true, PID: prev.PID}, ring: NewRing(5000), dev: prev, devPath: statePath}
	// The endpoints it was given; the one that answers is used (a state
	// written before the app came up does not know which yet).
	s.offer(ports{CDP: prev.CDPPort, Inspect: prev.InspPort, Bridge: prev.BridgePort, Token: prev.Token})
	switch prev.Backend {
	case driver.BackendBridge:
		s.cdpCandidate = ""
	case driver.BackendCDP:
		s.bridgeCandidate = ""
	}
	s.Note = fmt.Sprintf("adopted the dev command started %s by an earlier server (pid %d): same app, same ports; it keeps running when this server exits (stop ends it)", prev.Started.Format("15:04:05"), prev.PID)
	s.proc = adoptOwned(prev.PID, prev.RootCmd, s.ring)
	// The last lines give the context; what follows streams as before.
	s.tails = []*fileTail{tailFile(prev.StdoutLog, -16*1024, s.ring.Writer("stdout")), tailFile(prev.StderrLog, -16*1024, s.ring.Writer("stderr"))}
	if err := s.waitReady(ctx, timeout); err != nil {
		// The developer's app is not ours to kill because it is slow: leave it.
		s.release()
		return nil, fmt.Errorf("found the dev command left running (pid %d) but could not reach it: %w; stop it yourself or remove %s", prev.PID, err, statePath)
	}
	return m.add(s)
}

func lookPath(name, dir string) (string, error) {
	if strings.Contains(name, string(filepath.Separator)) {
		if !filepath.IsAbs(name) && dir != "" {
			name = filepath.Join(dir, name)
		}
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return name, nil
	}
	return execLookPath(name)
}

// waitReady waits until the launched app answers on one of the endpoints it
// was offered, which also tells its framework when the launch did not
// (a dev command): the DevTools Protocol for Electron (and Tauri on
// Windows, WebView2), the bridge plugin for Tauri. Then it connects.
func (s *Session) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var cdpGrace time.Time
	for {
		if s.proc.exited() {
			return fmt.Errorf("%s exited before it could be reached (%s)\noutput:\n%s", s.Name, s.proc.exitError(), s.ring.Tail(30))
		}
		if s.cdpCandidate != "" {
			if v, ok := probeCDP(ctx, s.cdpCandidate); ok {
				if s.Framework == "" {
					s.Framework = frameworkOf(v)
				}
				s.Backend, s.Endpoint = driver.BackendCDP, s.cdpCandidate
				break
			}
		}
		if s.bridgeCandidate != "" && tauri.Probe(ctx, s.bridgeCandidate) {
			s.Framework = FrameworkTauri
			// On Windows the same app also opens WebView2's DevTools port,
			// which drives it best: give it a moment to come up.
			if runtime.GOOS == "windows" && s.cdpCandidate != "" {
				if cdpGrace.IsZero() {
					cdpGrace = time.Now().Add(10 * time.Second)
				}
				if time.Now().Before(cdpGrace) {
					time.Sleep(150 * time.Millisecond)
					continue
				}
			}
			s.Backend, s.Endpoint = driver.BackendBridge, s.bridgeCandidate
			break
		}
		if time.Now().After(deadline) {
			return s.notReachedError(timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if s.Backend == driver.BackendBridge {
		return s.connectBridge(ctx, timeout)
	}
	if s.Framework == FrameworkTauri {
		s.Inspector = ""
	}
	b, err := engine.Connect(ctx, s.Endpoint, 30*time.Second)
	if err != nil {
		return fmt.Errorf("attach to %s: %w", s.Name, err)
	}
	s.browser = b
	s.generation = 1
	// The first window can take a moment after the port opens.
	winDeadline := time.Now().Add(min(timeout, 30*time.Second))
	for {
		if wins, err := b.Windows(); err == nil && len(wins) > 0 {
			break
		}
		if time.Now().After(winDeadline) || s.proc.exited() {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	s.waitFirstPaint(b, 4*time.Second)
	if s.Inspector != "" {
		ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
		for {
			if m, err := ConnectMain(ictx, s.Inspector); err == nil {
				s.main = m
				break
			}
			if ictx.Err() != nil {
				s.Note = strings.TrimSpace(s.Note + " The main process inspector did not answer at " + s.Inspector + "; eval_main is unavailable until it does.")
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		cancel()
	}
	return nil
}

// notReachedError explains a launch that never answered, per framework.
func (s *Session) notReachedError(timeout time.Duration) error {
	tail := s.ring.Tail(30)
	switch {
	case s.Framework == FrameworkTauri && runtime.GOOS != "windows":
		return fmt.Errorf("%s did not open its bridge %s within %s: the app must include tauri-plugin-mcp-app-desktop (registered in debug builds) and inherit $%s / $%s from its environment; a first build of a Tauri dev command can take minutes (startup_timeout_ms)\noutput:\n%s", s.Name, s.bridgeCandidate, timeout, tauri.EnvBridgePort, tauri.EnvBridgeToken, tail)
	case s.Framework == FrameworkTauri:
		return fmt.Errorf("%s opened neither WebView2's DevTools port %s nor its bridge %s within %s (WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS is set for it)\noutput:\n%s", s.Name, s.cdpCandidate, s.bridgeCandidate, timeout, tail)
	case s.Framework == "" && s.bridgeCandidate != "":
		return fmt.Errorf("%s answered neither on its debugging port %s (Electron: pass --remote-debugging-port=$%s to electron) nor on its bridge %s (Tauri: add tauri-plugin-mcp-app-desktop) within %s\noutput:\n%s", s.Name, s.cdpCandidate, EnvCDPPort, s.bridgeCandidate, timeout, tail)
	}
	return fmt.Errorf("%s did not open its debugging port %s within %s\noutput:\n%s", s.Name, s.cdpCandidate, timeout, tail)
}

// connectBridge says hello to the bridge that answered and waits for the
// first window to show something.
func (s *Session) connectBridge(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(min(timeout, 15*time.Second))
	var c *tauri.Client
	for {
		var err error
		if c, err = tauri.Dial(ctx, s.Endpoint, s.token); err == nil {
			break
		}
		if time.Now().After(deadline) || s.proc.exited() {
			return fmt.Errorf("connect to the bridge of %s at %s: %w", s.Name, s.Endpoint, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	s.bridge = tauri.NewApp(c, s.dialogPolicy)
	s.generation = 1
	if s.Mode == "command" && c.Hello.AppName != "" {
		s.Name = c.Hello.AppName
	}
	s.bridge.WaitFirstPaint(ctx, min(timeout, 30*time.Second))
	return nil
}

// waitFirstPaint gives the renderer a bounded moment to mount its UI so the
// first snapshot is not an empty document: it returns as soon as the first
// window has a body with text or an interactive element, or after d.
func (s *Session) waitFirstPaint(b *engine.Browser, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		page, err := b.Page(0)
		if err != nil {
			return
		}
		res, err := page.Timeout(time.Second).Eval(`() => document.readyState === "complete" && !!document.body && (document.body.innerText.trim().length > 0 || !!document.querySelector("button,a,input,textarea,select,[role],[contenteditable]"))`)
		if err == nil && res.Value.Bool() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Attach connects to a running application without owning it, or returns
// the session already driving that endpoint.
func (m *Manager) Attach(ctx context.Context, spec AttachSpec) (*Session, error) {
	s, _, err := m.AttachOrReuse(ctx, spec)
	return s, err
}

// normalizeEndpoint turns "9333", "localhost:9333/" or
// "http://LOCALHOST:9333" into http://127.0.0.1:9333, so one application
// reached under several spellings is recognised as the same.
func normalizeEndpoint(raw string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.Contains(endpoint, "://") {
		if p, err := strconv.Atoi(endpoint); err == nil {
			return loopbackEndpoint(p)
		}
		endpoint = "http://" + endpoint
	}
	scheme, rest, _ := strings.Cut(endpoint, "://")
	host, path, _ := strings.Cut(rest, "/")
	if h, port, err := net.SplitHostPort(host); err == nil && strings.EqualFold(h, "localhost") {
		host = net.JoinHostPort("127.0.0.1", port)
	}
	endpoint = strings.ToLower(scheme) + "://" + strings.ToLower(host)
	if path != "" {
		endpoint += "/" + path
	}
	return endpoint
}

// sessionAtLocked returns the open session driving endpoint (normalised),
// whatever its mode: attaching to the port of an app launched here reuses it
// too.
func (m *Manager) sessionAtLocked(endpoint string) *Session {
	for _, s := range m.sessions {
		if normalizeEndpoint(s.Endpoint) == endpoint {
			return s
		}
	}
	return nil
}

// AttachOrReuse is Attach that also says whether an open session was
// reused. A second attach to the same endpoint used to open a second session
// on the same app, after which every tool without session failed with
// « several sessions are open » (2026-10-24). The reused session follows the
// app's restarts on its own (see tickLocked), so attaching again is never
// needed.
func (m *Manager) AttachOrReuse(ctx context.Context, spec AttachSpec) (*Session, bool, error) {
	if strings.TrimSpace(spec.Endpoint) == "" {
		return nil, false, &UsageError{"give endpoint: http://127.0.0.1:PORT of an Electron app started with --remote-debugging-port, or the bridge port of a Tauri app started with " + tauri.EnvBridgePort}
	}
	endpoint := normalizeEndpoint(spec.Endpoint)
	m.mu.Lock()
	prev := m.sessionAtLocked(endpoint)
	m.mu.Unlock()
	if prev != nil {
		prev.Touch()
		prev.addInspector(ctx, spec.Inspector)
		return prev, true, nil
	}
	if err := m.admit(); err != nil {
		return nil, false, err
	}
	s, err := m.attachNew(ctx, endpoint, spec)
	if err != nil {
		return nil, false, err
	}
	// Two attach calls may race to the same endpoint: the loser keeps the
	// winner's session and lets its own connection go.
	added, reused, err := m.addUnique(s)
	if reused {
		s.closeUnregistered()
		added.Touch()
		added.addInspector(ctx, spec.Inspector)
	}
	return added, reused, err
}

// addInspector connects the main process inspector of a reused session that
// had none.
func (s *Session) addInspector(ctx context.Context, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	insp := normalizeEndpoint(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.main != nil || (s.Inspector != "" && s.Inspector != insp) {
		return
	}
	s.Inspector = insp
	if mc, err := ConnectMain(ctx, insp); err == nil {
		s.main = mc
	} else {
		s.Note = err.Error()
	}
}

// closeUnregistered drops the connections of a session that was never added.
func (s *Session) closeUnregistered() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.detachLocked()
}

func (m *Manager) attachNew(ctx context.Context, endpoint string, spec AttachSpec) (*Session, error) {
	v, ok := probeCDP(ctx, endpoint)
	if !ok {
		if v.Browser != "" {
			return nil, &UsageError{fmt.Sprintf("%s is a Node inspector (%s), not a remote debugging endpoint: pass it as inspector_endpoint", endpoint, v.Browser)}
		}
		return m.attachBridge(ctx, endpoint, spec)
	}
	b, err := engine.Connect(ctx, endpoint, 30*time.Second)
	if err != nil {
		return nil, err
	}
	// The ring holds this server's own notes about the app (a window that
	// could not be kept active, …): an attached app's output is not ours.
	s := &Session{Info: Info{Name: firstNonEmpty(spec.Name, v.Browser), Framework: frameworkOf(v), Backend: driver.BackendCDP, Mode: "attach", Endpoint: endpoint}, browser: b, generation: 1, ring: NewRing(200)}
	if spec.Inspector != "" {
		insp := normalizeEndpoint(spec.Inspector)
		s.Inspector = insp
		if mc, err := ConnectMain(ctx, insp); err == nil {
			s.main = mc
		} else {
			s.Note = err.Error()
		}
	}
	return s, nil
}

// attachBridge attaches to a Tauri app whose bridge plugin listens at
// endpoint (started by hand with MCP_APP_DESKTOP_BRIDGE_PORT).
func (m *Manager) attachBridge(ctx context.Context, endpoint string, spec AttachSpec) (*Session, error) {
	_, rest, _ := strings.Cut(endpoint, "://")
	addr, _, _ := strings.Cut(rest, "/")
	if !tauri.Probe(ctx, addr) {
		return nil, fmt.Errorf("nothing answers at %s: neither a remote debugging endpoint (Electron started with --remote-debugging-port) nor a Tauri bridge (an app with tauri-plugin-mcp-app-desktop started with %s)", endpoint, tauri.EnvBridgePort)
	}
	c, err := tauri.Dial(ctx, addr, spec.Token)
	if err != nil {
		hint := ""
		if spec.Token == "" {
			hint = " (if the app was started with " + tauri.EnvBridgeToken + ", pass its value as token)"
		}
		return nil, fmt.Errorf("a Tauri bridge listens at %s but the connection failed: %w%s", addr, err, hint)
	}
	s := &Session{Info: Info{Name: firstNonEmpty(spec.Name, c.Hello.AppName), Framework: FrameworkTauri, Backend: driver.BackendBridge, Mode: "attach", Endpoint: addr, PID: c.Hello.PID}, generation: 1, ring: NewRing(200), token: spec.Token}
	s.dialogs = &engine.DialogAutoPolicy{Accept: true}
	s.bridge = tauri.NewApp(c, s.dialogPolicy)
	// The plugin listens before the app opens its first window: give it a
	// bounded moment, as a launch does.
	s.bridge.WaitFirstPaint(ctx, 10*time.Second)
	return s, nil
}

// StaleArtifactsAge is the age past which a session artifacts directory left
// by a server that died without stop is removed when a new server starts.
// Several servers may share the root (one per client), so a younger
// directory may still belong to a live session and is kept.
const StaleArtifactsAge = 24 * time.Hour

// sweepStaleArtifacts removes the session directories under root last
// modified before cutoff. Errors are ignored: the sweep is housekeeping and
// must never stop the server from starting.
func sweepStaleArtifacts(root string, cutoff time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// StopReport states what was released.
type StopReport struct {
	SessionID  string `json:"session_id"`
	Terminated bool   `json:"terminated"`
	// ArtifactsDir is the session's screenshots and flow outputs directory;
	// ArtifactsKept says it was left on disk at the caller's request.
	ArtifactsDir  string `json:"artifacts_dir,omitempty"`
	ArtifactsKept bool   `json:"artifacts_kept,omitempty"`
	// Survivors lists pids that could not be terminated.
	Survivors []int  `json:"survivors,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Stop closes a session; an owned application is terminated together with
// every process it spawned, and its private profile removed. The session's
// artifacts (screenshots, flow outputs) are removed too, launched or attached.
func (m *Manager) Stop(id string) (*StopReport, error) {
	return m.stopSession(id, false)
}

// StopKeepArtifacts is Stop leaving the artifacts directory on disk, for a
// caller that still needs its screenshots as evidence.
func (m *Manager) StopKeepArtifacts(id string) (*StopReport, error) {
	return m.stopSession(id, true)
}

func (m *Manager) stopSession(id string, keepArtifacts bool) (*StopReport, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()
	defer m.changed()
	return s.stop(keepArtifacts), nil
}

// Close ends every session (server shutdown). A dev command still running
// is released, not stopped: it is the developer's app and the next server
// adopts it.
func (m *Manager) Close() {
	for _, s := range m.List() {
		if s.devPath != "" && s.proc != nil && !s.proc.exited() {
			m.mu.Lock()
			delete(m.sessions, s.ID)
			m.mu.Unlock()
			s.release()
			m.changed()
			continue
		}
		_, _ = m.Stop(s.ID)
	}
}

// release lets go of a session without signalling its application.
func (s *Session) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	s.setStateLocked(StateStopped)
	if s.stopWatch != nil {
		close(s.stopWatch)
	}
	s.detachLocked()
	for _, t := range s.tails {
		t.close()
	}
	if s.proc != nil {
		s.proc.release()
	}
	s.releaseArtifactsLocked(&StopReport{}, false)
}

func (s *Session) stop(keepArtifacts bool) *StopReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := &StopReport{SessionID: s.ID}
	if s.stopped {
		rep.Note = "already stopped"
		return rep
	}
	s.stopped = true
	s.setStateLocked(StateStopped)
	if s.stopWatch != nil {
		close(s.stopWatch)
	}
	s.detachLocked()
	defer s.releaseArtifactsLocked(rep, keepArtifacts)
	if !s.Owned {
		rep.Note = "attached session: detached, the application keeps running"
		return rep
	}
	if s.proc != nil {
		rep.Survivors = s.proc.terminate()
	}
	for _, t := range s.tails {
		t.close()
	}
	rep.Terminated = len(rep.Survivors) == 0
	if s.devPath != "" && rep.Terminated {
		removeDevState(s.devPath, s.dev)
	}
	if s.root != "" {
		var rmErr error
		for i := 0; i < 5; i++ {
			if rmErr = os.RemoveAll(s.root); rmErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if rmErr != nil {
			rep.Note = "private profile not removed: " + rmErr.Error()
		}
	}
	return rep
}

// releaseArtifactsLocked removes the session's artifacts directory unless
// keep is set; a failure is reported in the note, never hidden.
func (s *Session) releaseArtifactsLocked(rep *StopReport, keep bool) {
	if s.Dir == "" {
		return
	}
	rep.ArtifactsDir = s.Dir
	if keep {
		rep.ArtifactsKept = true
		return
	}
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(s.Dir); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	msg := "artifacts not removed: " + err.Error()
	if rep.Note != "" {
		msg = rep.Note + "; " + msg
	}
	rep.Note = msg
}

// mergeEnv builds the child environment: this server's environment (the
// developer's shell, which toolchains depend on), minus ELECTRON_RUN_AS_NODE
// and stale port variables, plus the caller's variables and the chosen
// ports (under their current and legacy names).
func mergeEnv(extra map[string]string, pp ports) []string {
	drop := map[string]bool{"ELECTRON_RUN_AS_NODE": true}
	for _, k := range portVars {
		drop[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := extra[k]; !drop[k] && !override {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	for _, kv := range pp.env() {
		if _, override := extra[strings.SplitN(kv, "=", 2)[0]]; !override {
			out = append(out, kv)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
