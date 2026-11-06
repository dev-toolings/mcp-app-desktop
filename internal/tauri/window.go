package tauri

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
)

// Window is one Tauri webview driven through the bridge.
type Window struct {
	app   *App
	label string
	index int
}

// refTables keeps the last snapshot's refs per webview label for the
// lifetime of the process (diffs compare against them).
var refTables sync.Map // label → map[string]engine.RefSnapshot

func refKey(a *App, label string) string { return a.c.Addr() + "|" + label }

func (w *Window) Index() int { return w.index }

// Label is the Tauri webview label.
func (w *Window) Label() string { return w.label }

type pageInfo struct {
	URL     string  `json:"url"`
	Title   string  `json:"title"`
	Ready   string  `json:"ready"`
	Boot    string  `json:"boot"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	DPR     float64 `json:"dpr"`
	Scroll  float64 `json:"scroll"`
	Mutated int64   `json:"mutated"`
	Now     int64   `json:"now"`
}

func (w *Window) info(ctx context.Context) (pageInfo, error) {
	var pi pageInfo
	err := w.app.run(ctx, w.label, "info", map[string]any{}, &pi)
	return pi, err
}

func (w *Window) Info(ctx context.Context) (string, string, error) {
	pi, err := w.info(ctx)
	return pi.URL, pi.Title, err
}

// loc is a target in the agent's terms.
type loc struct {
	Kind    string   `json:"kind"`
	Ref     string   `json:"ref,omitempty"`
	CSS     string   `json:"css,omitempty"`
	Role    string   `json:"role,omitempty"`
	Name    string   `json:"name,omitempty"`
	Text    string   `json:"text,omitempty"`
	Label   string   `json:"label,omitempty"`
	Pattern *pattern `json:"pattern,omitempty"`
	Exact   bool     `json:"exact,omitempty"`
	Desc    string   `json:"desc,omitempty"`
}

type pattern struct {
	Source string `json:"source"`
	Flags  string `json:"flags"`
}

// parseTarget turns the shared target syntax into an agent locator.
func parseTarget(target string) (*loc, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return nil, fmt.Errorf("target is required (@ref from snapshot, a CSS selector, or getByRole/getByText/getByLabel)")
	}
	if r := engine.InternalRef(t); strings.HasPrefix(r, "@") {
		return &loc{Kind: "ref", Ref: r}, nil
	}
	if err := engine.ChainedTargetError(t); err != nil {
		return nil, err
	}
	l, isLocator, err := engine.ParseLocator(t)
	if isLocator {
		if err != nil {
			return nil, err
		}
		out := &loc{Exact: l.Exact, Desc: t}
		if l.Pattern != nil {
			out.Pattern = &pattern{Source: l.Pattern.Source, Flags: l.Pattern.Flags}
		}
		switch {
		case l.Text != "":
			out.Kind, out.Text = "text", l.Text
		case l.Label != "":
			out.Kind, out.Label = "label", l.Label
		default:
			out.Kind, out.Role, out.Name = "role", engine.CanonicalRole(l.Role), l.Name
		}
		return out, nil
	}
	return &loc{Kind: "css", CSS: t}, nil
}

// retry runs fn until it succeeds, fails for good, or timeout passes.
func retry(ctx context.Context, timeout time.Duration, what string, fn func() error) error {
	deadline := time.Now().Add(timeout)
	interval := 50 * time.Millisecond
	for {
		err := fn()
		if err == nil || !retryable(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: %w (waited %s)", what, err, timeout.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w (%v)", what, err, ctx.Err())
		case <-time.After(interval):
		}
		interval = min(interval*2, 250*time.Millisecond)
	}
}

func defaultTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}

// ---------------------------------------------------------------- snapshot

type snapshotAnswer struct {
	Nodes    []engine.ExtractedNode `json:"nodes"`
	Stats    engine.ExtractionStats `json:"stats"`
	Warnings []string               `json:"warnings"`
}

func collectRefs(nodes []engine.ExtractedNode, refs map[string]engine.ExtractedNode) {
	for _, n := range nodes {
		if n.Ref != "" {
			refs[n.Ref] = n
		}
		collectRefs(n.Children, refs)
	}
}

func (w *Window) Snapshot(ctx context.Context, level engine.ExtractLevel, selector string) (*engine.ExtractionResult, error) {
	if err := engine.ValidateExtractLevel(level); err != nil {
		return nil, err
	}
	var ans snapshotAnswer
	err := retry(ctx, 5*time.Second, "snapshot", func() error {
		return w.app.run(ctx, w.label, "snapshot", map[string]any{"level": string(level), "selector": selector}, &ans)
	})
	if err != nil {
		return nil, err
	}
	result := &engine.ExtractionResult{Nodes: ans.Nodes, Refs: map[string]engine.ExtractedNode{}, Stats: ans.Stats, Warnings: ans.Warnings}
	collectRefs(result.Nodes, result.Refs)
	refTables.Store(refKey(w.app, w.label), engine.RefSnapshotsOf(result))
	return result, nil
}

func (w *Window) Refs() map[string]engine.RefSnapshot {
	if v, ok := refTables.Load(refKey(w.app, w.label)); ok {
		return v.(map[string]engine.RefSnapshot)
	}
	return nil
}

func (w *Window) Mutation(ctx context.Context, prev map[string]engine.RefSnapshot) (engine.SnapshotDiff, *engine.ExtractionResult, error) {
	result, err := w.Snapshot(ctx, engine.LevelSkeleton, "")
	if err != nil {
		return engine.SnapshotDiff{Unchanged: true}, nil, err
	}
	diff := engine.DiffRefs(prev, w.Refs())
	if !diff.Unchanged {
		return diff, result, nil
	}
	// A framework may re-render a moment after the event: one bounded
	// second look, like the CDP backend.
	w.Settle(ctx, 250*time.Millisecond)
	if result, err = w.Snapshot(ctx, engine.LevelSkeleton, ""); err != nil {
		return engine.SnapshotDiff{Unchanged: true}, nil, err
	}
	return engine.DiffRefs(prev, w.Refs()), result, nil
}

// ---------------------------------------------------------------- actions

func (w *Window) act(ctx context.Context, timeout time.Duration, what, op string, args map[string]any, out any) error {
	return retry(ctx, defaultTimeout(timeout), what, func() error {
		return w.app.run(ctx, w.label, op, args, out)
	})
}

func (w *Window) Click(ctx context.Context, target string, o driver.ClickOptions) (string, error) {
	l, err := parseTarget(target)
	if err != nil {
		return "", err
	}
	button := strings.ToLower(o.Button)
	if button == "" {
		button = "left"
	}
	var res struct {
		Note string `json:"note"`
	}
	err = w.act(ctx, o.Timeout, target, "click", map[string]any{"loc": l, "button": button, "count": max(o.Count, 1), "reveal": o.Hover != driver.HoverNever}, &res)
	return res.Note, err
}

func (w *Window) ClickAt(ctx context.Context, x, y float64, count int) error {
	return w.app.run(ctx, w.label, "clickAt", map[string]any{"x": x, "y": y, "count": max(count, 1)}, nil)
}

func (w *Window) Type(ctx context.Context, target, text string, o driver.TypeOptions) error {
	args := map[string]any{"text": text, "append": o.Append, "submit": o.Submit}
	if target == "" {
		return w.app.run(ctx, w.label, "type", args, nil)
	}
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	args["loc"] = l
	return w.act(ctx, o.Timeout, target, "type", args, nil)
}

func (w *Window) Select(ctx context.Context, target string, values []string, timeout time.Duration) error {
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	return w.act(ctx, timeout, target, "select", map[string]any{"loc": l, "values": values}, nil)
}

func (w *Window) Press(ctx context.Context, target, combo string, timeout time.Duration) error {
	c, err := driver.ParseCombo(combo)
	if err != nil {
		return err
	}
	mods := c.Mods
	if mods == nil {
		mods = []string{}
	}
	args := map[string]any{"combo": map[string]any{"mods": mods, "key": c.Key}}
	if target == "" {
		return w.app.run(ctx, w.label, "press", args, nil)
	}
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	args["loc"] = l
	return w.act(ctx, timeout, target, "press", args, nil)
}

func (w *Window) Hover(ctx context.Context, target string, timeout time.Duration) error {
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	return w.act(ctx, timeout, target, "hover", map[string]any{"loc": l}, nil)
}

func (w *Window) Drag(ctx context.Context, from, to string, steps int, timeout time.Duration) error {
	lf, err := parseTarget(from)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	lt, err := parseTarget(to)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	return w.act(ctx, timeout, from+" → "+to, "drag", map[string]any{"from": lf, "to": lt, "steps": max(steps, 1)}, nil)
}

func (w *Window) ScrollIntoView(ctx context.Context, target string, timeout time.Duration) error {
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	return w.act(ctx, timeout, target, "scrollIntoView", map[string]any{"loc": l}, nil)
}

func (w *Window) ScrollBy(ctx context.Context, dx, dy int) (float64, error) {
	var res struct {
		Pos float64 `json:"pos"`
	}
	err := w.app.run(ctx, w.label, "scrollBy", map[string]any{"dx": dx, "dy": dy}, &res)
	return res.Pos, err
}

func (w *Window) Viewport(ctx context.Context) (float64, float64, error) {
	pi, err := w.info(ctx)
	return pi.W, pi.H, err
}

type point struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Rect struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	} `json:"rect"`
}

func (w *Window) Wheel(ctx context.Context, target string, x, y, dy float64, steps int, interval, timeout time.Duration) error {
	if target != "" {
		l, err := parseTarget(target)
		if err != nil {
			return err
		}
		var p point
		if err := w.act(ctx, timeout, target, "point", map[string]any{"loc": l}, &p); err != nil {
			return err
		}
		x, y = p.X, p.Y
	}
	steps = max(steps, 1)
	for i := 0; i < steps; i++ {
		if err := w.app.run(ctx, w.label, "wheel", map[string]any{"x": x, "y": y, "dy": dy / float64(steps)}, nil); err != nil {
			return err
		}
		if i < steps-1 && interval > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return nil
}

// maxUpload bounds the files sent through the bridge in one call.
const maxUpload = 32 << 20

func (w *Window) Upload(ctx context.Context, target string, files []string, timeout time.Duration) error {
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	var payload []map[string]string
	total := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		total += len(data)
		if total > maxUpload {
			return fmt.Errorf("files over %d MiB: the Tauri bridge sends them through the page; upload fewer or smaller files", maxUpload>>20)
		}
		payload = append(payload, map[string]string{"name": filepath.Base(f), "type": mime.TypeByExtension(filepath.Ext(f)), "b64": base64.StdEncoding.EncodeToString(data)})
	}
	return w.act(ctx, timeout, target, "upload", map[string]any{"loc": l, "files": payload}, nil)
}

// ---------------------------------------------------------------- waits

func (w *Window) Find(ctx context.Context, target string, state engine.ElementState, timeout time.Duration) error {
	l, err := parseTarget(target)
	if err != nil {
		return err
	}
	if state == "" {
		state = engine.StateVisible
	}
	check := func() error {
		var res struct {
			OK  bool   `json:"ok"`
			Why string `json:"why"`
		}
		if err := w.app.run(ctx, w.label, "state", map[string]any{"loc": l, "state": string(state)}, &res); err != nil {
			return err
		}
		if !res.OK {
			code := "notfound"
			if res.Why == "moving" {
				code = "moving"
			}
			return &agentError{Code: code, Msg: target + " " + res.Why}
		}
		return nil
	}
	if timeout <= 0 {
		return check()
	}
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond
	for {
		err := check()
		if err == nil || !retryable(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("wait for target %q (state=%s) timed out after %s: %v", target, state, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		interval = min(interval*2, 500*time.Millisecond)
	}
}

func (w *Window) pollInfo(ctx context.Context, timeout time.Duration, what string, done func(pageInfo) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		pi, err := w.info(ctx)
		if err == nil && done(pi) {
			return nil
		}
		if err != nil && !retryable(err) && !errors.Is(err, errNoResult) {
			return err
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return fmt.Errorf("%s timed out after %s: %v", what, timeout, err)
			}
			return fmt.Errorf("%s timed out after %s", what, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (w *Window) WaitLoad(ctx context.Context, state string) error {
	switch state {
	case "", "load":
		return w.pollInfo(ctx, 30*time.Second, "wait load", func(pi pageInfo) bool { return pi.Ready == "complete" })
	case "domcontentloaded":
		return w.pollInfo(ctx, 30*time.Second, "wait domcontentloaded", func(pi pageInfo) bool { return pi.Ready != "" && pi.Ready != "loading" })
	case "stable", "idle", "networkidle":
		quiet := int64(300)
		if state != "stable" {
			quiet = 500
		}
		// Best effort, like a settle: a page that never stops changing (a
		// spinner, a clock) is reported, not waited on forever.
		err := w.pollInfo(ctx, 5*time.Second, "wait "+state, func(pi pageInfo) bool {
			return pi.Ready == "complete" && pi.Now-pi.Mutated >= quiet
		})
		if err != nil && ctx.Err() == nil {
			return nil
		}
		return err
	case "none":
		return nil
	}
	return fmt.Errorf("unknown load state %q: use load, domcontentloaded, stable or idle", state)
}

func (w *Window) Settle(ctx context.Context, d time.Duration) {
	start, err := w.info(ctx)
	if err != nil {
		return
	}
	_ = w.pollInfo(ctx, d, "settle", func(pi pageInfo) bool { return pi.Mutated > start.Now || pi.Boot != start.Boot })
}

func (w *Window) Wait(ctx context.Context, spec engine.WaitSpec) error {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if spec.Load != "" {
		if err := w.WaitLoad(ctx, spec.Load); err != nil {
			return fmt.Errorf("wait load %q: %w", spec.Load, err)
		}
	}
	if spec.URL != "" {
		if err := w.pollInfo(ctx, timeout, fmt.Sprintf("wait for url %q", spec.URL), func(pi pageInfo) bool { return strings.Contains(pi.URL, spec.URL) }); err != nil {
			return err
		}
	}
	target := strings.TrimSpace(spec.Ref)
	if target == "" {
		target = strings.TrimSpace(spec.Selector)
	}
	if spec.Text != "" {
		needle := strings.TrimSpace(spec.Text)
		gone := target == "" && strings.EqualFold(strings.TrimSpace(spec.State), string(engine.StateHidden))
		deadline := time.Now().Add(timeout)
		for {
			var res struct {
				OK bool `json:"ok"`
			}
			err := w.app.run(ctx, w.label, "text", map[string]any{"text": needle}, &res)
			if err == nil && res.OK != gone {
				break
			}
			if !time.Now().Before(deadline) {
				if gone {
					return fmt.Errorf("wait for text %q to disappear timed out after %s: still shown", needle, timeout)
				}
				return fmt.Errorf("wait for text %q timed out after %s", needle, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if target != "" {
		state := engine.StateVisible
		if spec.State != "" {
			parsed, err := engine.ParseElementState(spec.State)
			if err != nil {
				return err
			}
			state = parsed
		}
		if err := w.Find(ctx, target, state, timeout); err != nil {
			return fmt.Errorf("wait %q: %w", target, err)
		}
	}
	if spec.MS > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(spec.MS) * time.Millisecond):
		}
	}
	return nil
}

// ---------------------------------------------------------------- screenshot

func (w *Window) capture(ctx context.Context) (image.Image, error) {
	raw, err := w.app.c.Call(ctx, "screenshot", map[string]any{"label": w.label})
	if err != nil {
		return nil, err
	}
	var res struct {
		PNG string `json:"png_base64"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(res.PNG)
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	return img, nil
}

