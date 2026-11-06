package engine

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// WebP shares the JPEG path (elementScreenshotFmt); only PNG and JPEG are decoded
// here to avoid a test-only image dependency.

// A red box on a blue page: a capture that drifts shows blue.
const screenshotFixture = `<body style="margin:0;background:rgb(0,0,255);height:3000px">
<div id="box" style="position:absolute;left:300px;top:200px;width:120px;height:60px;background:rgb(255,0,0)"></div>
<div id="low" style="position:absolute;left:40px;top:1800px;width:80px;height:40px;background:rgb(255,0,0)"></div>
</body>`

// zoomedPage opens a page in a dedicated headless Chrome whose default page
// zoom is factor, the way Escape zooms its renderer (webFrame.setZoomFactor).
func zoomedPage(t *testing.T, factor float64) *rod.Page {
	t.Helper()
	if testing.Short() {
		t.Skip("requires Chrome")
	}
	bin, ok := launcher.LookPath()
	if !ok {
		t.Skip("no Chrome/Chromium installed")
	}
	dir, err := os.MkdirTemp("", "mcp-app-desktop-zoom-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	level := math.Log(factor) / math.Log(1.2) // Chrome stores zoom as a 1.2^level exponent
	prefs := fmt.Sprintf(`{"partition":{"default_zoom_level":{"x":%v}}}`, level)
	if err := os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(prefs), 0o644); err != nil {
		t.Fatal(err)
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).Delete("no-startup-window").UserDataDir(dir)
	controlURL, err := l.Launch()
	if err != nil {
		t.Fatalf("launch chrome: %v", err)
	}
	b, err := Connect(context.Background(), controlURL, 10*time.Second)
	if err != nil {
		l.Kill()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { b.Close(); l.Kill() })
	created, err := proto.TargetCreateTarget{URL: "about:blank", NewWindow: true}.Call(b.browser)
	if err != nil {
		t.Fatalf("new window: %v", err)
	}
	wins, err := b.Windows()
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	var page *rod.Page
	for _, w := range wins {
		if w.TargetID == string(created.TargetID) {
			if page, err = b.Page(w.Index); err != nil {
				t.Fatalf("page: %v", err)
			}
		}
	}
	if page == nil {
		t.Fatalf("window %s not listed", created.TargetID)
	}
	page.MustNavigate(dataURL(screenshotFixture)).MustWaitLoad()
	m, err := proto.PageGetLayoutMetrics{}.Call(page)
	if err != nil {
		t.Fatal(err)
	}
	if z := m.CSSVisualViewport.Zoom; z == nil || math.Abs(*z-factor) > 0.01 {
		t.Fatalf("page zoom not applied: %v", z)
	}
	return page
}

// emulateDPR gives the page a Retina-like device scale factor.
func emulateDPR(t *testing.T, page *rod.Page, dpr float64) {
	t.Helper()
	if dpr == 1 {
		return
	}
	if err := (proto.EmulationSetDeviceMetricsOverride{DeviceScaleFactor: dpr}).Call(page); err != nil {
		t.Fatalf("emulate dpr: %v", err)
	}
	t.Cleanup(func() { _ = proto.EmulationClearDeviceMetricsOverride{}.Call(page) })
}

// assertFramesBox checks the capture is the element alone: its size is the
// element's CSS size × devicePixelRatio and every pixel off the antialiased
// edge is the element's red.
func assertFramesBox(t *testing.T, page *rod.Page, selector string, data []byte, cssW, cssH float64) {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	dpr := page.MustEval(`() => devicePixelRatio`).Num()
	b := img.Bounds()
	wantW, wantH := cssW*dpr, cssH*dpr
	if math.Abs(float64(b.Dx())-wantW) > 2 || math.Abs(float64(b.Dy())-wantH) > 2 {
		t.Fatalf("%s: capture is %dx%d, want %.0fx%.0f (dpr %.3f)", selector, b.Dx(), b.Dy(), wantW, wantH, dpr)
	}
	bad := 0
	var first image.Point
	for y := b.Min.Y + 2; y < b.Max.Y-2; y++ {
		for x := b.Min.X + 2; x < b.Max.X-2; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 < 200 || g>>8 > 60 || bl>>8 > 60 {
				if bad == 0 {
					first = image.Pt(x, y)
				}
				bad++
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%s: %d pixels are not the element (first at %v): the clip is offset", selector, bad, first)
	}
}

func TestElementScreenshotFramesElementUnderZoom(t *testing.T) {
	page := zoomedPage(t, 12.0/14.0)
	for _, dpr := range []float64{1, 2} {
		for _, format := range []string{"png", "jpeg"} {
			t.Run(fmt.Sprintf("dpr%v/%s", dpr, format), func(t *testing.T) {
				emulateDPR(t, page, dpr)
				page.MustEval(`() => window.scrollTo(0, 0)`)
				data, err := TakeScreenshotFormat(page, false, "#box", format, 90, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertFramesBox(t, page, "#box", data, 120, 60)
				// Below the fold, and with the document scrolled away from it.
				page.MustEval(`() => window.scrollTo(0, 600)`)
				data, err = TakeScreenshotFormat(page, false, "#low", format, 90, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertFramesBox(t, page, "#low", data, 80, 40)
			})
		}
	}
}

func TestElementScreenshotFramesElementUnzoomed(t *testing.T) {
	_, page := newIsolatedPage(t)
	page.MustNavigate(dataURL(screenshotFixture)).MustWaitLoad()
	for _, dpr := range []float64{1, 2} {
		for _, format := range []string{"png", "jpeg"} {
			t.Run(fmt.Sprintf("dpr%v/%s", dpr, format), func(t *testing.T) {
				emulateDPR(t, page, dpr)
				page.MustEval(`() => window.scrollTo(0, 0)`)
				data, err := TakeScreenshotFormat(page, false, "#box", format, 90, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertFramesBox(t, page, "#box", data, 120, 60)
				page.MustEval(`() => window.scrollTo(0, 900)`)
				data, err = TakeScreenshotFormat(page, false, "#low", format, 90, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertFramesBox(t, page, "#low", data, 80, 40)
			})
		}
	}
}
