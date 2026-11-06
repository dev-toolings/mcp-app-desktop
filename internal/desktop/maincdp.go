package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/cdp"
)

// MainClient talks to the Electron main process through its Node inspector
// (--inspect). It speaks the same protocol as the renderer (CDP), but there
// is a single Node context: Runtime.evaluate is all that is needed to read
// BrowserWindows, ipcMain channels, app paths, or to stub a dialog.
type MainClient struct {
	ws       string
	client   *cdp.Client
	mu       sync.Mutex
	closed   bool
	closeFn  func()
	consoles []LogEntry
}

// mainPrelude binds the Electron modules an expression most often needs. The
// wrapper receives the inspector's command-line `require` as __cliRequire;
// the other sources cover an app whose main module exposes its own. A
// missing electron module (plain Node, tests) leaves the bindings undefined.
const mainPrelude = `const require = globalThis.require ?? (typeof module !== 'undefined' && module.require ? module.require.bind(module) : (typeof process !== 'undefined' && process.mainModule && process.mainModule.require ? process.mainModule.require.bind(process.mainModule) : __cliRequire));
let electron = {};
try { if (require) electron = require('electron'); } catch (__e) {}
const {app, BrowserWindow, webContents, ipcMain, dialog, session, Menu, shell, nativeTheme, screen, clipboard, powerMonitor} = electron;
const __safe = (v) => { const seen = new WeakSet(); return JSON.stringify(v, (k, x) => { if (typeof x === 'bigint') return x.toString() + 'n'; if (typeof x === 'function') return '[Function ' + (x.name || 'anonymous') + ']'; if (x && typeof x === 'object') { if (seen.has(x)) return '[Circular]'; seen.add(x); if (x instanceof Map) return Object.fromEntries(x); if (x instanceof Set) return [...x]; if (x instanceof Error) return {name: x.name, message: x.message, stack: x.stack}; } return x; }); };`

// mainEvalPlan decides, with the engine's own parser and without running
// anything, how the source is evaluated:
//
//   - a valid script (expression, IIFE, statements ending with an
//     expression) runs through a direct eval, whose completion value is the
//     value of the last expression statement, like the DevTools console;
//   - otherwise, a single expression using await is returned as is;
//   - otherwise the source is an async function body (top-level return or
//     await): when it does not end with a return, its last expression
//     statement is turned into one, provided the head and the tail parse on
//     their own and the cut is neither inside a line comment nor before a
//     line that continues the previous expression;
//   - a source that parses in none of these forms reports its syntax error.
const mainEvalPlan = `const __AsyncFunction = (async function () {}).constructor;
const __asyncParses = (body) => { try { new __AsyncFunction(body); return true; } catch (__e) { return false; } };
const __isScript = (code) => {
  if (require) {
    let vm = null;
    try { vm = require('vm'); } catch (__e) {}
    if (vm && vm.Script) {
      try { new vm.Script(code, { filename: 'eval_main' }); return true; } catch (__e) { return false; }
    }
  }
  try { new Function('(class { static {\n' + code + '\n} })'); return true; } catch (__e) { return false; }
};
const __plan = (() => {
  const trimmed = __src.replace(/[\s;]+$/, '');
  if (/^\s*\{/.test(trimmed) && __isScript('(' + trimmed + '\n)')) return { mode: 'script', src: '(' + trimmed + '\n)' };
  if (__isScript(__src)) return { mode: 'script', src: __src };
  const asExpr = 'return (\n' + trimmed + '\n);';
  if (__asyncParses(asExpr)) return { mode: 'body', body: asExpr };
  try { new __AsyncFunction(__src); } catch (__e) { return { mode: 'error', error: __e }; }
  const hazard = /^\s*[(\[\x60+\-*/%,.?:<>=&|^]/;
  let tries = 0;
  for (let i = trimmed.length - 1; i > 0 && tries < 64; i--) {
    const c = trimmed[i];
    if (c !== ';' && c !== '\n') continue;
    tries++;
    const head = trimmed.slice(0, c === ';' ? i + 1 : i);
    const tail = trimmed.slice(i + 1);
    if (!tail.trim()) continue;
    if (c === '\n' && hazard.test(tail)) continue;
    if (!__asyncParses(head) || __asyncParses(head + ')')) continue;
    const body = head + '\nreturn (\n' + tail + '\n);';
    if (__asyncParses(body)) return { mode: 'body', body };
  }
  return { mode: 'body', body: __src };
})();`

