package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func sessionProp() mcpgo.PropertyOption {
	return mcpgo.Description("Session id; omit when only one application is open.")
}

func (s *Server) sessionTools() []toolDef {
	return []toolDef{
		{mcpgo.NewTool("launch",
			mcpgo.WithDescription("Start an Electron or Tauri application and attach to it; the framework is recognised from the target. Electron target (a project dir with node_modules/electron run as `electron .`, a built .app, an executable): it gets --remote-debugging-port, --inspect (main process) and a private --user-data-dir. Tauri target: a project dir (src-tauri/tauri.conf.json) runs through its dev loop (tauri dev: the project's \"tauri\" script, its CLI, or cargo tauri); a built .app or executable runs directly. A Tauri app must register tauri-plugin-mcp-app-desktop (debug builds) on macOS and Linux; on Windows WebView2's DevTools port is used. command: a development command instead (e.g. [\"bun\",\"run\",\"dev\"]): it receives $MCP_APP_DESKTOP_CDP_PORT and $MCP_APP_DESKTOP_INSPECT_PORT (an Electron script passes them to electron as --remote-debugging-port / --inspect) and $MCP_APP_DESKTOP_BRIDGE_PORT / $MCP_APP_DESKTOP_BRIDGE_TOKEN (read by the Tauri plugin); whichever answers first tells the framework, and the session re-attaches after every restart the dev loop does. Returns the session and the first window's snapshot."),
			mcpgo.WithString("target", mcpgo.Description("Electron or Tauri project dir, .app bundle or executable path.")),
			mcpgo.WithArray("command", mcpgo.Description("Development command argv (no shell). ${MCP_APP_DESKTOP_CDP_PORT}, ${MCP_APP_DESKTOP_INSPECT_PORT} and ${MCP_APP_DESKTOP_BRIDGE_PORT} are expanded in argv and env (the former MCP_ELECTRON_* names too)."), mcpgo.WithStringItems()),
			mcpgo.WithString("framework", mcpgo.Enum("auto", "electron", "tauri"), mcpgo.Description("For command: the framework it starts (auto: whichever endpoint answers; a cwd with src-tauri means Tauri and a longer startup timeout for its first build)."), mcpgo.DefaultString("auto")),
			mcpgo.WithArray("args", mcpgo.Description("Extra application arguments (target launches)."), mcpgo.WithStringItems()),
			mcpgo.WithObject("env", mcpgo.Description("Extra environment variables, e.g. an app-specific test data dir."), mcpgo.AdditionalProperties(map[string]any{"type": "string"})),
			mcpgo.WithString("cwd", mcpgo.Description("Working directory (default: the project or target dir).")),
			mcpgo.WithBoolean("isolate", mcpgo.Description("Electron: private user-data-dir for a target launch (default true). A Tauri app keeps its own data directories. The app's own data dir env vars still need passing through env."), mcpgo.DefaultBool(true)),
			mcpgo.WithBoolean("inspect", mcpgo.Description("Electron: open the main process Node inspector for eval_main (default true)."), mcpgo.DefaultBool(true)),
			mcpgo.WithNumber("startup_timeout_ms", mcpgo.Description("Wait for the app to answer (default 60000 for a target, 180000 for a command, 900000 for a Tauri dev command, whose first build compiles the Rust side).")),
			mcpgo.WithString("name", mcpgo.Description("Display name for the session.")),
			mcpgo.WithTitleAnnotation("Launch application"), mcpgo.WithDestructiveHintAnnotation(true), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleLaunch, ""},
		{mcpgo.NewTool("attach",
			mcpgo.WithDescription("Attach to an app already running: an Electron app started with --remote-debugging-port (endpoint http://127.0.0.1:PORT or just the port; optional inspector_endpoint: its main process --inspect endpoint, for eval_main), or a Tauri app with tauri-plugin-mcp-app-desktop started with MCP_APP_DESKTOP_BRIDGE_PORT (endpoint: that port; token: its MCP_APP_DESKTOP_BRIDGE_TOKEN when it has one). The kind is recognised from what answers. stop detaches without closing the app."),
			mcpgo.WithString("endpoint", mcpgo.Required(), mcpgo.Description("Remote debugging endpoint (http://127.0.0.1:9222 or 9222) or Tauri bridge port (127.0.0.1:9333 or 9333).")),
			mcpgo.WithString("inspector_endpoint", mcpgo.Description("Electron: main process Node inspector endpoint, e.g. http://127.0.0.1:9229.")),
			mcpgo.WithString("token", mcpgo.Description("Tauri: the bridge token the app was started with (MCP_APP_DESKTOP_BRIDGE_TOKEN), if any.")),
			mcpgo.WithString("name", mcpgo.Description("Display name for the session.")),
			mcpgo.WithTitleAnnotation("Attach to a running application"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleAttach, ""},
		{mcpgo.NewTool("status",
			mcpgo.WithDescription("Sessions, their framework (electron, tauri) and backend (cdp, bridge), state (running, restarting, exited), restarts seen, endpoints, resources, and the windows of the running ones. Call it when a tool answers that the app is restarting or unreachable."),
			mcpgo.WithTitleAnnotation("Session status"), mcpgo.WithReadOnlyHintAnnotation(true), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleStatus, ""},
		{mcpgo.NewTool("windows",
			mcpgo.WithDescription("List the application's windows (Electron: BrowserWindow, WebContentsView, <webview>; Tauri: webview windows, with their label) with their index, title and URL. Pass window: <index> to the other tools to target one; 0 is the first window seen."),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithTitleAnnotation("List windows"), mcpgo.WithReadOnlyHintAnnotation(true), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleWindows, ""},
		{mcpgo.NewTool("logs",
			mcpgo.WithDescription("Console messages and uncaught exceptions of every window, the process stdout/stderr, and per framework: Electron's failed network requests and main process console (via its inspector); Tauri's IPC calls (every command the page invoked, its duration and outcome: source ipc) and the JavaScript dialogs answered. Oldest first. Default: the last 100 entries; errors: true keeps only errors, warnings, exceptions and stderr."),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("last", mcpgo.Description("Number of entries to return (default 100)."), mcpgo.DefaultNumber(100)),
			mcpgo.WithBoolean("errors", mcpgo.Description("Only errors, warnings, exceptions and stderr."), mcpgo.DefaultBool(false)),
			mcpgo.WithString("contains", mcpgo.Description("Keep entries containing this text (case-insensitive).")),
			mcpgo.WithString("source", mcpgo.Description("Keep one source: console, exception, network, page, main, ipc, stdout, stderr.")),
			mcpgo.WithNumber("since_seconds", mcpgo.Description("Only entries newer than this many seconds.")),
			mcpgo.WithNumber("window", mcpgo.Description("Only this window's renderer entries (process output is always included).")),
			mcpgo.WithTitleAnnotation("Read logs"), mcpgo.WithReadOnlyHintAnnotation(true), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleLogs, ""},
		{mcpgo.NewTool("stop",
			mcpgo.WithDescription("Close a session. An application started by launch is terminated with every process it spawned and its private profile removed; an attached one keeps running. The session's saved screenshots and flow outputs are deleted too, unless keep_artifacts is true."),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithBoolean("keep_artifacts", mcpgo.Description("Leave the session's screenshots and flow outputs on disk (evidence still needed). Default false: they are deleted with the session."), mcpgo.DefaultBool(false)),
			mcpgo.WithTitleAnnotation("Stop application"), mcpgo.WithDestructiveHintAnnotation(true), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleStop, ""},
	}
}

func stringSlice(req mcpgo.CallToolRequest, key string) ([]string, error) {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return strings.Fields(v), nil
	}
	return nil, fmt.Errorf("%s must be an array of strings", key)
}

func stringMap(req mcpgo.CallToolRequest, key string) (map[string]string, error) {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object of strings", key)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s must be a string", key, k)
		}
		out[k] = s
	}
	return out, nil
}

func (s *Server) handleLaunch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	var spec desktop.LaunchSpec
	var err error
	spec.Target = mcpgo.ParseString(req, "target", "")
	if spec.Command, err = stringSlice(req, "command"); err != nil {
		return errResult(err)
	}
	if spec.Args, err = stringSlice(req, "args"); err != nil {
		return errResult(err)
	}
	if spec.Env, err = stringMap(req, "env"); err != nil {
		return errResult(err)
	}
	spec.Dir = mcpgo.ParseString(req, "cwd", "")
	spec.Isolate = mcpgo.ParseBoolean(req, "isolate", true)
	spec.Inspect = mcpgo.ParseBoolean(req, "inspect", true)
	spec.StartupTimeout = time.Duration(mcpgo.ParseInt(req, "startup_timeout_ms", 0)) * time.Millisecond
	spec.Name = mcpgo.ParseString(req, "name", "")
	switch fw := strings.ToLower(mcpgo.ParseString(req, "framework", "auto")); fw {
	case "", "auto":
	case desktop.FrameworkElectron, desktop.FrameworkTauri:
		spec.Framework = fw
	default:
		return errorf("framework must be auto, electron or tauri")
	}
	sess, err := s.mgr.Launch(ctx, spec)
	if err != nil {
		return errResult(err)
	}
	return s.sessionResult(ctx, sess, "Launched")
}

func (s *Server) handleAttach(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sess, reused, err := s.mgr.AttachOrReuse(ctx, desktop.AttachSpec{
		Endpoint:  mcpgo.ParseString(req, "endpoint", ""),
		Inspector: mcpgo.ParseString(req, "inspector_endpoint", ""),
		Token:     mcpgo.ParseString(req, "token", ""),
		Name:      mcpgo.ParseString(req, "name", ""),
	})
	if err != nil {
		return errResult(err)
	}
	if reused {
		return s.sessionResult(ctx, sess, "Already attached to")
	}
	return s.sessionResult(ctx, sess, "Attached")
}

// sessionResult describes a new session and shows its first window.
func (s *Server) sessionResult(ctx context.Context, sess *desktop.Session, verb string) (*mcpgo.CallToolResult, error) {
	st := sess.Status()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s as session %s (%s app, pid %d, %s %s", verb, st.Name, st.ID, st.Framework, st.PID, st.Backend, st.Endpoint)
	if st.Inspector != "" {
		if st.MainProc {
			fmt.Fprintf(&sb, ", main inspector %s", st.Inspector)
		} else {
			fmt.Fprintf(&sb, ", inspector %s not connected", st.Inspector)
		}
	}
	sb.WriteString(")")
	if st.Profile != "" {
		fmt.Fprintf(&sb, "\nprivate profile: %s", st.Profile)
	}
	if st.Note != "" {
		fmt.Fprintf(&sb, "\nnote: %s", st.Note)
	}
	app, err := sess.App(ctx)
	if err != nil {
		fmt.Fprintf(&sb, "\n%v", err)
		return text(sb.String())
	}
	wins, err := app.Windows(ctx)
	if err != nil || len(wins) == 0 {
		sb.WriteString("\nno window yet: call snapshot once the app has opened one")
		return text(sb.String())
	}
	sb.WriteString("\n" + windowsText(wins))
	var snap string
	w, err := app.Window(ctx, 0)
	if err == nil {
		snap, err = s.snapshotText(ctx, sess, w, "content", "", true)
	}
	if err != nil {
		fmt.Fprintf(&sb, "\nsnapshot: %v", err)
	} else {
		sb.WriteString("\n" + snap)
	}
	return text(sb.String())
}

func (s *Server) handleStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sessions := s.mgr.List()
	if len(sessions) == 0 {
		return text("no session: call launch or attach")
	}
	var sb strings.Builder
	for _, sess := range sessions {
		st := sess.Status()
		var wins []driver.WindowInfo
		var winErr error
		if st.State == desktop.StateRunning {
			var app driver.App
			if app, winErr = sess.App(ctx); winErr == nil {
				wins, winErr = app.Windows(ctx)
			}
		}
		sb.WriteString(statusText(st, wins, winErr))
		if tool, since := sess.Busy(); tool != "" {
			fmt.Fprintf(&sb, "\n  busy: %s running for %s (actions of this session wait for it)", tool, time.Since(since).Round(time.Second))
		}
		sb.WriteString("\n")
	}
	return text(strings.TrimRight(sb.String(), "\n"))
}

// statusText renders one session for status. It reads only st and the
// windows listed by the caller (none while the app is not running), so a
// session caught mid-restart renders like any other.
func statusText(st desktop.Status, wins []driver.WindowInfo, winErr error) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %q %s %s state=%s since %s", st.ID, st.Name, st.Framework, st.Mode, st.State, st.StateSince.Format("15:04:05"))
	if st.PID > 0 {
		fmt.Fprintf(&sb, " pid=%d", st.PID)
	}
	fmt.Fprintf(&sb, " %s=%s", st.Backend, st.Endpoint)
	if st.Inspector != "" {
		fmt.Fprintf(&sb, " inspector=%s(%s)", st.Inspector, map[bool]string{true: "connected", false: "not connected"}[st.MainProc])
	}
	if n := st.Generation - 1; n > 0 {
		fmt.Fprintf(&sb, " restarts=%d", n)
		if len(st.Restarts) > 0 {
			fmt.Fprintf(&sb, " (last %s)", st.Restarts[len(st.Restarts)-1].Format("15:04:05"))
		}
	}
	if st.Usage != nil {
		fmt.Fprintf(&sb, "\n  resources: %d processes, %s resident, %.1f%% cpu", st.Usage.Processes, humanBytes(st.Usage.RSSBytes), st.Usage.CPUPercent)
		if st.Usage.RSSBytes > memoryWarnBytes {
			fmt.Fprintf(&sb, " — above %s: stop sessions you no longer need", humanBytes(memoryWarnBytes))
		}
	}
	if st.IdleStopIn > 0 {
		fmt.Fprintf(&sb, "\n  idle: %s without tool access, stopped automatically in %s", st.IdleFor.Round(time.Second), st.IdleStopIn.Round(time.Second))
	}
	if st.LastError != "" {
		fmt.Fprintf(&sb, "\n  last error: %s", st.LastError)
	}
	if st.Note != "" {
		fmt.Fprintf(&sb, "\n  note: %s", st.Note)
	}
	switch {
	case st.State != desktop.StateRunning:
	case winErr != nil:
		fmt.Fprintf(&sb, "\n  windows: %v", winErr)
	case len(wins) == 0:
		sb.WriteString("\n  no window open")
	default:
		sb.WriteString("\n  " + strings.ReplaceAll(windowsText(wins), "\n", "\n  "))
	}
	return sb.String()
}

func (s *Server) handleWindows(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	app, err := sess.App(ctx)
	if err != nil {
		return errResult(err)
	}
	wins, err := app.Windows(ctx)
	if err != nil {
		return errResult(err)
	}
	if len(wins) == 0 {
		return text("no window open")
	}
	return text(windowsText(wins))
}

func (s *Server) handleLogs(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	last := min(max(mcpgo.ParseInt(req, "last", 100), 1), maxLogEntries)
	if last <= 0 {
		last = 100
	}
	onlyErr := mcpgo.ParseBoolean(req, "errors", false)
	contains := strings.ToLower(mcpgo.ParseString(req, "contains", ""))
	source := strings.ToLower(mcpgo.ParseString(req, "source", ""))
	since := mcpgo.ParseFloat64(req, "since_seconds", 0)
	window := mcpgo.ParseInt(req, "window", -1)
	var cutoff time.Time
	if since > 0 {
		cutoff = time.Now().Add(-time.Duration(since * float64(time.Second)))
	}
	var keep []desktop.LogEntry
	for _, e := range sess.Logs() {
		if onlyErr && !e.IsError() {
			continue
		}
		if contains != "" && !strings.Contains(strings.ToLower(e.Text), contains) {
			continue
		}
		if source != "" && !strings.HasPrefix(e.Source, source) {
			continue
		}
		if !cutoff.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		if window >= 0 && e.Window >= 0 && e.Window != window {
			continue
		}
		keep = append(keep, e)
	}
	total := len(keep)
	if len(keep) > last {
		keep = keep[len(keep)-last:]
	}
	if len(keep) == 0 {
		return text("no log entries match")
	}
	var sb strings.Builder
	if total > len(keep) {
		fmt.Fprintf(&sb, "(%d of %d entries, newest last)\n", len(keep), total)
	}
	for _, e := range keep {
		sb.WriteString(formatLog(e))
		sb.WriteString("\n")
	}
	return text(strings.TrimRight(sb.String(), "\n"))
}

func formatLog(e desktop.LogEntry) string {
	var sb strings.Builder
	sb.WriteString(e.Time.Format("15:04:05.000"))
	sb.WriteString(" ")
	if e.Window >= 0 {
		fmt.Fprintf(&sb, "w%d ", e.Window)
	}
	sb.WriteString(e.Source)
	if e.Level != "" && e.Level != "info" {
		sb.WriteString("/" + e.Level)
	}
	sb.WriteString(" " + e.Text)
	if e.Location != "" {
		sb.WriteString("  (" + e.Location + ")")
	}
	return sb.String()
}

func (s *Server) handleStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id := mcpgo.ParseString(req, "session", "")
	sess, err := s.mgr.Get(id)
	if err != nil {
		return errResult(err)
	}
	stop := s.mgr.Stop
	if mcpgo.ParseBoolean(req, "keep_artifacts", false) {
		stop = s.mgr.StopKeepArtifacts
	}
	rep, err := stop(sess.ID)
	if err != nil {
		return errResult(err)
	}
	if len(rep.Survivors) > 0 {
		return errorf("session %s closed but %d process(es) survived SIGKILL: %v. %s", rep.SessionID, len(rep.Survivors), rep.Survivors, rep.Note)
	}
	msg := fmt.Sprintf("session %s closed", rep.SessionID)
	if rep.ArtifactsKept {
		msg += "; artifacts kept in " + rep.ArtifactsDir + " (delete them once the work is done)"
	} else if rep.ArtifactsDir != "" && !strings.Contains(rep.Note, "artifacts not removed") {
		msg += "; artifacts deleted"
	}
	if rep.Note != "" {
		msg += ": " + rep.Note
	}
	return text(msg)
}

// memoryWarnBytes is the resident size of one application's process tree
// above which status asks the agent to clean up.
const memoryWarnBytes = 2 << 30

// maxLogEntries caps one logs answer.
const maxLogEntries = 2000

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n/1024)
}
