package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const targetDesc = "Element: @ref from the last snapshot, a unique CSS selector, or getByRole(\"button\", { name: \"Save\" }) / getByText(\"Save\") / getByLabel(\"Email\") (shorthands text=…, label=…, role=button name=…). Waits until it is visible and enabled."

func windowProp() mcpgo.PropertyOption {
	return mcpgo.Description("Window index from windows (default 0).")
}

func snapshotProp() mcpgo.PropertyOption {
	return mcpgo.Description("What to return after the action: diff of interactive elements (default), full skeleton, or none.")
}

func (s *Server) pageTools() []toolDef {
	common := func(name, desc string, opts ...mcpgo.ToolOption) mcpgo.Tool {
		base := []mcpgo.ToolOption{mcpgo.WithDescription(desc)}
		base = append(base, opts...)
		base = append(base,
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("window", windowProp()),
			mcpgo.WithOpenWorldHintAnnotation(false),
		)
		return mcpgo.NewTool(name, base...)
	}
	action := func(name, desc string, opts ...mcpgo.ToolOption) mcpgo.Tool {
		opts = append(opts,
			mcpgo.WithString("snapshot", snapshotProp(), mcpgo.Enum("none", "diff", "full"), mcpgo.DefaultString("diff")),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("How long to wait for the target (default 10000).")),
		)
		return common(name, desc, opts...)
	}
	return []toolDef{
		{common("snapshot",
			"The window's accessibility tree with refs (@N) for interactive elements, plus its URL, title and the errors logged since the previous snapshot. The canonical first call before acting. level: skeleton (interactive only), content (adds text, default), full (every named node). selector scopes the tree to a CSS subtree.",
			mcpgo.WithString("level", mcpgo.Enum("skeleton", "content", "full"), mcpgo.DefaultString("content")),
			mcpgo.WithString("selector", mcpgo.Description("CSS selector scoping the extraction to one subtree.")),
			mcpgo.WithTitleAnnotation("Snapshot window"), mcpgo.WithReadOnlyHintAnnotation(true),
		), (*Server).handleSnapshot, ""},
		{action("click", "Click an element after it is visible, enabled and not covered: real mouse events at its centre (Electron, Tauri on Windows), or the same DOM events dispatched to it (Tauri bridge, which cannot apply CSS :hover: a button that only shows on hover is clicked while still transparent). Per-row buttons that only appear on hover are revealed by hovering first (see hover_first).",
			mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithString("button", mcpgo.Enum("left", "right", "middle"), mcpgo.DefaultString("left")),
			mcpgo.WithBoolean("double", mcpgo.Description("Double-click."), mcpgo.DefaultBool(false)),
			mcpgo.WithBoolean("hover_first", mcpgo.Description("Buttons shown only on hover (CSS :hover on a row). Omitted: a target hidden by visibility or opacity is revealed by moving the real mouse over it first. true: always hover first, and reveal a display:none target by hovering its nearest rendered ancestor (the row). false: never hover; the target must already be visible.")),
			mcpgo.WithTitleAnnotation("Click"),
		), (*Server).handleClick, ""},
		{action("type", "Type text into an element (clears it first unless append: true). submit: true presses Enter afterwards. Without target, types into the focused element.",
			mcpgo.WithString("text", mcpgo.Required()),
			mcpgo.WithString("target", mcpgo.Description(targetDesc)),
			mcpgo.WithBoolean("append", mcpgo.Description("Keep the existing value."), mcpgo.DefaultBool(false)),
			mcpgo.WithBoolean("submit", mcpgo.Description("Press Enter after typing."), mcpgo.DefaultBool(false)),
			mcpgo.WithTitleAnnotation("Type text"),
		), (*Server).handleType, ""},
		{action("fill_form", "Fill several fields in one call: a JSON object mapping targets to values, e.g. {\"@3\": \"Ada\", \"getByLabel(\\\"Email\\\")\": \"ada@example.com\"}.",
			mcpgo.WithString("fields", mcpgo.Required(), mcpgo.Description("JSON object: target → text (or a list of {\"target\", \"value\"}, filled in order).")),
			mcpgo.WithTitleAnnotation("Fill form"),
		), (*Server).handleFillForm, ""},
		{action("select", "Choose option(s) of a <select> by visible label or value.",
			mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithArray("values", mcpgo.Required(), mcpgo.Description("Option labels or values."), mcpgo.WithStringItems()),
			mcpgo.WithTitleAnnotation("Select option"),
		), (*Server).handleSelect, ""},
		{action("press", "Press a key or shortcut: Enter, Escape, Tab, ArrowDown, cmd+k, ctrl+shift+p, shift+Tab… With target, focuses it first.",
			mcpgo.WithString("key", mcpgo.Required()),
			mcpgo.WithString("target", mcpgo.Description(targetDesc)),
			mcpgo.WithTitleAnnotation("Press key"),
		), (*Server).handlePress, ""},
		{action("hover", "Move the mouse over an element (tooltips, hover menus).",
			mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithTitleAnnotation("Hover"),
		), (*Server).handleHover, ""},
		{action("drag", "Drag one element onto another with real mouse events.",
			mcpgo.WithString("from", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithString("to", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithNumber("steps", mcpgo.Description("Intermediate mouse moves (default 10)."), mcpgo.DefaultNumber(10)),
			mcpgo.WithTitleAnnotation("Drag and drop"),
		), (*Server).handleDrag, ""},
		{action("scroll", "Scroll an element into view (target), or the window by dy pixels (negative scrolls up).",
			mcpgo.WithString("target", mcpgo.Description(targetDesc)),
			mcpgo.WithNumber("dy", mcpgo.Description("Pixels to scroll the window by when no target is given.")),
			mcpgo.WithTitleAnnotation("Scroll"),
		), (*Server).handleScroll, ""},
		{action("wheel", "Real mouse-wheel events (the page gets `wheel` events, unlike scroll) over an element's centre (target) or at x/y: dy pixels in total (negative scrolls up), split into steps events interval_ms apart. Several calls in a row reproduce fast flicks and direction changes.",
			mcpgo.WithString("target", mcpgo.Description(targetDesc)),
			mcpgo.WithNumber("x", mcpgo.Description("Viewport X when no target is given.")),
			mcpgo.WithNumber("y", mcpgo.Description("Viewport Y when no target is given.")),
			mcpgo.WithNumber("dy", mcpgo.Required(), mcpgo.Description("Total pixels (negative scrolls up).")),
			mcpgo.WithNumber("steps", mcpgo.Description("Wheel events (default 10, max 500)."), mcpgo.DefaultNumber(10)),
			mcpgo.WithNumber("interval_ms", mcpgo.Description("Delay between events (default 16, max 1000)."), mcpgo.DefaultNumber(16)),
			mcpgo.WithTitleAnnotation("Mouse wheel"),
		), (*Server).handleWheel, ""},
		{action("upload", "Set the files of a file input (target may be the hidden <input type=file> as a CSS selector).",
			mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description(targetDesc)),
			mcpgo.WithArray("files", mcpgo.Required(), mcpgo.Description("Absolute file paths."), mcpgo.WithStringItems()),
			mcpgo.WithTitleAnnotation("Upload files"),
		), (*Server).handleUpload, ""},
		{common("wait_for", "Wait until a condition holds: target visible (or state hidden/attached/enabled), text present in the page, url containing a string, or a load state (load, domcontentloaded, stable, idle). Without a condition, waits timeout_ms.",
			mcpgo.WithString("target", mcpgo.Description(targetDesc)),
			mcpgo.WithString("state", mcpgo.Enum("visible", "hidden", "attached", "enabled", "stable"), mcpgo.DefaultString("visible")),
			mcpgo.WithString("text", mcpgo.Description("Visible text substring to wait for.")),
			mcpgo.WithString("url", mcpgo.Description("Substring the window URL must contain.")),
			mcpgo.WithString("load", mcpgo.Enum("load", "domcontentloaded", "stable", "idle")),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("Default 10000, max 120000."), mcpgo.DefaultNumber(10000)),
			mcpgo.WithTitleAnnotation("Wait for"), mcpgo.WithReadOnlyHintAnnotation(true),
		), (*Server).handleWaitFor, ""},
		{common("screenshot", "Capture the window (or one element) as an image. webp quality 60 by default (JPEG through the Tauri bridge, which has no webp encoder); save: true also writes a full-resolution PNG in the session's artifacts directory and returns its path (deleted by stop unless keep_artifacts). full_page is CDP only.",
			mcpgo.WithString("target", mcpgo.Description("Element to capture instead of the whole viewport.")),
			mcpgo.WithBoolean("full_page", mcpgo.Description("Capture beyond the viewport."), mcpgo.DefaultBool(false)),
			mcpgo.WithString("format", mcpgo.Enum("webp", "jpeg", "png"), mcpgo.DefaultString("webp")),
			mcpgo.WithNumber("quality", mcpgo.Description("1-100 for webp/jpeg (default 60)."), mcpgo.DefaultNumber(60)),
			mcpgo.WithBoolean("save", mcpgo.Description("Also save a PNG under the session artifacts dir."), mcpgo.DefaultBool(false)),
			mcpgo.WithString("name", mcpgo.Description("File name for the saved PNG.")),
			mcpgo.WithTitleAnnotation("Screenshot"), mcpgo.WithReadOnlyHintAnnotation(true),
		), (*Server).handleScreenshot, ""},
		{common("eval", "Evaluate JavaScript in the window (renderer, webview). Promises are awaited; the result is serialised. With target, `this` is that element. Use it to read app state, localStorage, or call the preload bridge (Electron: window.<api>; Tauri: window.__TAURI_INTERNALS__, or the invoke tool). A strict CSP without 'unsafe-eval' is fine: the expression is never passed to eval.",
			mcpgo.WithString("expression", mcpgo.Required(), mcpgo.Description("JS expression; top-level await OK.")),
			mcpgo.WithString("target", mcpgo.Description("Element bound as `this`.")),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("How long to wait for the result, promise included (default 8000, max 120000). On timeout the error says whether the window still answers (slow promise) or its main thread is busy."), mcpgo.DefaultNumber(8000)),
			mcpgo.WithTitleAnnotation("Evaluate in renderer"),
		), (*Server).handleEval, ""},
		{common("navigate", "Load a URL in the window (http(s), file, or the app's custom scheme), or go back/forward in its history.",
			mcpgo.WithString("url", mcpgo.Description("Absolute URL to load.")),
			mcpgo.WithString("history", mcpgo.Enum("back", "forward"), mcpgo.Description("Instead of url: step in history.")),
			mcpgo.WithString("wait", mcpgo.Enum("load", "domcontentloaded", "stable", "idle", "none"), mcpgo.DefaultString("load")),
			mcpgo.WithTitleAnnotation("Navigate"),
		), (*Server).handleNavigate, ""},
		{common("reload", "Reload the window's page (renderer only; the main process keeps running). Returns the new snapshot.",
			mcpgo.WithBoolean("ignore_cache", mcpgo.DefaultBool(false)),
			mcpgo.WithTitleAnnotation("Reload window"),
		), (*Server).handleReload, ""},
		{common("dialog", "How JavaScript dialogs (alert, confirm, prompt) of this session are answered from now on: accept (default) or dismiss, with an optional prompt text. Native dialogs (open/save file) are stubbed with eval_main on `dialog` in Electron; in Tauri they open for real (tauri-plugin-dialog).",
			mcpgo.WithString("action", mcpgo.Enum("accept", "dismiss"), mcpgo.DefaultString("accept")),
			mcpgo.WithString("text", mcpgo.Description("Prompt answer when accepting.")),
			mcpgo.WithTitleAnnotation("Dialog policy"),
		), (*Server).handleDialog, ""},
	}
}