func (w *Window) Screenshot(ctx context.Context, o driver.ShotOptions) (driver.Shot, error) {
	if o.FullPage {
		return driver.Shot{}, fmt.Errorf("full_page is not available through the Tauri bridge (it captures what the webview shows): scroll and capture in steps, or capture an element (target)")
	}
	var crop *image.Rectangle
	if o.Target != "" {
		l, err := parseTarget(o.Target)
		if err != nil {
			return driver.Shot{}, err
		}
		var p point
		if err := w.act(ctx, 5*time.Second, o.Target, "rect", map[string]any{"loc": l}, &p); err != nil {
			return driver.Shot{}, err
		}
		pi, err := w.info(ctx)
		if err != nil {
			return driver.Shot{}, err
		}
		img, err := w.capture(ctx)
		if err != nil {
			return driver.Shot{}, err
		}
		scale := 1.0
		if pi.W > 0 {
			scale = float64(img.Bounds().Dx()) / pi.W
		}
		r := image.Rect(int(math.Floor(p.Rect.X*scale)), int(math.Floor(p.Rect.Y*scale)), int(math.Ceil((p.Rect.X+p.Rect.W)*scale)), int(math.Ceil((p.Rect.Y+p.Rect.H)*scale))).Intersect(img.Bounds())
		if r.Empty() {
			return driver.Shot{}, fmt.Errorf("%s is outside the visible area of the window", o.Target)
		}
		crop = &r
		return encodeShot(img, crop, o)
	}
	img, err := w.capture(ctx)
	if err != nil {
		return driver.Shot{}, err
	}
	return encodeShot(img, crop, o)
}

