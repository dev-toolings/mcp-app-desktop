package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// session resolves the session argument (the only session when omitted).
func (s *Server) session(req mcpgo.CallToolRequest) (*desktop.Session, error) {
	return s.mgr.Get(strings.TrimSpace(mcpgo.ParseString(req, "session", "")))
}

// pageFn is the body of a tool that works on one window of one session.
type pageFn func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error)

// withPage resolves the session and window, serialises UI actions on the
// session, and runs fn. Errors become tool errors, never protocol errors, so
// the agent reads them.
func (s *Server) withPage(ctx context.Context, req mcpgo.CallToolRequest, fn pageFn) (*mcpgo.CallToolResult, error) {
	return s.withPageWithin(ctx, req, pageCallCeiling, fn)
}

// pageCallCeiling bounds one page tool call, whatever its own timeouts: the
// longest a tool may be asked to wait is 2 minutes (timeout_ms), and the
// session's action lock must never be held for longer than that by a call
// whose CDP request the page never answers (a screenshot held it 19 minutes,
// trace 2026-10-24). Flows, which chain many steps, are not bounded by it.
const pageCallCeiling = 3 * time.Minute

// actionWait is how long a page action waits for the previous action of the
// same session before answering « busy » instead of queueing behind it.
const actionWait = 15 * time.Second

// withPageWithin is withPage bounded by limit (0: only by ctx).
func (s *Server) withPageWithin(ctx context.Context, req mcpgo.CallToolRequest, limit time.Duration, fn pageFn) (*mcpgo.CallToolResult, error) {
	return s.runOnPage(ctx, req, limit, true, fn)
}

// observePage runs a read of the window (snapshot, screenshot, eval) without
// waiting for the session's action gate: while an action is stuck, they are
// how the agent sees why. Bounded by the same ceiling as an action.
func (s *Server) observePage(ctx context.Context, req mcpgo.CallToolRequest, fn pageFn) (*mcpgo.CallToolResult, error) {
	return s.runOnPage(ctx, req, pageCallCeiling, false, fn)
}

func (s *Server) runOnPage(ctx context.Context, req mcpgo.CallToolRequest, limit time.Duration, exclusive bool, fn pageFn) (*mcpgo.CallToolResult, error) {
	sess, err := s.session(req)
	if err != nil {
		return errResult(err)
	}
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	window := mcpgo.ParseInt(req, "window", 0)
	if exclusive {
		release, err := sess.AcquireActions(ctx, req.Params.Name, actionWait)
		if err != nil {
			return errResult(err)
		}
		defer release()
	}
	w, err := sess.Window(ctx, window)
	if err != nil {
		return errResult(err)
	}
	res, err := fn(sess, w)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && limit > 0 && res != nil && res.IsError {
		res.Content = append(res.Content, mcpgo.NewTextContent(fmt.Sprintf("\n(the call reached the %s ceiling of a page tool: the window stopped answering; check status and logs)", limit)))
	}
	return res, err
}

// normalizeTarget accepts "@3", "3", "e3", "ref:3" (refs), a CSS selector,
// or a Playwright locator (getByRole/getByText/getByLabel). The shorthands
// text=…, label=… and role=NAME[ name=…] are expanded to locators.
func normalizeTarget(in string) string {
	t := strings.TrimSpace(in)
	if t == "" {
		return ""
	}
	t = strings.TrimPrefix(t, "ref:")
	if isDigits(t) {
		return "@" + t
	}
	if strings.HasPrefix(t, "@") || strings.HasPrefix(t, "getBy") {
		return t
	}
	if r := engine.InternalRef(t); r != t {
		return r
	}
	if v, ok := strings.CutPrefix(t, "text="); ok {
		return fmt.Sprintf("getByText(%q)", strings.Trim(v, `"'`))
	}
	if v, ok := strings.CutPrefix(t, "label="); ok {
		return fmt.Sprintf("getByLabel(%q)", strings.Trim(v, `"'`))
	}
	if v, ok := strings.CutPrefix(t, "role="); ok {
		// Playwright's own form, role=textbox[name="Message"], read as
		// role=textbox name="Message" (it used to become a role named
		// « textbox[name=… » that never matched).
		if m := roleBracketName.FindStringSubmatch(v); m != nil {
			v = m[1] + " name=" + m[2]
		}
		role, name, hasName := strings.Cut(v, " name=")
		role = strings.TrimSpace(role)
		if !hasName {
			return fmt.Sprintf("getByRole(%q)", role)
		}
		name = strings.TrimSpace(name)
		if regexLiteral.MatchString(name) {
			return fmt.Sprintf("getByRole(%q, { name: %s })", role, name)
		}
		return fmt.Sprintf("getByRole(%q, { name: %q })", role, strings.Trim(name, `"'`))
	}
	return t
}