// windowsText renders the window list in one line per window.
func windowsText(wins []driver.WindowInfo) string {
	lines := make([]string, 0, len(wins))
	for _, w := range wins {
		title := w.Title
		if title == "" {
			title = "(untitled)"
		}
		kind := ""
		if w.Type != "" && w.Type != "page" {
			kind = " " + w.Type
		}
		if w.Label != "" {
			kind += " [" + w.Label + "]"
		}
		lines = append(lines, fmt.Sprintf("window %d%s: %q %s", w.Index, kind, title, w.URL))
	}
	return strings.Join(lines, "\n")
}

// snapshotText extracts one window and renders header + tree + recent errors.
func (s *Server) snapshotText(ctx context.Context, sess *desktop.Session, w driver.Window, level, selector string, withLegend bool) (string, error) {
	lvl := engine.ExtractLevel(level)
	if err := engine.ValidateExtractLevel(lvl); err != nil {
		return "", err
	}
	result, err := w.Snapshot(ctx, lvl, selector)
	if err != nil {
		return "", err
	}
	url, title, _ := w.Info(ctx)
	window := w.Index()
	var sb strings.Builder
	fmt.Fprintf(&sb, "[window %d] %q %s | %d interactive", window, title, url, result.Stats.InteractiveCount)
	if len(result.Warnings) > 0 {
		sb.WriteString(" | warning: " + strings.Join(result.Warnings, "; "))
	}
	errs := recentErrors(sess, window, s.markSnapshot(sess))
	if len(errs) > 0 {
		fmt.Fprintf(&sb, " | %d error(s) since last snapshot", len(errs))
	}
	sb.WriteString("\n")
	sb.WriteString(renderTree(result))
	if len(errs) > 0 {
		sb.WriteString("\nerrors:")
		for _, e := range errs {
			sb.WriteString("\n  " + formatLog(e))
		}
	}
	if withLegend {
		sb.WriteString("\n" + legend)
	}
	return sb.String(), nil
}

