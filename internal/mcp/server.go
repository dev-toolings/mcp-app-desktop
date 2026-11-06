// Package mcp exposes mcp-app-desktop as a Model Context Protocol server over
// stdio: tools to launch, observe, drive and debug an Electron or a Tauri
// application from Claude Code, Codex or any other MCP host.
//
// One long-lived session per application keeps the connection (CDP, or the
// Tauri bridge), the per-window ref snapshots and the logs across tool
// calls, so a ref read by snapshot stays valid for the next click.
//
// The tool list is dynamic: tools that only make sense for one framework
// (eval_main for Electron, invoke and emit for Tauri) are listed while a
// session of that framework is open, and the host is told when that changes
// (notifications/tools/list_changed).
package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"
)

// Options configure the server.
type Options struct {
	// ArtifactsRoot receives per-session screenshots and flow outputs
	// (default: the user cache directory, mcp-app-desktop/sessions).
	ArtifactsRoot string
	Version       string
	// MaxSessions caps open sessions (0: desktop.DefaultMaxSessions).
	MaxSessions int
	// IdleTimeout stops a launched app unused for that long (0: default,
	// negative: never).
	IdleTimeout time.Duration
	// StaticTools lists every tool from the start, for hosts that ignore
	// tools/list_changed (framework tools then answer an error without a
	// session of their framework).
	StaticTools bool
}

// Server holds the application sessions and the tool handlers.
type Server struct {
	opts  Options
	mgr   *desktop.Manager
	trace *Tracer

	// srv is the MCP server Build returned; active lists the framework
	// tools registered on it.
	dynMu  sync.Mutex
	srv    *mcpsrv.MCPServer
	active map[string]bool
}

// New creates a server; Build registers the tools on an MCP server.
func New(opts Options) (*Server, error) {
	mgr, err := desktop.NewManager(opts.ArtifactsRoot)
	if err == nil {
		mgr.MaxSessions, mgr.IdleTimeout = opts.MaxSessions, opts.IdleTimeout
	}
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, mgr: mgr, trace: NewTracer(tracePath())}, nil
}

// Manager exposes the session manager (CLI flows reuse it).
func (s *Server) Manager() *desktop.Manager { return s.mgr }

// Instructions is what the MCP host shows the model about this server.
const Instructions = `mcp-app-desktop drives desktop applications built with Electron or Tauri, for development and debugging. The framework is recognised by itself.
Loop: launch (project dir, .app, executable, or a dev command) or attach → snapshot (refs @N) → click/type/press/… by ref, CSS selector or getByRole/getByText/getByLabel → wait_for → logs on errors → screenshot when layout matters → stop.
Actions return a diff of the interactive elements; re-snapshot after a navigation or a big UI change (refs are per window).
eval runs JavaScript in a window. Framework tools appear while a session of their framework is open: eval_main (Electron main process: app, BrowserWindow, ipcMain, dialog…), invoke and emit (Tauri commands and events; logs source ipc shows every command the page called).
Tauri on macOS and Linux is driven through tauri-plugin-mcp-app-desktop, which the app registers in debug builds; on Windows through WebView2's DevTools port.
A dev command (bun run dev, tauri dev) that restarts the app on recompilation is followed automatically, by a launched or an attached session: tools answer "restarting" until the new instance is reachable; never attach again (attach to an endpoint already driven returns its session).
When a call fails, hangs, times out or is interrupted, read trace (problems: true; with_stacks: true for a call reported stuck): every call's arguments, duration and outcome, host cancellations, and the server's goroutines when a call ran past 20 s. Use it to find the cause and fix mcp-app-desktop itself.`

// toolDef is one entry of the tool table.
type toolDef struct {
	tool    mcpgo.Tool
	handler func(*Server, context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	// framework is set for a tool listed only while a session of that
	// framework is open.
	framework string
}

func (s *Server) serverTool(def toolDef) mcpsrv.ServerTool {
	h := def.handler
	return mcpsrv.ServerTool{Tool: def.tool, Handler: s.trace.Wrap(def.tool.Name, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return h(s, ctx, req)
	})}
}

// Build returns an MCP server with the tools registered: every common tool,
// and the framework tools of the open sessions (all of them with
// StaticTools).
func (s *Server) Build() *mcpsrv.MCPServer {
	srv := mcpsrv.NewMCPServer("mcp-app-desktop", s.opts.Version,
		mcpsrv.WithToolCapabilities(!s.opts.StaticTools),
		mcpsrv.WithInstructions(Instructions),
		mcpsrv.WithRecovery(),
	)
	var common []mcpsrv.ServerTool
	for _, def := range s.tools() {
		if def.framework == "" || s.opts.StaticTools {
			common = append(common, s.serverTool(def))
		}
	}
	srv.AddTools(common...)
	s.dynMu.Lock()
	s.srv, s.active = srv, map[string]bool{}
	s.dynMu.Unlock()
	if !s.opts.StaticTools {
		s.mgr.OnChange = s.syncTools
		s.syncTools()
	}
	return srv
}

// syncTools lists the framework tools of the frameworks open, and only
// those; the host is notified when the list changes.
func (s *Server) syncTools() {
	open := s.mgr.Frameworks()
	s.dynMu.Lock()
	defer s.dynMu.Unlock()
	if s.srv == nil {
		return
	}
	var add []mcpsrv.ServerTool
	var drop []string
	for _, def := range s.tools() {
		if def.framework == "" {
			continue
		}
		want, has := open[def.framework], s.active[def.tool.Name]
		switch {
		case want && !has:
			add = append(add, s.serverTool(def))
			s.active[def.tool.Name] = true
		case !want && has:
			drop = append(drop, def.tool.Name)
			delete(s.active, def.tool.Name)
		}
	}
	if len(drop) > 0 {
		s.srv.DeleteTools(drop...)
	}
	if len(add) > 0 {
		s.srv.AddTools(add...)
	}
}

// Tools returns the sorted tool names.
func (s *Server) Tools() []string {
	defs := s.tools()
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.tool.Name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) tools() []toolDef {
	var defs []toolDef
	defs = append(defs, s.sessionTools()...)
	defs = append(defs, s.pageTools()...)
	defs = append(defs, s.frameworkTools()...)
	defs = append(defs, s.flowTools()...)
	defs = append(defs, s.traceTools()...)
	return defs
}

// Close stops every application the server launched.
func (s *Server) Close() {
	s.mgr.Close()
	s.trace.Close()
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[mcp-app-desktop] "+strings.TrimRight(format, "\n")+"\n", args...)
}
