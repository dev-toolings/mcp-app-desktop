package flow

import (
	"strings"
	"testing"
)

func TestParseSelectorsBecomeTargets(t *testing.T) {
	f, err := Parse([]byte(`
name: demo
env:
  AGENT: Ada
---
- tapOn: "Créer un agent"
- tapOn:
    role: textbox
    text: "Nom"
- tapOn: { id: save }
- tapOn: { css: "form button" }
- tapOn: { label: "Email" }
- tapOn: { ref: e4, optional: true, stepLabel: "ref tap" }
- inputText: ${AGENT}
- assertNotVisible: "Erreur"
- extendedWaitUntil: { visible: "Terminé", timeout: 3000 }
- evalMain: "app.isReady()"
`), "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "demo" || f.Env["AGENT"] != "Ada" {
		t.Fatalf("header %+v", f)
	}
	want := []string{
		`getByText("Créer un agent")`,
		`getByRole("textbox", { name: "Nom" })`,
		`[data-testid="save"], #save`,
		`form button`,
		`getByLabel("Email")`,
		`@4`,
	}
	for i, w := range want {
		if f.Steps[i].Target != w {
			t.Errorf("step %d target %q want %q", i+1, f.Steps[i].Target, w)
		}
	}
	if !f.Steps[5].Optional || f.Steps[5].Label != "ref tap" {
		t.Errorf("optional/label not parsed: %+v", f.Steps[5])
	}
	if Expand(f.Steps[6].Text, f.Env) != "Ada" {
		t.Errorf("env expansion: %q", Expand(f.Steps[6].Text, f.Env))
	}
	if f.Steps[8].Visible == "" || f.Steps[8].Timeout.Milliseconds() != 3000 {
		t.Errorf("extendedWaitUntil: %+v", f.Steps[8])
	}
	if f.Steps[9].Command != "evalMain" || f.Steps[9].Text != "app.isReady()" {
		t.Errorf("evalMain: %+v", f.Steps[9])
	}
}

func TestParseRejectsMobileAndUnknown(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"- swipe", "not supported"},
		{"- fly: away", "unknown command"},
		{"- tapOn: { text: a, index: 2 }", "not supported here"},
		{"- tapOn: { text: a, id: b }", "one of"},
		{"- tapOn: { point: '1,2', modifiers: [cmd] }", "modifiers require an element selector"},
		{"- tapOn: { text: a, modifiers: [hyper] }", "unknown modifier"},
		{"- tapOn: { text: a, modifiers: [42] }", "modifiers must be a list of strings"},
		{"- scrollUntilVisible: { element: x, direction: SIDEWAYS }", "direction"},
		{"- extendedWaitUntil: { timeout: 10 }", "exactly one"},
		{"- takeScreenshot: ../x", "relative"},
		{"- repeat: { times: 2 }", "commands"},
	} {
		_, err := Parse([]byte(c.yaml), "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.yaml, err, c.want)
		}
	}
}

func TestBuildTargetEscapesIDs(t *testing.T) {
	got, err := BuildTarget("", "", "", "a:b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != `[data-testid="a:b"], #a\:b` {
		t.Fatalf("got %q", got)
	}
}
