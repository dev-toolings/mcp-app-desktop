// Package cdpwin implements driver.Window over the Chrome DevTools Protocol:
// Electron windows, and Tauri windows on Windows (WebView2). It is a thin
// layer over the engine, which does the work (accessibility snapshots,
// strict locators, real input events).
package cdpwin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// App is a driver.App over a CDP connection.
type App struct {
	b *engine.Browser
	// prepare runs once per page before it is driven (event hub, dialog
	// handler, focus emulation); the session owns that state.
	prepare func(*engine.Browser, *rod.Page)
}

// New wraps a connection; prepare may be nil.
func New(b *engine.Browser, prepare func(*engine.Browser, *rod.Page)) *App {
	return &App{b: b, prepare: prepare}
}

// Browser exposes the CDP connection (Electron-only tools, tests).
func (a *App) Browser() *engine.Browser { return a.b }

func (a *App) Backend() string { return driver.BackendCDP }

func (a *App) Windows(ctx context.Context) ([]driver.WindowInfo, error) {
	wins, err := a.b.Windows()
	if err != nil {
		return nil, err
	}
	out := make([]driver.WindowInfo, 0, len(wins))
	for _, w := range wins {
		out = append(out, driver.WindowInfo{Index: w.Index, Type: w.Type, Title: w.Title, URL: w.URL})
	}
	return out, nil
}

func (a *App) Window(ctx context.Context, index int) (driver.Window, error) {
	page, err := a.b.Page(index)
	if err != nil {
		return nil, err
	}
	if a.prepare != nil {
		a.prepare(a.b, page)
	}
	// The caller's context bounds every CDP call on the returned page, so a
	// cancelled tool call or an interrupted flow stops waiting at once.
	return &Window{b: a.b, page: page.Context(ctx), index: a.b.IndexOf(page)}, nil
}

// Window is one CDP page target.
type Window struct {
	b     *engine.Browser
	page  *rod.Page
	index int
}

// Page exposes the rod page (tests and Electron-only helpers).
func (w *Window) Page() *rod.Page { return w.page }

func (w *Window) Index() int { return w.index }

func (w *Window) Info(ctx context.Context) (string, string, error) {
	info, err := w.page.Info()
	if err != nil {
		return "", "", err
	}
	if info == nil {
		return "", "", nil
	}
	return info.URL, info.Title, nil
}

func (w *Window) Snapshot(ctx context.Context, level engine.ExtractLevel, selector string) (*engine.ExtractionResult, error) {
	if err := engine.ValidateExtractLevel(level); err != nil {
		return nil, err
	}
	result, err := engine.Extract(w.page, level, selector)
	if err != nil {
		return nil, err
	}
	_ = w.b.SaveSnapshot(w.page, result)
	return result, nil
}

func (w *Window) Refs() map[string]engine.RefSnapshot {
	if snap := w.b.Snapshot(w.page); snap != nil {
		return snap.Refs
	}
	return nil
}

func (w *Window) Mutation(ctx context.Context, prev map[string]engine.RefSnapshot) (engine.SnapshotDiff, *engine.ExtractionResult, error) {
	return engine.CaptureMutation(w.b, w.page, &engine.PageSnapshot{Refs: prev})
}

// resolve waits for the target to be visible and actionable.
func (w *Window) resolve(target string, timeout time.Duration) (*rod.Element, error) {
	if target == "" {
		return nil, fmt.Errorf("target is required (@ref from snapshot, a CSS selector, or getByRole/getByText/getByLabel)")
	}
	el, err := engine.WaitForTarget(w.page, target, w.b.Snapshot(w.page), engine.StateVisible, timeout)
	if err != nil {
		return nil, err
	}
	if el == nil {
		return nil, fmt.Errorf("%s: not found", target)
	}
	if err := engine.EnsureActionable(el, timeout); err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	return el, nil
}

func (w *Window) attached(target string, timeout time.Duration) (*rod.Element, error) {
	el, err := engine.WaitForTarget(w.page, target, w.b.Snapshot(w.page), engine.StateAttached, timeout)
	if err != nil {
		return nil, err
	}
	if el == nil {
		return nil, fmt.Errorf("%s: not found", target)
	}
	return el, nil
}

