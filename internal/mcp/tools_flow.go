package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/desktop"
	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/flow"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const flowDesc = "Run a Maestro-style YAML flow against a window and report every step. Give flow (YAML text) or file (path). Commands: launchApp, tapOn, doubleTapOn, inputText, eraseText, pressKey, assertVisible, assertNotVisible, scroll, scrollUntilVisible, waitForAnimationToEnd, extendedWaitUntil, takeScreenshot, runFlow, repeat, evalJs, assertJs, evalMain, assertMain. Selectors: a string (visible text or accessible name, case-insensitive, must be unique) or a mapping with text, role (+text as name), label, id (DOM id or data-testid), css, ref; ${NAME} expands from env. Any command accepts label: and optional: true. A failing step stops the flow and returns the screen and a screenshot path."

func (s *Server) flowTools() []toolDef {
	return []toolDef{
		{mcpgo.NewTool("run_flow",
			mcpgo.WithDescription(flowDesc),
			mcpgo.WithString("flow", mcpgo.Description("Flow YAML (optional header document with name/env, then the command list).")),
			mcpgo.WithString("file", mcpgo.Description("Path to a flow file (runFlow references resolve relative to it).")),
			mcpgo.WithObject("env", mcpgo.Description("${NAME} values; the flow header's env wins."), mcpgo.AdditionalProperties(map[string]any{"type": "string"})),
			mcpgo.WithNumber("timeout_ms", mcpgo.Description("Default element lookup timeout (10000).")),
			mcpgo.WithString("session", sessionProp()),
			mcpgo.WithNumber("window", windowProp()),
			mcpgo.WithTitleAnnotation("Run flow"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleRunFlow, ""},
	}
}

func (s *Server) handleRunFlow(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	yamlText := mcpgo.ParseString(req, "flow", "")
	file := mcpgo.ParseString(req, "file", "")
	env, err := stringMap(req, "env")
	if err != nil {
		return errResult(err)
	}
	var f *flow.Flow
	switch {
	case file != "":
		if f, err = flow.ParseFile(file); err != nil {
			return errResult(fmt.Errorf("%s: %w", file, err))
		}
	case strings.TrimSpace(yamlText) != "":
		if f, err = flow.Parse([]byte(yamlText), ""); err != nil {
			return errResult(err)
		}
	default:
		return errorf("give flow (YAML) or file")
	}
	timeout := lookupTimeout(req)
	return s.withPageWithin(ctx, req, 0, func(sess *desktop.Session, w driver.Window) (*mcpgo.CallToolResult, error) {
		outDir := filepath.Join(sess.Dir, "flows", time.Now().Format("150405"))
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			return errResult(err)
		}
		r := &flow.Runner{Window: w, OutDir: outDir, Env: env, LookupTimeout: timeout}
		if main := sess.Main(); main != nil {
			r.Main = func(ctx context.Context, expr string) (string, error) { return main.Eval(ctx, expr, 8*time.Second) }
		}
		res := r.Run(ctx, f)
		name := f.Name
		if name == "" && file != "" {
			name = filepath.Base(file)
		}
		head := ""
		if name != "" {
			head = name + ": "
		}
		out := head + res.Summary()
		if res.Passed {
			return text(out)
		}
		return mcpgo.NewToolResultError(out), nil
	})
}
