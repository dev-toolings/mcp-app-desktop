package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) traceTools() []toolDef {
	return []toolDef{
		{mcpgo.NewTool("trace",
			mcpgo.WithDescription("mcp-app-desktop's own trace, to diagnose and fix the server: one JSON line per call start and end (arguments, duration, error, result head), host cancellations (an interrupted call), and a dump of the server's goroutines when a call ran past 20 s (\"stuck\": where it blocks). Oldest first. Also written to the file named in the answer."),
			mcpgo.WithNumber("last", mcpgo.Description("Lines to return (default 40)."), mcpgo.DefaultNumber(40)),
			mcpgo.WithString("tool", mcpgo.Description("Only this tool's calls.")),
			mcpgo.WithBoolean("problems", mcpgo.Description("Only failed, cancelled and stuck calls (with their start line)."), mcpgo.DefaultBool(false)),
			mcpgo.WithNumber("since_seconds", mcpgo.Description("Only lines newer than this many seconds.")),
			mcpgo.WithBoolean("with_stacks", mcpgo.Description("Include the goroutine dumps of stuck calls (long)."), mcpgo.DefaultBool(false)),
			mcpgo.WithBoolean("clear", mcpgo.Description("Delete the trace once the problem is understood or fixed (it is also bounded to 4 MiB and removed after a day)."), mcpgo.DefaultBool(false)),
			mcpgo.WithTitleAnnotation("Read the server trace"), mcpgo.WithOpenWorldHintAnnotation(false),
		), (*Server).handleTrace, ""},
	}
}

func (s *Server) handleTrace(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if mcpgo.ParseBoolean(req, "clear", false) {
		if err := s.trace.Clear(); err != nil {
			return errResult(err)
		}
		return mcpgo.NewToolResultText("trace cleared: " + s.trace.Path()), nil
	}
	q := traceQuery{
		last:       min(max(mcpgo.ParseInt(req, "last", 40), 1), 500),
		tool:       strings.TrimSpace(mcpgo.ParseString(req, "tool", "")),
		problems:   mcpgo.ParseBoolean(req, "problems", false),
		since:      time.Duration(mcpgo.ParseFloat64(req, "since_seconds", 0) * float64(time.Second)),
		withStacks: mcpgo.ParseBoolean(req, "with_stacks", false),
	}
	recs, err := s.trace.Read(q)
	if err != nil {
		return errResult(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "trace %s · %d line(s)\n", s.trace.Path(), len(recs))
	for _, rec := range recs {
		stacks, _ := rec["goroutines"].(string)
		if q.withStacks && stacks != "" {
			delete(rec, "goroutines")
		}
		line, _ := json.Marshal(rec)
		b.Write(line)
		b.WriteByte('\n')
		if q.withStacks && stacks != "" {
			b.WriteString(stacks)
			b.WriteString("\n")
		}
	}
	return mcpgo.NewToolResultText(b.String()), nil
}
