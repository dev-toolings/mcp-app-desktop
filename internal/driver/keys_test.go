package driver

import "testing"

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
