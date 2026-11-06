package tauri

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/dev-toolings/mcp-app-desktop/internal/driver"
)

func TestEvalBodies(t *testing.T) {
	for _, c := range []struct {
		expr string
		want string // a body that must be among the candidates
	}{
		{"document.title", "return (\ndocument.title\n);"},
		{"const a = 2; a * 21", "const a = 2;\nreturn (\na * 21\n);"},
		{"const a = 2\na * 21", "const a = 2\nreturn (\na * 21\n);"},
		{"const s = 'x;y'; s.length;", "const s = 'x;y';\nreturn (\ns.length\n);"},
		{"if (x) { y(); }", "if (x) { y(); }"},
	} {
		bodies := evalBodies(c.expr)
		found := false
		for _, b := range bodies {
			if b == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: %q not among %q", c.expr, c.want, bodies)
		}
		if bodies[len(bodies)-1] != c.expr {
			t.Errorf("%q: the plain body must be the last resort", c.expr)
		}
	}
}

func TestSplitLastIgnoresNestedAndQuoted(t *testing.T) {
	for _, c := range []struct {
		code string
		ok   bool
		last string
	}{
		{"f(a; b)", false, ""},
		{"x = `a;${b;c}`", false, ""},
		{"let a = 1; return a", false, ""}, // a return is not an expression
		{"a(); // c;\nb", true, "b"},
		{"a();\n  .then(x)", false, ""}, // a continued chain is not split
		{"/* ; */ x", false, ""},
	} {
		_, last, ok := splitLast(c.code, ';')
		if c.code == "a(); // c;\nb" {
			_, last, ok = splitLast(c.code, '\n')
		}
		if c.code == "a();\n  .then(x)" {
			_, last, ok = splitLast(c.code, '\n')
		}
		if ok != c.ok || (ok && last != c.last) {
			t.Errorf("%q: got %q %v, want %q %v", c.code, last, ok, c.last, c.ok)
		}
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]loc{
		"@3":                  {Kind: "ref", Ref: "@3"},
		"e12":                 {Kind: "ref", Ref: "@12"},
		"#save":               {Kind: "css", CSS: "#save"},
		`getByText("Save")`:   {Kind: "text", Text: "Save", Desc: `getByText("Save")`},
		`getByLabel("Email")`: {Kind: "label", Label: "Email", Desc: `getByLabel("Email")`},
		`getByRole("b")`:      {Kind: "role", Role: "button", Desc: `getByRole("b")`},
		`getByRole("Button", { name: "Go", exact: true })`: {Kind: "role", Role: "button", Name: "Go", Exact: true, Desc: `getByRole("Button", { name: "Go", exact: true })`},
	} {
		got, err := parseTarget(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.Kind != want.Kind || got.Ref != want.Ref || got.CSS != want.CSS || got.Text != want.Text || got.Label != want.Label || got.Role != want.Role || got.Name != want.Name || got.Exact != want.Exact {
			t.Errorf("%s: got %+v want %+v", in, *got, want)
		}
	}
	got, err := parseTarget(`getByRole("button", { name: /^Mode : /i })`)
	if err != nil || got.Pattern == nil || got.Pattern.Source != "^Mode : " || got.Pattern.Flags != "i" {
		t.Fatalf("regex name: %+v %v", got, err)
	}
	if _, err := parseTarget(".a >> .b"); err == nil {
		t.Fatal("chaining must be refused")
	}
	if _, err := parseTarget("  "); err == nil {
		t.Fatal("empty target must be refused")
	}
	if _, err := parseTarget(`getByTitle("x")`); err == nil {
		t.Fatal("unsupported locator must be refused")
	}
}

func TestEncodeShotCropsAndConverts(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		for y := 0; y < 20; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 12), 0, 255})
		}
	}
	crop := image.Rect(10, 5, 30, 15)
	shot, err := encodeShot(img, &crop, driver.ShotOptions{Format: "png"})
	if err != nil || shot.Format != "png" {
		t.Fatal(err)
	}
	dec, err := png.Decode(bytes.NewReader(shot.Data))
	if err != nil || dec.Bounds().Dx() != 20 || dec.Bounds().Dy() != 10 {
		t.Fatalf("crop: %v %v", dec.Bounds(), err)
	}
	if r, g, _, _ := dec.At(0, 0).RGBA(); r>>8 != 60 || g>>8 != 60 {
		t.Fatalf("crop origin pixel: %d %d", r>>8, g>>8)
	}
	shot, err = encodeShot(img, nil, driver.ShotOptions{Format: "webp", Quality: 50})
	if err != nil || shot.Format != "jpeg" || !strings.Contains(shot.Note, "webp") {
		t.Fatalf("webp fallback: %+v %v", shot.Note, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(shot.Data)); err != nil {
		t.Fatal(err)
	}
	if _, err := encodeShot(img, nil, driver.ShotOptions{Format: "gif"}); err == nil {
		t.Fatal("unknown format must fail")
	}
}

func TestRetryableCodes(t *testing.T) {
	for code, want := range map[string]bool{"notfound": true, "hidden": true, "covered": true, "disabled": true, "moving": true, "ambiguous": false, "stale": false, "invalid": false, "exception": false} {
		if got := retryable(&agentError{Code: code, Msg: code}); got != want {
			t.Errorf("%s: retryable=%v", code, got)
		}
	}
	if !retryable(errNoResult) {
		t.Error("a page between two documents is retried")
	}
}

func TestAgentScriptIsVersioned(t *testing.T) {
	if strings.Contains(agentScript, "__AGENT_VERSION__") || !strings.Contains(agentScript, agentVersion) || !strings.HasSuffix(agentScript, `;"installed"`) {
		t.Fatal("agent script must carry its version and evaluate to \"installed\"")
	}
	for _, banned := range []string{"eval(", "new Function("} {
		if strings.Contains(agentSource, banned) {
			t.Errorf("the agent must not use %s (a strict CSP forbids it)", banned)
		}
	}
}