// resolveClickable resolves a click target. A target that only shows on
// hover (per-row buttons styled with :hover) is revealed with a real mouse
// move over it (or over its row) before the usual actionability checks, so
// the click that follows is a real mouse event on a visible element.
func (w *Window) resolveClickable(target string, timeout time.Duration, mode driver.HoverMode) (el *rod.Element, note string, err error) {
	if mode == driver.HoverNever {
		el, err := w.resolve(target, timeout)
		return el, "", err
	}
	if target == "" {
		return nil, "", fmt.Errorf("target is required (@ref from snapshot, a CSS selector, or getByRole/getByText/getByLabel)")
	}
	deadline := time.Now().Add(timeout)
	el, err = w.attached(target, timeout)
	if err != nil {
		return nil, "", err
	}
	reveal, err := engine.InspectHoverReveal(el)
	if err != nil {
		if mode == driver.HoverAlways {
			return nil, "", fmt.Errorf("%s: %w", target, err)
		}
		// Auto mode only reveals boxed elements; anything else takes the
		// ordinary path, which waits for the target to show up.
		reveal = nil
	}
	hover := reveal != nil && (mode == driver.HoverAlways || reveal.HiddenBy == "visibility" || reveal.HiddenBy == "opacity")
	if !hover {
		el, err := w.resolve(target, max(time.Until(deadline), time.Second))
		return el, "", err
	}
	if err := engine.HoverToReveal(w.page, reveal); err != nil {
		return nil, "", fmt.Errorf("%s: %w", target, err)
	}
	if reveal.HiddenBy != "" {
		note = fmt.Sprintf(" (revealed by hovering, was hidden by %s)", reveal.HiddenBy)
		if reveal.Ancestor != "" {
			note = fmt.Sprintf(" (revealed by hovering %s, was hidden by %s)", reveal.Ancestor, reveal.HiddenBy)
		}
	}
	if err := engine.EnsureActionable(el, max(time.Until(deadline), time.Second)); err != nil {
		if reveal.HiddenBy != "" {
			return nil, "", fmt.Errorf("%s: %w (it was hidden by %s and hovering it did not reveal it)", target, err, reveal.HiddenBy)
		}
		return nil, "", fmt.Errorf("%s: %w", target, err)
	}
	return el, note, nil
}

func mouseButton(s string) (proto.InputMouseButton, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "left":
		return proto.InputMouseButtonLeft, nil
	case "right":
		return proto.InputMouseButtonRight, nil
	case "middle":
		return proto.InputMouseButtonMiddle, nil
	}
	return "", fmt.Errorf("button must be left, right or middle")
}

func (w *Window) Click(ctx context.Context, target string, o driver.ClickOptions) (string, error) {
	button, err := mouseButton(o.Button)
	if err != nil {
		return "", err
	}
	el, note, err := w.resolveClickable(target, o.Timeout, o.Hover)
	if err != nil {
		return "", err
	}
	if o.Count == 2 {
		return note, engine.DblClickElementWithButton(w.page, el, button)
	}
	return note, engine.ClickElementWithButton(w.page, el, button)
}

func (w *Window) ClickAt(ctx context.Context, x, y float64, count int) error {
	if err := w.page.Mouse.MoveTo(proto.Point{X: x, Y: y}); err != nil {
		return err
	}
	return w.page.Mouse.Click(proto.InputMouseButtonLeft, max(count, 1))
}

func (w *Window) Type(ctx context.Context, target, text string, o driver.TypeOptions) error {
	var el *rod.Element
	if target != "" {
		var err error
		if el, err = w.resolve(target, o.Timeout); err != nil {
			return err
		}
	}
	switch {
	case el != nil && o.Append:
		if err := engine.FocusElement(el); err != nil {
			return err
		}
		if err := w.page.InsertText(text); err != nil {
			return err
		}
	case el != nil:
		if err := engine.TypeElement(w.page, el, text); err != nil {
			return err
		}
	default:
		if err := engine.TypeFocused(w.page, text); err != nil {
			return err
		}
	}
	if !o.Submit {
		return nil
	}
	if el != nil {
		return engine.SubmitOnElement(w.page, el)
	}
	return w.pressKeys("Enter")
}