// roleBracketName matches ROLE[name=VALUE] (VALUE quoted or a regex).
var roleBracketName = regexp.MustCompile(`^\s*([A-Za-z]+)\s*\[\s*name\s*=\s*(.+?)\s*\]\s*$`)

// regexLiteral recognises a JavaScript regex literal (/source/flags) given
// as the name of the role=… shorthand.
var regexLiteral = regexp.MustCompile(`^/.+/[a-z]*$`)

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isRef(target string) bool { return strings.HasPrefix(target, "@") }

// timeoutMS reads timeout_ms, or « timeout » (milliseconds too) when only
// that was given: a wait_for with timeout "30000" silently used the 10 s
// default and failed early (trace, 2026-10-24).
func timeoutMS(req mcpgo.CallToolRequest, def int) int {
	args := req.GetArguments()
	if _, ok := args["timeout_ms"]; !ok {
		if _, ok := args["timeout"]; ok {
			return mcpgo.ParseInt(req, "timeout", def)
		}
	}
	return mcpgo.ParseInt(req, "timeout_ms", def)
}

// lookupTimeout bounds how long an action waits for its target to appear.
func lookupTimeout(req mcpgo.CallToolRequest) time.Duration {
	ms := timeoutMS(req, 10000)
	if ms <= 0 {
		ms = 10000
	}
	if ms > 120000 {
		ms = 120000
	}
	return time.Duration(ms) * time.Millisecond
}

// evalTimeout bounds eval and eval_main: 8 s by default, 2 minutes at most.
func evalTimeout(req mcpgo.CallToolRequest) time.Duration {
	ms := timeoutMS(req, 8000)
	if ms <= 0 {
		ms = 8000
	}
	if ms > 120000 {
		ms = 120000
	}
	return time.Duration(ms) * time.Millisecond
}

// parseHoverMode reads the click tool's hover_first: absent is auto (hover
// only a target hidden by visibility or opacity), true always hovers first,
// false never does.
func parseHoverMode(req mcpgo.CallToolRequest) driver.HoverMode {
	v, ok := req.GetArguments()["hover_first"]
	if !ok || v == nil {
		return driver.HoverAuto
	}
	if b, ok := v.(bool); ok {
		if b {
			return driver.HoverAlways
		}
		return driver.HoverNever
	}
	return driver.HoverAuto
}

func parseButton(s string) (string, error) {
	switch b := strings.ToLower(strings.TrimSpace(s)); b {
	case "", "left":
		return "left", nil
	case "right", "middle":
		return b, nil
	}
	return "", fmt.Errorf("button must be left, right or middle")
}

func snapshotMode(req mcpgo.CallToolRequest) (engine.SnapshotMode, error) {
	return engine.ParseSnapshotMode(mcpgo.ParseString(req, "snapshot", "diff"))
}

// mutate runs an action and reports what changed on the window according
// to the snapshot mode: nothing, a diff of the interactive elements, or the
// whole skeleton.
func mutate(ctx context.Context, w driver.Window, mode engine.SnapshotMode, summary string, act func() error) (*mcpgo.CallToolResult, error) {
	return mutateWith(ctx, w, mode, func() string { return summary }, act)
}

// mutateWith is mutate with a summary computed after the action ran.
func mutateWith(ctx context.Context, w driver.Window, mode engine.SnapshotMode, summaryFn func() string, act func() error) (*mcpgo.CallToolResult, error) {
	prev := w.Refs()
	if err := act(); err != nil {
		return errResult(err)
	}
	summary := summaryFn()
	if mode == engine.SnapshotModeNone {
		return text(summary)
	}
	diff, result, err := w.Mutation(ctx, prev)
	if err != nil {
		return textf("%s\n(page changed but could not be re-read: %v)", summary, err)
	}
	if mode == engine.SnapshotModeFull {
		return textf("%s\n%s", summary, renderTree(result))
	}
	return textf("%s\n%s", summary, engine.FormatDiff(diff))
}
