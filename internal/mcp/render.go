package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func errResult(err error) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError(err.Error()), nil
}

func errorf(format string, args ...any) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError(fmt.Sprintf(format, args...)), nil
}

func text(s string) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText(s), nil
}

func textf(format string, args ...any) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText(fmt.Sprintf(format, args...)), nil
}

// jsonResult emits a one-line summary followed by compact JSON.
func jsonResult(payload any, summary string) (*mcpgo.CallToolResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
	}
	if summary == "" {
		return mcpgo.NewToolResultText(string(data)), nil
	}
	return mcpgo.NewToolResultText(summary + "\n" + string(data)), nil
}

// agentProfile renders accessibility trees in the compact agent format
// ("@3 b Save", one line per node) that keeps snapshots small.
var agentProfile = engine.RenderProfile{Agent: true, Format: "text", MaxLabelLen: 100, AbbrevRoles: true, DropEmptyStats: true}

// renderTree renders an extraction as text; an empty tree says so instead of
// returning nothing.
func renderTree(result *engine.ExtractionResult) string {
	if result == nil {
		return "(no accessibility tree)"
	}
	out := strings.TrimRight(engine.FormatTextProfile(result, agentProfile), "\n")
	if strings.TrimSpace(out) == "" {
		return "(empty page: no named or interactive element)"
	}
	return out
}

// legend is appended once to a full snapshot so the abbreviations are known.
const legend = "tags: b button, a link, t textbox, c checkbox, r radio, s combobox, m menuitem, x tab, hN heading, txt text, li item, img image; ✓/✗ checked, off disabled; @N = ref for click/type/…"

// jsonUnmarshalLoose decodes JSON text.
func jsonUnmarshalLoose(raw string, out any) error { return json.Unmarshal([]byte(raw), out) }