func (w *Window) Select(ctx context.Context, target string, values []string, timeout time.Duration) error {
	el, err := w.resolve(target, timeout)
	if err != nil {
		return err
	}
	if err := engine.SelectOptions(el, values); err != nil {
		return fmt.Errorf("select: %w", err)
	}
	return nil
}

func (w *Window) Press(ctx context.Context, target, combo string, timeout time.Duration) error {
	if _, err := driver.ParseCombo(combo); err != nil {
		return err
	}
	if target != "" {
		el, err := w.resolve(target, timeout)
		if err != nil {
			return err
		}
		if err := engine.FocusElement(el); err != nil {
			return fmt.Errorf("focus: %w", err)
		}
	}
	return w.pressKeys(combo)
}

// rodKeys maps DOM key values to rod input keys.
var rodKeys = map[string]input.Key{
	"Enter": input.Enter, "Tab": input.Tab, "Escape": input.Escape, "Backspace": input.Backspace,
	"Delete": input.Delete, " ": input.Space, "ArrowUp": input.ArrowUp, "ArrowDown": input.ArrowDown,
	"ArrowLeft": input.ArrowLeft, "ArrowRight": input.ArrowRight, "Home": input.Home, "End": input.End,
	"PageUp": input.PageUp, "PageDown": input.PageDown, "Insert": input.Insert,
	"F1": input.F1, "F2": input.F2, "F3": input.F3, "F4": input.F4, "F5": input.F5, "F6": input.F6,
	"F7": input.F7, "F8": input.F8, "F9": input.F9, "F10": input.F10, "F11": input.F11, "F12": input.F12,
	"Meta": input.MetaLeft, "Control": input.ControlLeft, "Alt": input.AltLeft, "Shift": input.ShiftLeft,
}

func rodKey(name string) input.Key {
	if k, ok := rodKeys[name]; ok {
		return k
	}
	return input.Key([]rune(name)[0])
}

// PressKeys dispatches a key combination to the page's focused element.
func PressKeys(page *rod.Page, combo string) error {
	c, err := driver.ParseCombo(combo)
	if err != nil {
		return err
	}
	mods := make([]input.Key, 0, len(c.Mods))
	for _, m := range c.Mods {
		mods = append(mods, rodKey(m))
	}
	ka := page.KeyActions()
	if len(mods) > 0 {
		ka = ka.Press(mods...)
	}
	ka = ka.Type(rodKey(c.Key))
	if len(mods) > 0 {
		ka = ka.Release(mods...)
	}
	if err := ka.Do(); err != nil {
		return fmt.Errorf("press %s: %w", combo, err)
	}
	return nil
}

func (w *Window) pressKeys(combo string) error { return PressKeys(w.page, combo) }

func (w *Window) Hover(ctx context.Context, target string, timeout time.Duration) error {
	el, err := w.resolve(target, timeout)
	if err != nil {
		return err
	}
	return engine.HoverElement(w.page, el)
}

func (w *Window) Drag(ctx context.Context, from, to string, steps int, timeout time.Duration) error {
	src, err := w.resolve(from, timeout)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	dst, err := w.resolve(to, timeout)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	sb, err := src.Shape()
	if err != nil {
		return err
	}
	db, err := dst.Shape()
	if err != nil {
		return err
	}
	sr, dr := sb.Box(), db.Box()
	if sr == nil || dr == nil {
		return fmt.Errorf("source or target has no visible box")
	}
	return engine.DragDropCoords(w.page, sr.X+sr.Width/2, sr.Y+sr.Height/2, dr.X+dr.Width/2, dr.Y+dr.Height/2, steps)
}

func (w *Window) ScrollIntoView(ctx context.Context, target string, timeout time.Duration) error {
	el, err := w.attached(target, timeout)
	if err != nil {
		return err
	}
	return engine.ScrollIntoViewNow(el)
}