// snapshotMarks remembers when each session was last snapshotted so the next
// snapshot can list the errors logged in between.
var snapshotMarks sync.Map // session id → time.Time

func (s *Server) markSnapshot(sess *desktop.Session) time.Time {
	now := time.Now()
	prev, loaded := snapshotMarks.Swap(sess.ID, now)
	if !loaded {
		return sess.Started
	}
	return prev.(time.Time)
}

func recentErrors(sess *desktop.Session, window int, since time.Time) []desktop.LogEntry {
	var out []desktop.LogEntry
	for _, e := range sess.Logs() {
		if !e.IsError() || e.Time.Before(since) {
			continue
		}
		if e.Window >= 0 && e.Window != window {
			continue
		}
		out = append(out, e)
	}
	if len(out) > 8 {
		out = out[len(out)-8:]
	}
	return out
}

func (s *Server) handleSnapshot(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	level := mcpgo.ParseString(req, "level", "content")
	selector := mcpgo.ParseString(req, "selector", "")
	return s.observePage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		out, err := s.snapshotText(ctx, sess, w, level, selector, false)
		if err != nil {
			return errResult(fmt.Errorf("snapshot: %w", err))
		}
		if app, err := sess.App(ctx); err == nil {
			if wins, err := app.Windows(ctx); err == nil && len(wins) > 1 {
				out += "\n" + windowsText(wins)
			}
		}
		return text(out)
	})
}