func encodeShot(img image.Image, crop *image.Rectangle, o driver.ShotOptions) (driver.Shot, error) {
	if crop != nil {
		dst := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
		draw.Draw(dst, dst.Bounds(), img, crop.Min, draw.Src)
		img = dst
	}
	var buf bytes.Buffer
	shot := driver.Shot{Format: o.Format}
	switch o.Format {
	case "", "png":
		shot.Format = "png"
		if err := png.Encode(&buf, img); err != nil {
			return driver.Shot{}, err
		}
	case "jpeg", "jpg", "webp":
		q := o.Quality
		if q < 1 || q > 100 {
			q = 60
		}
		if o.Format == "webp" {
			shot.Note = "the Tauri bridge has no webp encoder: JPEG at the same quality"
		}
		shot.Format = "jpeg"
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return driver.Shot{}, err
		}
	default:
		return driver.Shot{}, fmt.Errorf("format must be webp, jpeg or png")
	}
	shot.Data = buf.Bytes()
	return shot, nil
}

// ---------------------------------------------------------------- eval

// Eval runs expr in the webview. The expression is part of the script the
// bridge evaluates (never passed to eval or new Function, which a strict
// CSP forbids), so it is tried in turn as an expression, as statements whose
// last one is returned, and as a function body.
func (w *Window) Eval(ctx context.Context, expr, target string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	var self string
	if target != "" {
		l, err := parseTarget(target)
		if err != nil {
			return "", err
		}
		lj, _ := json.Marshal(l)
		self = "A.evalTarget(" + string(lj) + ")"
	} else {
		self = "undefined"
	}
	// The agent must be there for A.start: a cheap operation installs it.
	if err := w.app.run(ctx, w.label, "ping", map[string]any{}, nil); err != nil {
		return "", err
	}
	var started struct {
		Pending string `json:"pending"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	var lastErr error
	ran := false
	for _, body := range evalBodies(expr) {
		script := `(function(){try{var A=window.__MCP_AD_AGENT__;if(!A)return '` + installMarker + `';var self=` + self + `;return JSON.stringify(A.start(async function(){` + body + `
},self));}catch(e){return JSON.stringify({error:String(e&&e.message||e),code:(e&&e.code)||'exception'});}})()`
		res, err := w.app.evalRaw(ctx, w.label, script, 0)
		if errors.Is(err, errNoResult) {
			lastErr = err
			continue // did not parse in this form: try the next one
		}
		if err != nil {
			return "", err
		}
		if res == installMarker {
			return "", fmt.Errorf("the page was replaced while evaluating: retry")
		}
		if err := json.Unmarshal([]byte(res), &started); err != nil {
			return "", fmt.Errorf("eval: unexpected answer %.200s", res)
		}
		ran = true
		break
	}
	if !ran {
		return "", fmt.Errorf("the expression did not run: it does not parse as an expression, as statements or as a function body (or the page was navigating): %v", lastErr)
	}
	if started.Error != "" {
		return "", errors.New(started.Error)
	}
	deadline := time.Now().Add(timeout)
	interval := 5 * time.Millisecond
	for {
		var r struct {
			Pending bool   `json:"pending"`
			Value   string `json:"value"`
		}
		err := w.app.run(ctx, w.label, "take", map[string]any{"id": started.Pending}, &r)
		if err != nil {
			var ae *agentError
			if errors.As(err, &ae) && ae.Code == "exception" {
				return "", errors.New(ae.Msg)
			}
			return "", err
		}
		if !r.Pending {
			return r.Value, nil
		}
		if !time.Now().Before(deadline) {
			_ = w.app.run(context.Background(), w.label, "drop", map[string]any{"id": started.Pending}, nil)
			return "", &engine.EvalTimeoutError{Timeout: timeout, Responsive: true}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		interval = min(interval*2, 100*time.Millisecond)
	}
}

// evalBodies returns the async function bodies to try for a user
// expression, most specific first.
func evalBodies(expr string) []string {
	code := strings.TrimSpace(expr)
	code = strings.TrimRight(code, "; \t\n")
	bodies := []string{"return (\n" + code + "\n);"}
	if head, last, ok := splitLast(code, ';'); ok {
		bodies = append(bodies, head+";\nreturn (\n"+last+"\n);")
	}
	if head, last, ok := splitLast(code, '\n'); ok {
		bodies = append(bodies, head+"\nreturn (\n"+last+"\n);")
	}
	return append(bodies, expr)
}

var statementKeywords = []string{"return", "const ", "let ", "var ", "if", "for", "while", "do ", "do{", "switch", "try", "throw ", "function", "class ", "import ", "export ", "async function", "{", "}", "//", "/*"}

// splitLast splits code at its last top-level sep (outside brackets,
// strings, template literals and comments) into the statements before it
// and a final expression statement. ok is false when there is no such split
// or the last statement is not an expression.
func splitLast(code string, sep byte) (head, last string, ok bool) {
	depth := 0
	var quote byte
	cut := -1
	for i := 0; i < len(code); i++ {
		c := code[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '/':
			if i+1 < len(code) && code[i+1] == '/' {
				for i < len(code) && code[i] != '\n' {
					i++
				}
				if sep == '\n' && depth == 0 && i < len(code) {
					cut = i
				}
				continue
			}
			if i+1 < len(code) && code[i+1] == '*' {
				end := strings.Index(code[i+2:], "*/")
				if end < 0 {
					return "", "", false
				}
				i += end + 3
				continue
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		default:
			if c == sep && depth == 0 {
				cut = i
			}
		}
	}
	if cut < 0 {
		return "", "", false
	}
	head, last = code[:cut], strings.TrimSpace(code[cut+1:])
	if last == "" || strings.TrimSpace(head) == "" {
		return "", "", false
	}
	for _, kw := range statementKeywords {
		if strings.HasPrefix(last, kw) {
			return "", "", false
		}
	}
	if strings.ContainsAny(last[:1], ".)]?:+-*/%&|=,") {
		return "", "", false
	}
	return head, last, true
}

// ---------------------------------------------------------------- navigation

func (w *Window) navigateAndWait(ctx context.Context, op string, args map[string]any, wait string) error {
	before, err := w.info(ctx)
	if err != nil {
		return err
	}
	if err := w.app.run(ctx, w.label, op, args, nil); err != nil && !errors.Is(err, errNoResult) {
		return err
	}
	if wait == "none" {
		return nil
	}
	err = w.pollInfo(ctx, 30*time.Second, op, func(pi pageInfo) bool {
		changed := pi.Boot != before.Boot || pi.URL != before.URL
		return changed && pi.Ready != "loading"
	})
	if err != nil {
		return err
	}
	if wait == "" {
		wait = "load"
	}
	return w.WaitLoad(ctx, wait)
}

func (w *Window) Navigate(ctx context.Context, url, wait string) error {
	return w.navigateAndWait(ctx, "navigate", map[string]any{"url": url}, wait)
}

func (w *Window) History(ctx context.Context, delta int, wait string) error {
	return w.navigateAndWait(ctx, "history", map[string]any{"delta": delta}, wait)
}

func (w *Window) Reload(ctx context.Context, ignoreCache bool) error {
	before, err := w.info(ctx)
	if err != nil {
		return err
	}
	if err := w.app.run(ctx, w.label, "reload", map[string]any{}, nil); err != nil && !errors.Is(err, errNoResult) {
		return fmt.Errorf("reload: %w", err)
	}
	if err := w.pollInfo(ctx, 30*time.Second, "reload", func(pi pageInfo) bool { return pi.Boot != before.Boot && pi.Ready == "complete" }); err != nil {
		return err
	}
	return w.WaitLoad(ctx, "stable")
}

// InvokeScript is the eval body calling a Tauri command from a webview, as
// the page would (window.__TAURI_INTERNALS__.invoke). It runs on both
// backends: the bridge, and WebView2's DevTools Protocol on Windows.
func InvokeScript(command string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	cj, _ := json.Marshal(command)
	aj, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return `return (async () => {
const I = window.__TAURI_INTERNALS__;
if (!I || typeof I.invoke !== 'function') throw new Error('window.__TAURI_INTERNALS__.invoke is missing: this window is not a Tauri webview, or shows a page outside the app');
return await I.invoke(` + string(cj) + `, ` + string(aj) + `);
})()`, nil
}

// EmitScript is the eval body emitting a Tauri event to every listener
// (the core event plugin, as @tauri-apps/api's emit does).
func EmitScript(event string, payload any) (string, error) {
	ej, _ := json.Marshal(event)
	pj, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return `return (async () => {
const I = window.__TAURI_INTERNALS__;
if (!I || typeof I.invoke !== 'function') throw new Error('window.__TAURI_INTERNALS__.invoke is missing: this window is not a Tauri webview');
await I.invoke('plugin:event|emit', { event: ` + string(ej) + `, payload: ` + string(pj) + ` });
return 'emitted ' + ` + string(ej) + `;
})()`, nil
}
