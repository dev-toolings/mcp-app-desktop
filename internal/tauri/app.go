package tauri

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
)

//go:embed agent.js
var agentSource string

// agentVersion changes with the agent source, so a server with a newer agent
// reinstalls it over an older one left in the page.
var agentVersion = func() string {
	sum := sha256.Sum256([]byte(agentSource))
	return hex.EncodeToString(sum[:6])
}()

var agentScript = strings.Replace(agentSource, "__AGENT_VERSION__", agentVersion, 1) + "\n;\"installed\""

// webviewInfo is one entry of the bridge's windows command.
type webviewInfo struct {
	Label   string  `json:"label"`
	URL     string  `json:"url"`
	Title   string  `json:"title"`
	Visible bool    `json:"visible"`
	Focused bool    `json:"focused"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	Scale   float64 `json:"scale_factor"`
}

// App is a driver.App over one bridge connection.
type App struct {
	c *Client
	// Dialogs returns the session's JavaScript dialog policy.
	Dialogs func() (accept bool, text string)

	mu      sync.Mutex
	indices map[string]int // label → index, stable while the connection lives
	next    int
}

// NewApp wraps a connected client.
func NewApp(c *Client, dialogs func() (bool, string)) *App {
	return &App{c: c, Dialogs: dialogs, indices: map[string]int{}}
}

// Client returns the bridge connection.
func (a *App) Client() *Client { return a.c }

func (a *App) Backend() string { return driver.BackendBridge }

func (a *App) webviews(ctx context.Context) ([]webviewInfo, error) {
	raw, err := a.c.Call(ctx, "windows", nil)
	if err != nil {
		return nil, fmt.Errorf("list windows: %w", err)
	}
	var list []webviewInfo
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("list windows: %w", err)
	}
	return list, nil
}

// indexOf assigns indices in order of first sight: 0 is the first window
// seen (the bridge lists "main" first), and an index stays with its window
// while it exists.
func (a *App) indexOf(label string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i, ok := a.indices[label]; ok {
		return i
	}
	i := a.next
	a.indices[label] = i
	a.next++
	return i
}

func (a *App) Windows(ctx context.Context) ([]driver.WindowInfo, error) {
	list, err := a.webviews(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]driver.WindowInfo, 0, len(list))
	for _, w := range list {
		out = append(out, driver.WindowInfo{Index: a.indexOf(w.Label), Type: "page", Title: w.Title, URL: w.URL, Label: w.Label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func (a *App) Window(ctx context.Context, index int) (driver.Window, error) {
	wins, err := a.Windows(ctx)
	if err != nil {
		return nil, err
	}
	open := make([]string, 0, len(wins))
	for _, w := range wins {
		if w.Index == index {
			return &Window{app: a, label: w.Label, index: index}, nil
		}
		open = append(open, fmt.Sprintf("%d (%s)", w.Index, w.Label))
	}
	if len(open) == 0 {
		return nil, fmt.Errorf("window %d does not exist: the app has no window open", index)
	}
	return nil, fmt.Errorf("window %d does not exist (open: %s)", index, strings.Join(open, ", "))
}

// Labels lists the open webview labels.
func (a *App) Labels(ctx context.Context) ([]string, error) {
	list, err := a.webviews(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.Label)
	}
	return out, nil
}

// errNoResult is an eval the webview did not run: the script threw at top
// level or did not parse, or the page was being replaced.
var errNoResult = errors.New("the webview returned no result")

// evalRaw runs script in a webview and returns the string it evaluated to.
// Scripts sent here must always evaluate to a string: anything else may not
// survive the native bridge (a Date makes WKWebView's JSON conversion abort
// the app).
func (a *App) evalRaw(ctx context.Context, label, script string, timeout time.Duration) (string, error) {
	if a.GPUI() {
		// Never sent: the bridge would answer an unknown command anyway.
		return "", fmt.Errorf("eval: %w", a.noJavaScript(ctx))
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < timeout {
			timeout = max(left, 50*time.Millisecond)
		}
	}
	raw, err := a.c.Call(ctx, "eval", map[string]any{"label": label, "script": script, "timeout_ms": timeout.Milliseconds()})
	if err != nil {
		return "", err
	}
	var res struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("eval: %w", err)
	}
	if res.Value == "" {
		return "", errNoResult
	}
	var s string
	if err := json.Unmarshal([]byte(res.Value), &s); err != nil {
		return "", fmt.Errorf("eval: the script returned %.200s instead of a string", res.Value)
	}
	return s, nil
}

// agentError is an error reported by the page agent, with its code:
// notfound, hidden, covered, disabled (retryable while waiting), stale,
// ambiguous, invalid, exception, error.
type agentError struct {
	Code string
	Msg  string
}

func (e *agentError) Error() string { return e.Msg }

func retryable(err error) bool {
	var ae *agentError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "notfound", "hidden", "covered", "disabled", "moving":
			return true
		}
		return false
	}
	// The page may be between two documents (navigation, dev reload).
	return errors.Is(err, errNoResult)
}

const installMarker = `{"__install":true}`

// GPUI reports whether the app is a GPUI one (see the package comment).
func (a *App) GPUI() bool { return a.c.GPUI() }

// kind names the bridge in messages: Tauri or GPUI.
func (a *App) kind() string {
	if a.GPUI() {
		return "GPUI"
	}
	return "Tauri"
}

// decodeAgent turns an agent answer into out, or into an *agentError when
// it is the agent's failure envelope {"error", "code"}.
func decodeAgent(op string, res []byte, out any) error {
	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(res, &envelope); err == nil && envelope.Error != "" {
		return &agentError{Code: envelope.Code, Msg: envelope.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res, out); err != nil {
		return fmt.Errorf("%s: unexpected answer %.200s: %w", op, res, err)
	}
	return nil
}

// runNative calls one agent operation through a GPUI bridge, which runs it
// natively: nothing to install, no script.
func (a *App) runNative(ctx context.Context, label, op string, args any, out any) error {
	raw, err := a.c.Call(ctx, "agent", map[string]any{"label": label, "op": op, "args": args})
	if err != nil {
		return err
	}
	return decodeAgent(op, raw, out)
}

// run calls one agent operation in a webview, installing the agent first
// when the document has none (a new page, a reload, an older agent). A GPUI
// app runs the operation natively.
func (a *App) run(ctx context.Context, label, op string, args any, out any) error {
	if a.GPUI() {
		return a.runNative(ctx, label, op, args, out)
	}
	argJSON, err := json.Marshal(args)
	if err != nil {
		return err
	}
	opJSON, _ := json.Marshal(op)
	accept, text := true, ""
	if a.Dialogs != nil {
		accept, text = a.Dialogs()
	}
	dialogs, _ := json.Marshal(map[string]any{"accept": accept, "text": text})
	script := `(function(){try{var A=window.__MCP_AD_AGENT__;if(!A||A.v!==` + jsString(agentVersion) + `)return '` + installMarker + `';A.dialogs=` + string(dialogs) + `;return JSON.stringify(A.run(` + string(opJSON) + `,` + string(argJSON) + `));}catch(e){return JSON.stringify({error:String(e&&e.message||e),code:'error'});}})()`
	for attempt := 0; attempt < 3; attempt++ {
		res, err := a.evalRaw(ctx, label, script, 0)
		if err != nil {
			return err
		}
		if res == installMarker {
			if err := a.install(ctx, label); err != nil {
				return err
			}
			continue
		}
		return decodeAgent(op, []byte(res), out)
	}
	return fmt.Errorf("%s: the page agent could not be installed in window %q (the page keeps reloading?)", op, label)
}

func (a *App) install(ctx context.Context, label string) error {
	res, err := a.evalRaw(ctx, label, agentScript, 15*time.Second)
	if err != nil {
		if errors.Is(err, errNoResult) {
			return fmt.Errorf("install the page agent in window %q: the webview did not run it (page loading?): %w", label, err)
		}
		return fmt.Errorf("install the page agent in window %q: %w", label, err)
	}
	if res != "installed" {
		return fmt.Errorf("install the page agent in window %q: unexpected answer %.100q", label, res)
	}
	return nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// WaitFirstPaint gives the first window a bounded moment to mount its UI,
// so the first snapshot is not an empty document: it returns as soon as the
// window has text or an interactive element, or after d.
func (a *App) WaitFirstPaint(ctx context.Context, d time.Duration) {
	if a.GPUI() {
		a.waitFirstNodes(ctx, d)
		return
	}
	deadline := time.Now().Add(d)
	const probe = `(function(){var b=document.body;return String(document.readyState==="complete"&&!!b&&(b.innerText.trim().length>0||!!document.querySelector("button,a,input,textarea,select,[role],[contenteditable]")));})()`
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if labels, err := a.Labels(ctx); err == nil && len(labels) > 0 {
			if res, err := a.evalRaw(ctx, labels[0], probe, 2*time.Second); err == nil && res == "true" {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// gpuiTreeGrace bounds the wait for a GPUI window's first accessibility
// node once the window exists: GPUI builds the tree on its first frame, so a
// window still empty after that exposes no node at all (elements without
// .id() and .role()), and waiting longer would only delay the launch.
const gpuiTreeGrace = 3 * time.Second

// waitFirstNodes is WaitFirstPaint for a GPUI app: it waits for the first
// window (at most d), then for its tree to have a node (at most
// gpuiTreeGrace more).
func (a *App) waitFirstNodes(ctx context.Context, d time.Duration) {
	deadline := time.Now().Add(d)
	var seen time.Time
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if labels, err := a.Labels(ctx); err == nil && len(labels) > 0 {
			if seen.IsZero() {
				seen = time.Now()
			}
			var ans struct {
				Nodes []json.RawMessage `json:"nodes"`
			}
			if err := a.runNative(ctx, labels[0], "snapshot", map[string]any{"level": "content"}, &ans); err == nil && len(ans.Nodes) > 0 {
				return
			}
			if time.Since(seen) > gpuiTreeGrace {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// Commands lists the commands a GPUI app registered for invoke
// (gpui_mcp_app_desktop::register).
func (a *App) Commands(ctx context.Context) ([]string, error) {
	if !a.GPUI() {
		return nil, fmt.Errorf("commands: only a GPUI bridge lists its commands")
	}
	raw, err := a.c.Call(ctx, "commands", nil)
	if err != nil {
		return nil, fmt.Errorf("list the app's commands: %w", err)
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("list the app's commands: %w", err)
	}
	return names, nil
}

// noJavaScript is the error of anything needing JavaScript in a GPUI app. It
// names the commands the app registered, the stand-in for eval.
func (a *App) noJavaScript(ctx context.Context) error {
	registered := "none registered yet"
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if names, err := a.Commands(lctx); err != nil {
		registered = "could not list them: " + err.Error()
	} else if len(names) > 0 {
		registered = "registered: " + strings.Join(names, ", ")
	}
	return fmt.Errorf("no JavaScript in a GPUI app (%s): use invoke with a command the app registers with gpui_mcp_app_desktop::register(cx, \"name\", |args, window, cx| ...) (%s; invoke with an unknown command lists them too); read and drive the UI with snapshot, click, type, press", a.c.Hello.AppName, registered)
}

// Invoke calls a command a GPUI app registered, in window label (the first
// one when empty), and returns its JSON answer.
func (a *App) Invoke(ctx context.Context, label, command string, args any) (json.RawMessage, error) {
	if !a.GPUI() {
		return nil, fmt.Errorf("invoke %s: a Tauri command runs through the page (InvokeScript), not the bridge", command)
	}
	if args == nil {
		args = map[string]any{}
	}
	return a.c.Call(ctx, "invoke", map[string]any{"label": label, "command": command, "args": args})
}