func (s *Server) handleClick(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	button, err := parseButton(mcpgo.ParseString(req, "button", "left"))
	if err != nil {
		return errResult(err)
	}
	double := mcpgo.ParseBoolean(req, "double", false)
	hover := parseHoverMode(req)
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		verb, count := "clicked", 1
		if double {
			verb, count = "double-clicked", 2
		}
		var note string
		summary := func() string { return fmt.Sprintf("%s %s%s", verb, target, note) }
		return mutateWith(ctx, w, mode, summary, func() error {
			var err error
			note, err = w.Click(ctx, target, driver.ClickOptions{Button: button, Count: count, Hover: hover, Timeout: timeout})
			return err
		})
	})
}

func (s *Server) handleType(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	textArg := mcpgo.ParseString(req, "text", "")
	appendText := mcpgo.ParseBoolean(req, "append", false)
	submit := mcpgo.ParseBoolean(req, "submit", false)
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		summary := fmt.Sprintf("typed %d chars", len([]rune(textArg)))
		if target != "" {
			summary += " into " + target
		}
		if submit {
			summary += " + Enter"
		}
		return mutate(ctx, w, mode, summary, func() error {
			return w.Type(ctx, target, textArg, driver.TypeOptions{Append: appendText, Submit: submit, Timeout: timeout})
		})
	})
}

