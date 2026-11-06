package desktop

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// LogEntry is one line of diagnostics: a renderer console message, an
// uncaught exception, a failed network request, or a line of the process's
// stdout/stderr (the main process's own console).
type LogEntry struct {
	Time   time.Time `json:"time"`
	Source string    `json:"source"` // console, exception, network, page, stdout, stderr, server
	Level  string    `json:"level,omitempty"`
	Text   string    `json:"text"`
	// Location is file:line for console messages and exceptions.
	Location string `json:"location,omitempty"`
	// Window is the window index the entry came from (-1 for process output).
	Window int `json:"window"`
}

// IsError reports whether the entry is worth an agent's attention.
func (e LogEntry) IsError() bool {
	switch e.Source {
	case "exception", "stderr":
		return true
	}
	switch strings.ToLower(e.Level) {
	case "error", "warning", "warn", "fatal":
		return true
	}
	return false
}

// maxLine caps one retained output line; longer lines are cut.
const maxLine = 4 << 10

// Ring keeps the last lines of a process's output.
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	max     int
	dropped int
}

// NewRing keeps at most max lines.
func NewRing(max int) *Ring { return &Ring{max: max} }

// Writer returns a line-splitting writer tagging lines with source.
func (r *Ring) Writer(source string) io.Writer { return &lineWriter{r: r, source: source} }

// Add appends one line.
func (r *Ring) Add(source, text string) {
	if len(text) > maxLine {
		text = text[:maxLine] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, LogEntry{Time: time.Now(), Source: source, Text: text, Window: -1})
	if len(r.entries) > r.max {
		n := len(r.entries) - r.max
		r.dropped += n
		r.entries = append(r.entries[:0:0], r.entries[n:]...)
	}
}

// Entries returns a copy of the retained lines.
func (r *Ring) Entries() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LogEntry(nil), r.entries...)
}

// Tail returns the last n lines joined, for error messages.
func (r *Ring) Tail(n int) string {
	e := r.Entries()
	if len(e) > n {
		e = e[len(e)-n:]
	}
	lines := make([]string, len(e))
	for i, x := range e {
		lines[i] = x.Text
	}
	if len(lines) == 0 {
		return "(no output)"
	}
	return strings.Join(lines, "\n")
}

// Find returns the text following prefix on the most recent matching line.
func (r *Ring) Find(prefix string) string {
	e := r.Entries()
	for i := len(e) - 1; i >= 0; i-- {
		if _, after, ok := strings.Cut(e[i].Text, prefix); ok && strings.TrimSpace(after) != "" {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

type lineWriter struct {
	r      *Ring
	source string
	mu     sync.Mutex
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.r.Add(w.source, strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLine {
		w.r.Add(w.source, string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}