// mainEvalRun evaluates the plan in the wrapper's scope (a direct eval sees
// the prelude bindings), awaits a promise or thenable within __timeoutMs, and
// serialises the value: a string as is, undefined as "undefined", anything
// else as JSON.
const mainEvalRun = `if (__plan.mode === 'error') throw __plan.error;
let __v = __plan.mode === 'script' ? eval(__plan.src) : eval('(async () => {\n' + __plan.body + '\n})()');
if (__v !== null && (typeof __v === 'object' || typeof __v === 'function') && typeof __v.then === 'function') {
  let __timer;
  try {
    __v = await Promise.race([__v, new Promise((_, reject) => {
      __timer = setTimeout(() => reject(new Error('the promise did not settle within ' + __timeoutMs + ' ms (timeout_ms)')), __timeoutMs);
      if (__timer && typeof __timer.unref === 'function') __timer.unref();
    })]);
  } finally { clearTimeout(__timer); }
}
if (typeof __v === 'string') return __v;
if (__v === undefined) return 'undefined';
const __out = __safe(__v);
return __out === undefined ? String(__v) : __out;`

// mainEvalScript builds the expression sent to Runtime.evaluate for src.
func mainEvalScript(src string, timeout time.Duration) string {
	lit, _ := json.Marshal(src)
	return "(async (__cliRequire) => {\n" + mainPrelude + "\nconst __src = " + string(lit) + ";\nconst __timeoutMs = " + strconv.FormatInt(timeout.Milliseconds(), 10) + ";\n" + mainEvalPlan + "\n" + mainEvalRun + "\n})(typeof require === 'function' ? require : undefined)"
}

// ConnectMain attaches to a Node inspector endpoint (http://127.0.0.1:PORT).
func ConnectMain(ctx context.Context, endpoint string) (*MainClient, error) {
	ws := probeInspector(ctx, endpoint)
	if ws == "" {
		return nil, fmt.Errorf("no Node inspector answers at %s", endpoint)
	}
	dialer := &recordingDialer{}
	sock := &cdp.WebSocket{Dialer: dialer}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sock.Connect(dctx, ws, http.Header{}); err != nil {
		return nil, fmt.Errorf("inspector %s: %w", endpoint, err)
	}
	client := cdp.New().Start(sock)
	m := &MainClient{ws: ws, client: client, closeFn: dialer.closeAll}
	go m.drain()
	// Runtime.enable makes the inspector report console calls and exceptions
	// of the main process; both are kept for the logs tool.
	ectx, ecancel := context.WithTimeout(ctx, 3*time.Second)
	defer ecancel()
	_, _ = client.Call(ectx, "", "Runtime.enable", map[string]any{})
	return m, nil
}

func (m *MainClient) drain() {
	for ev := range m.client.Event() {
		switch ev.Method {
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
				Args []struct {
					Value       json.RawMessage `json:"value"`
					Description string          `json:"description"`
				} `json:"args"`
				Timestamp float64 `json:"timestamp"`
			}
			if json.Unmarshal(ev.Params, &p) != nil {
				continue
			}
			parts := make([]string, 0, len(p.Args))
			for _, a := range p.Args {
				if len(a.Value) > 0 && string(a.Value) != "null" {
					var s string
					if json.Unmarshal(a.Value, &s) == nil {
						parts = append(parts, s)
					} else {
						parts = append(parts, string(a.Value))
					}
				} else if a.Description != "" {
					parts = append(parts, a.Description)
				}
			}
			m.add(LogEntry{Time: cdpTime(p.Timestamp), Source: "main", Level: consoleLevel(p.Type), Text: strings.Join(parts, " "), Window: -1})
		case "Runtime.exceptionThrown":
			var p struct {
				Timestamp float64 `json:"timestamp"`
				Details   struct {
					Text      string `json:"text"`
					Exception *struct {
						Description string `json:"description"`
					} `json:"exception"`
					URL        string `json:"url"`
					LineNumber int    `json:"lineNumber"`
				} `json:"exceptionDetails"`
			}
			if json.Unmarshal(ev.Params, &p) != nil {
				continue
			}
			text := p.Details.Text
			if p.Details.Exception != nil && p.Details.Exception.Description != "" {
				text = p.Details.Exception.Description
			}
			loc := ""
			if p.Details.URL != "" {
				loc = fmt.Sprintf("%s:%d", p.Details.URL, p.Details.LineNumber+1)
			}
			m.add(LogEntry{Time: cdpTime(p.Timestamp), Source: "main-exception", Level: "error", Text: text, Location: loc, Window: -1})
		}
	}
}

