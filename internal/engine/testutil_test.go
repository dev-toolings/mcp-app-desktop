package engine

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// One headless system Chrome serves the whole package: launching one per test
// made Chrome's Dock icon appear and vanish fifty times per run. Tests attach
// to it exactly the way the server attaches to an Electron app (Connect, never
// Rod's launch path) and each gets its own window. Requires an installed
// Chrome; tests skip under -short.
var shared struct {
	once sync.Once
	l    *launcher.Launcher
	dir  string
	b    *Browser
	skip string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.b != nil {
		shared.b.Close()
		shared.l.Kill()
		shared.l.Cleanup()
		_ = os.RemoveAll(shared.dir)
	}
	os.Exit(code)
}

func sharedBrowser(t *testing.T) *Browser {
	t.Helper()
	if testing.Short() {
		t.Skip("requires Chrome")
	}
	shared.once.Do(func() {
		bin, ok := launcher.LookPath()
		if !ok {
			shared.skip = "no Chrome/Chromium installed"
			return
		}
		dir, err := os.MkdirTemp("", "mcp-app-desktop-engine-chrome-*")
		if err != nil {
			shared.skip = fmt.Sprintf("temp profile: %v", err)
			return
		}
		l := launcher.New().Bin(bin).Headless(true).Leakless(false).Delete("no-startup-window").UserDataDir(dir)
		controlURL, err := l.Launch()
		if err != nil {
			shared.skip = fmt.Sprintf("launch chrome: %v", err)
			_ = os.RemoveAll(dir)
			return
		}
		b, err := Connect(context.Background(), controlURL, 10*time.Second)
		if err != nil {
			l.Kill()
			_ = os.RemoveAll(dir)
			shared.skip = fmt.Sprintf("connect: %v", err)
			return
		}
		shared.l, shared.dir, shared.b = l, dir, b
	})
	if shared.skip != "" {
		t.Skip(shared.skip)
	}
	return shared.b
}

// testBrowser returns the shared browser; the cleanup is kept for call sites
// written when every test owned a browser.
func testBrowser(t *testing.T) (*Browser, func()) {
	t.Helper()
	return sharedBrowser(t), func() {}
}

// newIsolatedPage opens a fresh window in the shared browser, attached
// through the production path, and closes it when the test ends.
func newIsolatedPage(t *testing.T) (*Browser, *rod.Page) {
	t.Helper()
	b := sharedBrowser(t)
	created, err := proto.TargetCreateTarget{URL: "about:blank", NewWindow: true}.Call(b.browser)
	if err != nil {
		t.Fatalf("new window: %v", err)
	}
	t.Cleanup(func() {
		_, _ = proto.TargetCloseTarget{TargetID: created.TargetID}.Call(b.browser)
		_, _ = b.Windows()
	})
	wins, err := b.Windows()
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	for _, w := range wins {
		if w.TargetID == string(created.TargetID) {
			page, err := b.Page(w.Index)
			if err != nil {
				t.Fatalf("page: %v", err)
			}
			return b, page
		}
	}
	t.Fatalf("new window %s not listed", created.TargetID)
	return nil, nil
}

// dataURL wraps an HTML fragment into a navigable data: URL.
func dataURL(html string) string {
	return "data:text/html," + url.PathEscape(html)
}