func (s *Server) handleFillForm(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	fields, err := formFields(req.GetArguments()["fields"])
	if err != nil {
		return errResult(err)
	}
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, fmt.Sprintf("filled %d field(s)", len(fields)), func() error {
			for _, f := range fields {
				target := normalizeTarget(f.target)
				if target == "" {
					return fmt.Errorf("target is required (@ref from snapshot, a CSS selector, or getByRole/getByText/getByLabel)")
				}
				if err := w.Type(ctx, target, f.text, driver.TypeOptions{Timeout: timeout}); err != nil {
					return fmt.Errorf("%s: %w", target, err)
				}
			}
			return nil
		})
	})
}

type formField struct{ target, text string }

// formFields reads fill_form's fields: an object target → text (filled in
// target order), or a list of {target, value|text} (filled in list order),
// either as JSON text or as a structured argument. Agents sent the list form
// and got a JSON error (trace, 2026-10-24).
func formFields(raw any) ([]formField, error) {
	const want = `fields must be a JSON object of target → text ({"@3": "Ada"}) or a list of {"target": …, "value": …}`
	if text, ok := raw.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, fmt.Errorf("fields is required")
		}
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return nil, fmt.Errorf("%s: %v", want, err)
		}
	}
	var out []formField
	switch v := raw.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			text, ok := v[k].(string)
			if !ok {
				return nil, fmt.Errorf("%s: the value of %q is not a string", want, k)
			}
			out = append(out, formField{k, text})
		}
	case []any:
		for i, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: item %d is not an object", want, i)
			}
			target, _ := m["target"].(string)
			text, ok := m["value"].(string)
			if !ok {
				text, ok = m["text"].(string)
			}
			if strings.TrimSpace(target) == "" || !ok {
				return nil, fmt.Errorf("%s: item %d needs a target and a string value", want, i)
			}
			out = append(out, formField{target, text})
		}
	case nil:
	default:
		return nil, fmt.Errorf("%s", want)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("fields is required")
	}
	return out, nil
}

func (s *Server) handleSelect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	values, err := stringSlice(req, "values")
	if err != nil {
		return errResult(err)
	}
	if len(values) == 0 {
		return errorf("values is required")
	}
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, fmt.Sprintf("selected %v in %s", values, target), func() error {
			return w.Select(ctx, target, values, timeout)
		})
	})
}

func (s *Server) handlePress(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	key := mcpgo.ParseString(req, "key", "")
	if _, err := driver.ParseCombo(key); err != nil {
		return errResult(err)
	}
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		summary := "pressed " + key
		if target != "" {
			summary += " on " + target
		}
		return mutate(ctx, w, mode, summary, func() error {
			return w.Press(ctx, target, key, timeout)
		})
	})
}

func (s *Server) handleHover(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, "hovered "+target, func() error {
			return w.Hover(ctx, target, timeout)
		})
	})
}

