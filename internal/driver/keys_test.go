package driver

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestParseCombo(t *testing.T) {
	c, err := ParseCombo("cmd+shift+p")
	if err != nil || len(c.Mods) != 2 || c.Mods[0] != "Meta" || c.Mods[1] != "Shift" || c.Key != "p" {
		t.Fatalf("cmd+shift+p → %+v %v", c, err)
	}
	if c, err := ParseCombo("Enter"); err != nil || c.Key != "Enter" {
		t.Fatalf("Enter → %+v %v", c, err)
	}
	if c, err := ParseCombo("space"); err != nil || c.Key != " " {
		t.Fatalf("space → %+v %v", c, err)
	}
	if c, err := ParseCombo("shift++"); err != nil || c.Key != "+" || len(c.Mods) != 1 {
		t.Fatalf("shift++ → %+v %v", c, err)
	}
	if _, err := ParseCombo("hyper+x"); err == nil {
		t.Fatal("unknown modifier must fail")
	}
	if _, err := ParseCombo("é"); err == nil {
		t.Fatal("non-US character must fail with a hint")
	}
	if _, err := ParseCombo("  "); err == nil {
		t.Fatal("empty key must fail")
	}
}

func TestParseModifiers(t *testing.T) {
	platform := "Control"
	if runtime.GOOS == "darwin" {
		platform = "Meta"
	}
	for _, tc := range []struct{ names, want []string }{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"cmd", "Ctrl", "shift", "alt"}, []string{"Meta", "Control", "Shift", "Alt"}},
		{[]string{" COMMAND ", "meta", "super", "win", "control", "option", "opt"}, []string{"Meta", "Control", "Alt"}},
		{[]string{"controlOrMeta"}, []string{platform}},
	} {
		got, err := ParseModifiers(tc.names)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%q → %v, %v, want %v", tc.names, got, err, tc.want)
		}
	}
	for _, name := range []string{"hyper", "", "cmd+shift"} {
		if _, err := ParseModifiers([]string{"cmd", name}); err == nil || !strings.Contains(err.Error(), "unknown modifier") ||
			!strings.Contains(err.Error(), "option") || !strings.Contains(err.Error(), "ControlOrMeta") {
			t.Errorf("%q: expected accepted names in error, got %v", name, err)
		}
	}
}
