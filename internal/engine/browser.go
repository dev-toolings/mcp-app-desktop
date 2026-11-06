package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Browser is one CDP connection to a running Electron application. It never
// owns the process: the host layer (internal/electron) launches, restarts
// and stops the app; this type attaches to its remote debugging endpoint,
// enumerates its windows and keeps the per-window ref snapshots in memory.
type Browser struct {
	browser   *rod.Browser
	endpoint  string
	timeout   time.Duration
	ownership Ownership
	cancel    context.CancelFunc
	dialer    *recordingDialer

	mu        sync.Mutex
	snapshots map[proto.TargetTargetID]*PageSnapshot
	pages     map[proto.TargetTargetID]*rod.Page
	// order assigns a stable window index to every target in the order it
	// was first seen, so an index an agent read keeps pointing at the same
	// window while others open or close.
	order     []proto.TargetTargetID
	closeOnce sync.Once
}

// WindowInfo describes one Electron page target (BrowserWindow,
// WebContentsView or <webview>) as reported by CDP.
type WindowInfo struct {
	Index    int    `json:"index"`
	TargetID string `json:"target_id"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	// Attached is true when this target was already opened as a page.
	Attached bool `json:"attached,omitempty"`
}

// Connect attaches to a CDP endpoint: http(s)://host:port (resolved through
// /json/version) or a full ws:// URL. The connection is loopback-only.
func Connect(ctx context.Context, endpoint string, timeout time.Duration) (*Browser, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("connect: endpoint is required")
	}
	if err := requireLoopback(endpoint); err != nil {
		return nil, err
	}
	wsURL, err := launcher.ResolveURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	bctx, cancel := context.WithCancel(context.Background())
	dialer := &recordingDialer{}
	ws := &cdp.WebSocket{Dialer: dialer}
	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	defer dialCancel()
	if err := ws.Connect(dialCtx, wsURL, http.Header{}); err != nil {
		cancel()
		return nil, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	client := cdp.New().Start(ws)
	// NoDefaultDevice: rod otherwise emulates a 1280x800 laptop (scale 1)
	// on every page, which paints the app in a corner of its real window.
	rb := rod.New().Client(client).Context(bctx).NoDefaultDevice()
	if err := rb.Connect(); err != nil {
		cancel()
		dialer.closeAll()
		return nil, fmt.Errorf("connect %s: %w", endpoint, err)
	}
	b := &Browser{
		browser:   rb,
		endpoint:  endpoint,
		timeout:   timeout,
		ownership: OwnershipAttached,
		cancel:    cancel,
		dialer:    dialer,
		snapshots: map[proto.TargetTargetID]*PageSnapshot{},
		pages:     map[proto.TargetTargetID]*rod.Page{},
	}
	_ = watcherFor(rb)
	return b, nil
}

func requireLoopback(endpoint string) error {
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("connect: %q is not a loopback endpoint; mcp-app-desktop only attaches to local applications", endpoint)
	}
	return nil
}

// recordingDialer remembers the connections it opened so Close can drop the
// WebSocket without sending Browser.close, which would quit the application.
type recordingDialer struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (d *recordingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

func (d *recordingDialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

// Endpoint returns the endpoint this browser attached to.
func (b *Browser) Endpoint() string { return b.endpoint }

// RodBrowser exposes the underlying connection.
func (b *Browser) RodBrowser() *rod.Browser { return b.browser }

// Alive reports whether the connection still answers within d.
func (b *Browser) Alive(d time.Duration) bool {
	if b == nil || b.browser == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := proto.TargetGetTargets{}.Call(b.browser.Context(ctx))
	return err == nil
}

// windowTarget reports whether a CDP target is an Electron page an agent can
// drive: BrowserWindow and WebContentsView contents are "page", <webview>
// guests are "webview". Service workers, DevTools and the like are skipped.
func windowTarget(t *proto.TargetTargetInfo) bool {
	if t == nil {
		return false
	}
	switch t.Type {
	case proto.TargetTargetInfoTypePage, "webview":
	default:
		return false
	}
	return !strings.HasPrefix(t.URL, "devtools://") && !strings.HasPrefix(t.URL, "chrome-extension://")
}

// windowsTimeout bounds one listing of the application's targets.
const windowsTimeout = 10 * time.Second

// Windows lists the application's pages with their stable indexes.
func (b *Browser) Windows() ([]WindowInfo, error) {
	if b == nil || b.browser == nil {
		return nil, fmt.Errorf("browser is not connected")
	}
	// Bounded: the connection has no deadline of its own, and an app busy
	// starting up held a launch on this call (trace, 2026-10-24); status,
	// logs and every page tool list windows.
	res, err := proto.TargetGetTargets{}.Call(b.browser.Timeout(windowsTimeout))
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	live := map[proto.TargetTargetID]*proto.TargetTargetInfo{}
	for _, t := range res.TargetInfos {
		if windowTarget(t) {
			live[t.TargetID] = t
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Keep the index of every target still alive, append newcomers in the
	// order Chromium reports them, and forget what disappeared.
	kept := b.order[:0:0]
	seen := map[proto.TargetTargetID]bool{}
	for _, id := range b.order {
		if _, ok := live[id]; ok {
			kept = append(kept, id)
			seen[id] = true
		} else {
			delete(b.snapshots, id)
			if p, ok := b.pages[id]; ok {
				forgetPageEventState(p)
				delete(b.pages, id)
			}
		}
	}
	newcomers := make([]proto.TargetTargetID, 0)
	for _, t := range res.TargetInfos {
		if _, ok := live[t.TargetID]; ok && !seen[t.TargetID] {
			newcomers = append(newcomers, t.TargetID)
		}
	}
	sort.SliceStable(newcomers, func(i, j int) bool { return newcomers[i] < newcomers[j] })
	b.order = append(kept, newcomers...)
	out := make([]WindowInfo, 0, len(b.order))
	for i, id := range b.order {
		t := live[id]
		_, attached := b.pages[id]
		out = append(out, WindowInfo{Index: i, TargetID: string(id), Type: string(t.Type), URL: t.URL, Title: t.Title, Attached: attached})
	}
	return out, nil
}

// Page returns the page for a window index (0 is the first window seen). A
// negative index selects the only window, or the first one.
func (b *Browser) Page(index int) (*rod.Page, error) {
	wins, err := b.Windows()
	if err != nil {
		return nil, err
	}
	if len(wins) == 0 {
		return nil, fmt.Errorf("the application has no window yet")
	}
	if index < 0 {
		index = 0
	}
	if index >= len(wins) {
		return nil, fmt.Errorf("window %d does not exist (%d windows: 0-%d)", index, len(wins), len(wins)-1)
	}
	id := proto.TargetTargetID(wins[index].TargetID)
	b.mu.Lock()
	p, ok := b.pages[id]
	b.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err = b.browser.PageFromTarget(id)
	if err != nil {
		return nil, fmt.Errorf("attach to window %d: %w", index, err)
	}
	b.mu.Lock()
	if existing, ok := b.pages[id]; ok {
		p = existing
	} else {
		b.pages[id] = p
	}
	b.mu.Unlock()
	return p, nil
}

// PageByTarget returns the cached page for a target, if any.
func (b *Browser) PageByTarget(id proto.TargetTargetID) *rod.Page {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pages[id]
}

// IndexOf returns the window index of a page, or -1.
func (b *Browser) IndexOf(page *rod.Page) int {
	if page == nil {
		return -1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, id := range b.order {
		if id == page.TargetID {
			return i
		}
	}
	return -1
}

// SaveSnapshot records the refs of an extraction for the page's target.
func (b *Browser) SaveSnapshot(page *rod.Page, result *ExtractionResult) error {
	if b == nil || page == nil || result == nil {
		return nil
	}
	snapshot, err := BuildSnapshot(page, result)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.snapshots[page.TargetID] = snapshot
	b.mu.Unlock()
	return nil
}

// Snapshot returns the last snapshot of the page's target.
func (b *Browser) Snapshot(page *rod.Page) *PageSnapshot {
	if b == nil || page == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.snapshots[page.TargetID]; ok {
		cp := *s
		return &cp
	}
	return nil
}

// CachedExtract returns the last extraction when the page URL is unchanged.
func (b *Browser) CachedExtract(page *rod.Page) *ExtractionResult {
	snap := b.Snapshot(page)
	if snap == nil || snap.CachedExtraction == nil || snap.URL == "" {
		return nil
	}
	currentURL, err := page.Eval("() => location.href")
	if err != nil || currentURL.Value.Str() != snap.URL {
		return nil
	}
	return snap.CachedExtraction
}

// InvalidateCachedExtract drops the cached accessibility dump for page so the
// next snapshot re-extracts after a mutating action. Refs stay in place so
// click/type can still resolve @N until a fresh extract overwrites them.
func (b *Browser) InvalidateCachedExtract(page *rod.Page) error {
	if b == nil || page == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.snapshots[page.TargetID]; ok {
		s.CachedExtraction = nil
	}
	return nil
}

// DeleteSnapshot removes stored ref state for a closed page target.
func (b *Browser) DeleteSnapshot(targetID proto.TargetTargetID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.snapshots, targetID)
	return nil
}

// Close detaches from the application. It never sends Browser.close: the
// Electron process belongs to the host layer, which stops it when it owns it.
func (b *Browser) Close() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		// Cut the connection first: a half-dead app (quitting, frozen)
		// would otherwise leave a CDP call pending forever, and Close runs
		// under the session lock. Everything after this is local cleanup.
		if b.cancel != nil {
			b.cancel()
		}
		if b.dialer != nil {
			b.dialer.closeAll()
		}
		if b.browser != nil {
			stopBrowserEventHubs(b.browser)
			b.mu.Lock()
			pages := make([]*rod.Page, 0, len(b.pages))
			for _, p := range b.pages {
				pages = append(pages, p)
			}
			b.mu.Unlock()
			for _, p := range pages {
				forgetPageEventState(p)
			}
			StopPopupWatch(b.browser)
		}
	})
}
