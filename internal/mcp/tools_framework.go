package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// frameworkTools are listed only while a session of their framework is open.
func (s *Server) frameworkTools() []toolDef {
	return []toolDef{
		{mcpgo.NewTool("eval_main",
			mcpgo.WithDescription("Electron only. Evaluate JavaScript in the Electron main process through its Node inspector. require, app, BrowserWindow, webContents, ipcMain, dialog, session, Menu, shell, nativeTheme, screen are in scope. Returns the value of the last expression like the DevTools console: a bare expression, an IIFE ((() => {...; return x})()), an async IIFE, statements ending with an expression, or statements with return / top-level await; promises are awaited within timeout_ms; strings come back as is, other values as JSON; exceptions and rejections are reported as errors. Examples: BrowserWindow.getAllWindows().map(w => ({id: w.id, title: w.getTitle(), bounds: w.getBounds()})); ipcMain.eventNames(); app.getPath('userData'); dialog.showOpenDialog = async () => ({canceled: false, filePaths: ['/tmp/x']})."),
			mcpgo.WithString("expression", mcpgo.Required()),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("How long to wait for the result, promise included (default 8000, max 120000); synchronous code running longer is terminated."), mcpgo.DefaultNumber(8000)),
			mcpgo.WithTitleAnnotation("Evaluate in main process"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleEvalMain, desktop.FrameworkElectron},
		{mcpgo.NewTool("invoke",
			mcpgo.WithDescription("Tauri only. Call a Tauri command (a #[tauri::command] of the app, or a plugin command such as plugin:dialog|open) from a window, exactly as the page does with invoke(), and return its result (strings as is, other values as JSON); a rejected command is reported as an error. The capabilities of that window apply. Every command the page itself calls is in logs (source ipc)."),
			mcpgo.WithString("command", mcpgo.Required(), mcpgo.Description("Command name, e.g. greet or plugin:fs|read_text_file.")),
			mcpgo.WithObject("args", mcpgo.Description("Command arguments as a JSON object (camelCase keys, as the JavaScript side passes them).")),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("window", windowProp()),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("How long to wait for the command (default 8000, max 120000)."), mcpgo.DefaultNumber(8000)),
			mcpgo.WithTitleAnnotation("Invoke Tauri command"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleInvoke, desktop.FrameworkTauri},
		{mcpgo.NewTool("emit",
			mcpgo.WithDescription("Tauri only. Emit a Tauri event to every listener (Rust and windows), as the page does with emit() from @tauri-apps/api/event."),
			mcpgo.WithString("event", mcpgo.Required(), mcpgo.Description("Event name.")),
			mcpgo.WithString("payload", mcpgo.Description("Payload as JSON text (a string, number, object…); omitted: null.")),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("window", windowProp()),
			mcpgo.WithTitleAnnotation("Emit Tauri event"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleEmit, desktop.FrameworkTauri},
	}
}

func (s *Server) handleEvalMain(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	expr := mcpgo.ParseString(req, "expression", "")
	if strings.TrimSpace(expr) == "" {
		return errorf("expression is required")
	}
	timeout := evalTimeout(req)
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	if sess.Framework != desktop.FrameworkElectron {
		return errorf("eval_main needs an Electron session: %s is a %s app, which has no JavaScript main process (use eval in a window, or invoke for its commands)", sess.Name, sess.Framework)
	}
	if _, err := sess.App(ctx); err != nil {
		return errResult(err)
	}
	main := sess.Main()
	if main == nil {
		st := sess.Status()
		if st.Inspector == "" {
			return errorf("no main process inspector for this session: launch with inspect: true (default) or attach with inspector_endpoint")
		}
		return errorf("the main process inspector at %s is not connected yet (a packaged app may refuse --inspect). Check status and logs", st.Inspector)
	}
	value, err := main.Eval(ctx, expr, timeout)
	if err != nil {
		return errResult(err)
	}
	return text(value)
}

// tauriWindow runs fn on a window of a Tauri session.
func (s *Server) tauriWindow(ctx context.Context, req mcpgo.CallToolRequest, tool string, fn pageFn) (*mcpgo.CallToolResult, error) {
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	if sess.Framework != desktop.FrameworkTauri {
		return errorf("%s needs a Tauri session: %s is a %s app (use eval, or eval_main for Electron's main process)", tool, sess.Name, sess.Framework)
	}
	return s.withPage(ctx, req, fn)
}

func (s *Server) handleInvoke(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	command := strings.TrimSpace(mcpgo.ParseString(req, "command", ""))
	if command == "" {
		return errorf("command is required")
	}
	var args map[string]any
	if raw, ok := req.GetArguments()["args"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return errorf("args must be a JSON object")
		}
		args = m
	}
	script, err := tauri.InvokeScript(command, args)
	if err != nil {
		return errResult(err)
	}
	timeout := evalTimeout(req)
	return s.tauriWindow(ctx, req, "invoke", func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		value, err := w.Eval(ctx, script, "", timeout)
		if err != nil {
			return errResult(fmt.Errorf("invoke %s: %w", command, err))
		}
		return textf("%s → (%s)\n%s", command, time.Since(start).Round(time.Millisecond), value)
	})
}

func (s *Server) handleEmit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	event := strings.TrimSpace(mcpgo.ParseString(req, "event", ""))
	if event == "" {
		return errorf("event is required")
	}
	var payload any
	if raw := strings.TrimSpace(mcpgo.ParseString(req, "payload", "")); raw != "" {
		if err := jsonUnmarshalLoose(raw, &payload); err != nil {
			return errorf("payload must be JSON text (\"a string\", 42, {\"k\": 1}): %v", err)
		}
	}
	script, err := tauri.EmitScript(event, payload)
	if err != nil {
		return errResult(err)
	}
	return s.tauriWindow(ctx, req, "emit", func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		value, err := w.Eval(ctx, script, "", 8*time.Second)
		if err != nil {
			return errResult(fmt.Errorf("emit %s: %w", event, err))
		}
		return text(value)
	})
}