func (s *Server) handleDrag(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	from := normalizeTarget(mcpgo.ParseString(req, "from", ""))
	to := normalizeTarget(mcpgo.ParseString(req, "to", ""))
	steps := mcpgo.ParseInt(req, "steps", 10)
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, fmt.Sprintf("dragged %s onto %s", from, to), func() error {
			return w.Drag(ctx, from, to, steps, timeout)
		})
	})
}

func (s *Server) handleScroll(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	dy := mcpgo.ParseInt(req, "dy", 0)
	if target == "" && dy == 0 {
		return errorf("give target (scroll into view) or dy (pixels)")
	}
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		summary := fmt.Sprintf("scrolled by %d", dy)
		if target != "" {
			summary = "scrolled " + target + " into view"
		}
		return mutate(ctx, w, mode, summary, func() error {
			if target != "" {
				return w.ScrollIntoView(ctx, target, timeout)
			}
			_, err := w.ScrollBy(ctx, 0, dy)
			return err
		})
	})
}

func (s *Server) handleWheel(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	x, y := mcpgo.ParseFloat64(req, "x", -1), mcpgo.ParseFloat64(req, "y", -1)
	dy := mcpgo.ParseFloat64(req, "dy", 0)
	steps := mcpgo.ParseInt(req, "steps", 10)
	interval := mcpgo.ParseInt(req, "interval_ms", 16)
	if dy == 0 {
		return errorf("dy is required and non-zero")
	}
	if target == "" && (x < 0 || y < 0) {
		return errorf("give target, or both x and y")
	}
	if steps < 1 || steps > 500 {
		return errorf("steps must be between 1 and 500")
	}
	if interval < 0 || interval > 1000 {
		return errorf("interval_ms must be between 0 and 1000")
	}
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, fmt.Sprintf("wheeled %g px in %d events", dy, steps), func() error {
			return w.Wheel(ctx, target, x, y, dy, steps, time.Duration(interval)*time.Millisecond, timeout)
		})
	})
}

func (s *Server) handleUpload(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	files, err := stringSlice(req, "files")
	if err != nil {
		return errResult(err)
	}
	if len(files) == 0 {
		return errorf("files is required")
	}
	for _, f := range files {
		if !filepath.IsAbs(f) {
			return errorf("file paths must be absolute: %s", f)
		}
		if _, err := os.Stat(f); err != nil {
			return errorf("%s: %v", f, err)
		}
	}
	mode, err := snapshotMode(req)
	if err != nil {
		return errResult(err)
	}
	timeout := lookupTimeout(req)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		return mutate(ctx, w, mode, fmt.Sprintf("uploaded %d file(s) to %s", len(files), target), func() error {
			return w.Upload(ctx, target, files, timeout)
		})
	})
}

func (s *Server) handleWaitFor(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	state := mcpgo.ParseString(req, "state", "visible")
	textArg := mcpgo.ParseString(req, "text", "")
	url := mcpgo.ParseString(req, "url", "")
	load := mcpgo.ParseString(req, "load", "")
	timeout := lookupTimeout(req)
	if target == "" && textArg == "" && url == "" && load == "" {
		select {
		case <-time.After(timeout):
		case <-ctx.Done():
			return errResult(ctx.Err())
		}
		return textf("waited %s (no condition)", timeout)
	}
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		if err := w.Wait(ctx, engine.WaitSpec{Selector: target, Text: textArg, URL: url, Load: load, State: state, Timeout: timeout}); err != nil {
			return errResult(err)
		}
		label := firstNonEmpty(target, textArg, url, load)
		if (target != "" || textArg != "") && state != "visible" {
			label += " (" + state + ")"
		}
		return textf("condition met after %s: %s", time.Since(start).Round(time.Millisecond), label)
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleScreenshot(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	fullPage := mcpgo.ParseBoolean(req, "full_page", false)
	format := mcpgo.ParseString(req, "format", "webp")
	quality := mcpgo.ParseInt(req, "quality", 60)
	if quality < 1 || quality > 100 {
		quality = 60
	}
	save := mcpgo.ParseBoolean(req, "save", false)
	name := mcpgo.ParseString(req, "name", "")
	return s.observePage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		shot, err := w.Screenshot(ctx, driver.ShotOptions{Target: target, FullPage: fullPage, Format: format, Quality: quality})
		if err != nil {
			return errResult(fmt.Errorf("screenshot: %w", err))
		}
		summary := fmt.Sprintf("window %d %s (%d bytes)", w.Index(), shot.Format, len(shot.Data))
		if shot.Note != "" {
			summary += " (" + shot.Note + ")"
		}
		if save {
			png := shot
			if shot.Format != "png" {
				if png, err = w.Screenshot(ctx, driver.ShotOptions{Target: target, FullPage: fullPage, Format: "png"}); err != nil {
					return errResult(fmt.Errorf("screenshot png: %w", err))
				}
			}
			path, err := saveArtifact(sess.Dir, name, "screenshot-"+time.Now().Format("150405.000"), ".png", png.Data)
			if err != nil {
				return errResult(err)
			}
			summary += " saved to " + path
		}
		return mcpgo.NewToolResultImage(summary, base64.StdEncoding.EncodeToString(shot.Data), "image/"+shot.Format), nil
	})
}