func cdpTime(ms float64) time.Time {
	if ms <= 0 {
		return time.Now()
	}
	return time.UnixMilli(int64(ms))
}

func consoleLevel(typ string) string {
	switch typ {
	case "error", "assert":
		return "error"
	case "warning":
		return "warning"
	case "info", "log", "dir", "dirxml", "table", "trace", "count", "timeEnd":
		return "info"
	case "debug":
		return "debug"
	}
	return typ
}

const maxMainLogs = 1000

func (m *MainClient) add(e LogEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consoles = append(m.consoles, e)
	if len(m.consoles) > maxMainLogs {
		m.consoles = append(m.consoles[:0:0], m.consoles[len(m.consoles)-maxMainLogs:]...)
	}
}

// Logs returns the console messages and exceptions seen in the main process.
func (m *MainClient) Logs() []LogEntry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]LogEntry(nil), m.consoles...)
}

// Eval runs JavaScript in the main process with the Electron modules in
// scope and returns the JSON-serialised value of its last expression (see
// mainEvalPlan): bare expressions, IIFEs, statement lists, and bodies with a
// return or a top-level await all work. A promise is awaited for at most
// timeout.
func (m *MainClient) Eval(ctx context.Context, expr string, timeout time.Duration) (string, error) {
	if m == nil {
		return "", fmt.Errorf("the main process inspector is not connected (launch with inspect: true, or attach with inspector_endpoint)")
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if strings.TrimSpace(expr) == "" {
		return "", fmt.Errorf("expression is required")
	}
	// The inspector answers once the promise settles or the script's own
	// timer fires; the extra margin only covers a main thread too busy to
	// run that timer.
	cctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	start := time.Now()
	raw, err := m.client.Call(cctx, "", "Runtime.evaluate", map[string]any{
		"expression":            mainEvalScript(expr, timeout),
		"awaitPromise":          true,
		"returnByValue":         true,
		"includeCommandLineAPI": true,
		"timeout":               timeout.Milliseconds(),
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return "", fmt.Errorf("main eval: no answer from the main process within %s (timeout_ms): it may be blocked by synchronous work or paused at a breakpoint; raise timeout_ms (max 120000) or check status and logs", timeout)
		}
		// V8 terminates synchronous code that outlives the evaluate timeout
		// and reports it as a bare internal error.
		var cdpErr *cdp.Error
		if errors.As(err, &cdpErr) && cdpErr.Code == -32603 && time.Since(start) >= timeout*9/10 {
			return "", fmt.Errorf("main eval: synchronous code was terminated after %s (timeout_ms); the main process is responsive again", timeout)
		}
		return "", fmt.Errorf("main eval: %w", err)
	}
	var res struct {
		Result struct {
			Type        string          `json:"type"`
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("main eval: decode: %w", err)
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			msg = res.ExceptionDetails.Exception.Description
		}
		return "", fmt.Errorf("main eval threw: %s", msg)
	}
	var s string
	if json.Unmarshal(res.Result.Value, &s) == nil {
		return s, nil
	}
	if len(res.Result.Value) > 0 {
		return string(res.Result.Value), nil
	}
	return res.Result.Description, nil
}

// Close drops the inspector connection; the inspector itself stays open in
// the application until it exits.
func (m *MainClient) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	if m.closeFn != nil {
		m.closeFn()
	}
}