func (w *Window) ScrollBy(ctx context.Context, dx, dy int) (float64, error) {
	if dx == 0 {
		if _, err := engine.ScrollBy(w.page, dy); err != nil {
			return 0, err
		}
	} else if _, err := w.page.Eval(`(dx, dy) => window.scrollBy(dx, dy)`, dx, dy); err != nil {
		return 0, err
	}
	pos, err := w.page.Eval(`() => window.scrollY + window.scrollX`)
	if err != nil {
		return 0, err
	}
	return pos.Value.Num(), nil
}

func (w *Window) Viewport(ctx context.Context) (float64, float64, error) {
	v, err := w.page.Eval(`() => [window.innerWidth, window.innerHeight]`)
	if err != nil {
		return 0, 0, err
	}
	arr := v.Value.Arr()
	if len(arr) != 2 {
		return 0, 0, fmt.Errorf("viewport: unexpected %s", v.Value.String())
	}
	return arr[0].Num(), arr[1].Num(), nil
}

func (w *Window) Wheel(ctx context.Context, target string, x, y, dy float64, steps int, interval, timeout time.Duration) error {
	px, py := x, y
	if target != "" {
		el, err := w.resolve(target, timeout)
		if err != nil {
			return err
		}
		shape, err := el.Shape()
		if err != nil {
			return err
		}
		box := shape.Box()
		if box == nil {
			return fmt.Errorf("%s has no visible box", target)
		}
		px, py = box.X+box.Width/2, box.Y+box.Height/2
	}
	return engine.WheelAt(w.page, px, py, dy, steps, interval)
}

func (w *Window) Upload(ctx context.Context, target string, files []string, timeout time.Duration) error {
	el, err := w.attached(target, timeout)
	if err != nil {
		return err
	}
	return el.SetFiles(files)
}

func (w *Window) Wait(ctx context.Context, spec engine.WaitSpec) error {
	_, err := engine.WaitForAgent(w.page, w.b, w.b.Snapshot(w.page), spec, spec.Timeout)
	return err
}

func (w *Window) Find(ctx context.Context, target string, state engine.ElementState, timeout time.Duration) error {
	el, err := engine.WaitForTarget(w.page, target, w.b.Snapshot(w.page), state, timeout)
	if err != nil {
		return err
	}
	switch state {
	case engine.StateHidden:
		if el != nil {
			return fmt.Errorf("%s is still visible", target)
		}
	default:
		if el == nil {
			return fmt.Errorf("%s: not found", target)
		}
	}
	return nil
}

func (w *Window) Settle(ctx context.Context, d time.Duration) {
	_ = engine.WaitForImminentDOM(w.page, d)
}

func (w *Window) WaitLoad(ctx context.Context, state string) error {
	return engine.WaitForPage(w.page, state)
}

func (w *Window) Screenshot(ctx context.Context, o driver.ShotOptions) (driver.Shot, error) {
	format := o.Format
	if format == "" {
		format = "png"
	}
	data, err := engine.TakeScreenshotFormat(w.page, o.FullPage, o.Target, format, o.Quality, w.b.Snapshot(w.page))
	if err != nil {
		return driver.Shot{}, err
	}
	return driver.Shot{Data: data, Format: format}, nil
}

func (w *Window) Eval(ctx context.Context, expr, target string, timeout time.Duration) (string, error) {
	value, err := engine.EvalJSTimeout(w.page, expr, target, w.b.Snapshot(w.page), timeout)
	if err != nil {
		return "", err
	}
	_ = w.b.InvalidateCachedExtract(w.page)
	return value, nil
}

func (w *Window) Navigate(ctx context.Context, url, wait string) error {
	_, err := engine.Navigate(w.page, url, wait)
	_ = w.b.InvalidateCachedExtract(w.page)
	return err
}

func (w *Window) History(ctx context.Context, delta int, wait string) error {
	err := engine.HistoryStep(w.page, delta, wait)
	_ = w.b.InvalidateCachedExtract(w.page)
	return err
}

func (w *Window) Reload(ctx context.Context, ignoreCache bool) error {
	wait := w.page.Timeout(30 * time.Second).WaitEvent(&proto.PageLoadEventFired{})
	if err := (proto.PageReload{IgnoreCache: ignoreCache}).Call(w.page); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	wait()
	_ = engine.WaitForPage(w.page, "stable")
	_ = w.b.InvalidateCachedExtract(w.page)
	return nil
}