// saveArtifact writes data under dir with a safe file name.
func saveArtifact(dir, name, fallback, ext string, data []byte) (string, error) {
	if name == "" {
		name = fallback
	}
	name = filepath.Base(filepath.Clean(name))
	if name == "." || name == "/" || name == "" {
		name = fallback
	}
	if !strings.HasSuffix(strings.ToLower(name), ext) {
		name += ext
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, data, 0o600)
}

func (s *Server) handleEval(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	expr := mcpgo.ParseString(req, "expression", "")
	if strings.TrimSpace(expr) == "" {
		return errorf("expression is required")
	}
	target := normalizeTarget(mcpgo.ParseString(req, "target", ""))
	timeout := evalTimeout(req)
	return s.observePage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		value, err := w.Eval(ctx, expr, target, timeout)
		if err != nil {
			return errResult(fmt.Errorf("eval: %w", err))
		}
		return text(value)
	})
}

func (s *Server) handleNavigate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	url := strings.TrimSpace(mcpgo.ParseString(req, "url", ""))
	history := mcpgo.ParseString(req, "history", "")
	wait := mcpgo.ParseString(req, "wait", "load")
	if url == "" && history == "" {
		return errorf("give url or history")
	}
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		if history != "" {
			delta := 1
			if history == "back" {
				delta = -1
			}
			if err := w.History(ctx, delta, wait); err != nil {
				return errResult(fmt.Errorf("%s: %w", history, err))
			}
		} else if err := w.Navigate(ctx, url, wait); err != nil {
			return errResult(fmt.Errorf("navigate: %w", err))
		}
		out, err := s.snapshotText(ctx, sess, w, "content", "", false)
		if err != nil {
			if u, _, ierr := w.Info(ctx); ierr == nil {
				return textf("navigated to %s (snapshot failed: %v)", u, err)
			}
			return errResult(err)
		}
		return text(out)
	})
}

func (s *Server) handleReload(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	ignoreCache := mcpgo.ParseBoolean(req, "ignore_cache", false)
	return s.withPage(ctx, req, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		if err := w.Reload(ctx, ignoreCache); err != nil {
			return errResult(err)
		}
		out, err := s.snapshotText(ctx, sess, w, "content", "", false)
		if err != nil {
			return textf("reloaded (snapshot failed: %v)", err)
		}
		return text("reloaded\n" + out)
	})
}

func (s *Server) handleDialog(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	action := strings.ToLower(mcpgo.ParseString(req, "action", "accept"))
	textArg := mcpgo.ParseString(req, "text", "")
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	sess.Dialogs().Set(action != "dismiss", textArg)
	if action != "dismiss" {
		action = "accept"
	}
	return textf("JavaScript dialogs will %s", action)
}
