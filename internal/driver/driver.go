// Package driver is the contract between the MCP tools (and flows) and one
// window of a driven application, whatever drives it: the Chrome DevTools
// Protocol for Electron and for Tauri on Windows (WebView2), or the Tauri
// bridge plugin for Tauri on macOS and Linux (WKWebView, WebKitGTK).
//
// Both backends speak the same target syntax (@ref, CSS, getByRole/
// getByText/getByLabel), return the same extraction tree (engine types) and
// report the same errors, so a tool or a flow is written once.
package driver

import (
	"context"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/engine"
)

// Backend names, as reported by status and the session description.
const (
	BackendCDP    = "cdp"
	BackendBridge = "bridge"
)

// WindowInfo describes one window (page target, webview) of the app.
type WindowInfo struct {
	Index int
	// Type is "page" for a top-level window; CDP also reports "webview"
	// and "iframe"-like targets an app embeds.
	Type  string
	Title string
	URL   string
	// Label is the Tauri webview label ("main"), empty for CDP targets.
	Label string
}

// App is one live connection to an application.
type App interface {
	Backend() string
	Windows(ctx context.Context) ([]WindowInfo, error)
	Window(ctx context.Context, index int) (Window, error)
}

// HoverMode is the click tool's hover_first.
type HoverMode int

const (
	// HoverAuto reveals a target hidden by visibility or opacity.
	HoverAuto HoverMode = iota
	// HoverAlways hovers first, revealing a display:none target through
	// its nearest rendered ancestor.
	HoverAlways
	// HoverNever requires the target to be visible already.
	HoverNever
)

// ClickOptions tune Click.
type ClickOptions struct {
	Button  string // left (default), right, middle
	Count   int    // 1 (default) or 2
	Hover   HoverMode
	Timeout time.Duration
}

// TypeOptions tune Type.
type TypeOptions struct {
	Append  bool
	Submit  bool
	Timeout time.Duration
}

// ShotOptions tune Screenshot.
type ShotOptions struct {
	Target   string
	FullPage bool
	Format   string // webp, jpeg, png
	Quality  int
}

// Shot is a captured image. Format may differ from the one asked for when
// the backend cannot produce it (the bridge has no webp encoder); Note then
// says so.
type Shot struct {
	Data   []byte
	Format string
	Note   string
}

// Window is one window of the application, bound to the calling tool's
// context.
type Window interface {
	Index() int
	// Info returns the window's current URL and title.
	Info(ctx context.Context) (url, title string, err error)

	// Snapshot extracts the tree at level (scoped to a CSS selector when
	// given) and records its refs for the next targets.
	Snapshot(ctx context.Context, level engine.ExtractLevel, selector string) (*engine.ExtractionResult, error)
	// Refs returns the refs recorded by the last snapshot (nil if none).
	Refs() map[string]engine.RefSnapshot
	// Mutation re-reads the window after an action and diffs its
	// interactive elements against prev.
	Mutation(ctx context.Context, prev map[string]engine.RefSnapshot) (engine.SnapshotDiff, *engine.ExtractionResult, error)

	// Click clicks target; the note says how a hidden target was revealed.
	Click(ctx context.Context, target string, o ClickOptions) (note string, err error)
	// ClickAt clicks a viewport point count times.
	ClickAt(ctx context.Context, x, y float64, count int) error
	// Type types into target (the focused element when target is empty).
	Type(ctx context.Context, target, text string, o TypeOptions) error
	Select(ctx context.Context, target string, values []string, timeout time.Duration) error
	// Press presses a key combination, on target (focused first) when given.
	Press(ctx context.Context, target, combo string, timeout time.Duration) error
	Hover(ctx context.Context, target string, timeout time.Duration) error
	Drag(ctx context.Context, from, to string, steps int, timeout time.Duration) error
	ScrollIntoView(ctx context.Context, target string, timeout time.Duration) error
	// ScrollBy scrolls the window and returns the new scroll position
	// (scrollX + scrollY), so a caller can tell when it stopped moving.
	ScrollBy(ctx context.Context, dx, dy int) (float64, error)
	// Viewport returns the window's inner size in CSS pixels.
	Viewport(ctx context.Context) (w, h float64, err error)
	Wheel(ctx context.Context, target string, x, y, dy float64, steps int, interval, timeout time.Duration) error
	Upload(ctx context.Context, target string, files []string, timeout time.Duration) error

	// Wait waits for the wait_for conditions.
	Wait(ctx context.Context, spec engine.WaitSpec) error
	// Find waits up to timeout for target to reach state (visible,
	// hidden, attached, enabled, stable). A zero timeout checks once.
	Find(ctx context.Context, target string, state engine.ElementState, timeout time.Duration) error
	// Settle waits at most d for the DOM to stop changing after an action.
	Settle(ctx context.Context, d time.Duration)
	// WaitLoad waits for a load state (load, domcontentloaded, stable, idle).
	WaitLoad(ctx context.Context, state string) error

	Screenshot(ctx context.Context, o ShotOptions) (Shot, error)
	// Eval evaluates JavaScript in the window (`this` is target when given).
	Eval(ctx context.Context, expr, target string, timeout time.Duration) (string, error)

	Navigate(ctx context.Context, url, wait string) error
	History(ctx context.Context, delta int, wait string) error
	Reload(ctx context.Context, ignoreCache bool) error
}
