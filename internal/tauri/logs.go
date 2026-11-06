package tauri

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// LogEntry is one record of the plugin's init script: a console message,
// an uncaught error, a Tauri IPC call, or a dialog the agent answered.
type LogEntry struct {
	Time     time.Time
	Source   string // console, exception, ipc, page
	Level    string
	Text     string
	Location string
	Label    string
	Window   int
}

type cursor struct {
	boot string
	seq  int64
}

// LogPoller drains the init script's buffer of every webview. It keeps one
// cursor per webview label, so a document replaced by a navigation or a
// reload (a new boot id) is read from its start.
type LogPoller struct {
	mu      sync.Mutex
	cursors map[string]cursor
}

// NewLogPoller returns an empty poller.
func NewLogPoller() *LogPoller { return &LogPoller{cursors: map[string]cursor{}} }

type drained struct {
	Boot    string `json:"boot"`
	Seq     int64  `json:"seq"`
	Entries []struct {
		Seq      int64  `json:"seq"`
		TS       int64  `json:"ts"`
		Source   string `json:"source"`
		Level    string `json:"level"`
		Text     string `json:"text"`
		Location string `json:"location"`
	} `json:"entries"`
}

// Poll returns the entries logged since the previous poll in every window.
func (p *LogPoller) Poll(ctx context.Context, a *App) []LogEntry {
	wins, err := a.Windows(ctx)
	if err != nil {
		return nil
	}
	var out []LogEntry
	for _, w := range wins {
		p.mu.Lock()
		cur := p.cursors[w.Label]
		p.mu.Unlock()
		script := `(function(){var s=window.__MCP_APP_DESKTOP__;return s&&s.drain?s.drain(` + jsString(cur.boot) + `,` + jsonInt(cur.seq) + `):'{"missing":true}';})()`
		res, err := a.evalRaw(ctx, w.Label, script, 3*time.Second)
		if err != nil {
			continue
		}
		var d drained
		if json.Unmarshal([]byte(res), &d) != nil || d.Boot == "" {
			continue
		}
		for _, e := range d.Entries {
			out = append(out, LogEntry{Time: time.UnixMilli(e.TS), Source: e.Source, Level: e.Level, Text: e.Text, Location: e.Location, Label: w.Label, Window: w.Index})
		}
		p.mu.Lock()
		p.cursors[w.Label] = cursor{boot: d.Boot, seq: d.Seq}
		p.mu.Unlock()
	}
	return out
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
