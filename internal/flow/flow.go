// Package flow parses and runs Maestro-style YAML flows against an Electron
// window. It implements the desktop-relevant subset of Maestro's command set
// with Maestro's syntax, plus evalJs/assertJs (renderer) and
// evalMain/assertMain (main process); mobile-only commands are rejected at
// parse time instead of being silently skipped.
package flow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Flow is one parsed flow file.
type Flow struct {
	// AppID is the header's appId: informative (the session decides what runs).
	AppID string
	Name  string
	Env   map[string]string
	Steps []Step
}

// Step is one command.
type Step struct {
	// Command is Maestro's command name (tapOn, inputText, …).
	Command  string
	Label    string
	Optional bool
	// Line is the 1-based YAML line of the command, for error reports.
	Line int

	// Target is the element in mcp-app-desktop's target syntax (@ref, CSS,
	// getByRole/getByText/getByLabel), already built from the selector.
	Target string
	// Point is "x,y" (CSS pixels) or "x%,y%" (relative to the viewport).
	Point string
	Text  string
	Key   string
	Count int
	// Timeout bounds waits (zero means the command default).
	Timeout   time.Duration
	Direction string
	// Steps holds nested commands (repeat, runFlow with commands or file).
	Steps []Step
	// Visible / NotVisible are extendedWaitUntil's conditions.
	Visible, NotVisible string
}

// unsupported lists Maestro commands that have no desktop meaning.
var unsupported = map[string]string{
	"back":         "there is no system back button on desktop; use pressKey: Escape or tapOn",
	"hideKeyboard": "desktop apps have no software keyboard", "setLocation": "mobile-only",
	"travel": "mobile-only", "addMedia": "mobile-only", "clearKeychain": "mobile-only",
	"setAirplaneMode": "mobile-only", "toggleAirplaneMode": "mobile-only", "setOrientation": "mobile-only",
	"swipe":      "use scroll or scrollUntilVisible on desktop",
	"openLink":   "not supported: launch the app with the URL as an argument instead",
	"clearState": "state is controlled by launch isolation (a fresh private profile per launch)",
	"stopApp":    "use the stop tool", "killApp": "use the stop tool",
	"longPressOn": "not supported on desktop",
	"runScript":   "Maestro's GraalJS scripts are not supported; use evalJs (renderer) or evalMain (main process)",
	"evalScript":  "Maestro's GraalJS scripts are not supported; use evalJs or evalMain",
	"assertTrue":  "use assertJs or assertMain", "startRecording": "not supported", "stopRecording": "not supported",
	"copyTextFrom": "not supported", "pasteText": "not supported", "inputRandomText": "not supported",
	"inputRandomNumber": "not supported", "inputRandomEmail": "not supported", "inputRandomPersonName": "not supported",
	"assertWithAI": "not supported", "assertNoDefectsWithAI": "not supported", "extractTextWithAI": "not supported",
}

// Commands lists every supported command, for the documentation and errors.
var Commands = []string{
	"launchApp", "tapOn", "doubleTapOn", "inputText", "eraseText", "pressKey",
	"assertVisible", "assertNotVisible", "scroll", "scrollUntilVisible",
	"waitForAnimationToEnd", "extendedWaitUntil", "takeScreenshot", "runFlow",
	"repeat", "evalJs", "assertJs", "evalMain", "assertMain",
}

// ParseFile reads a flow file; runFlow paths resolve relative to it.
func ParseFile(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, filepath.Dir(path))
}

// Parse parses flow YAML. baseDir resolves runFlow file references ("" means
// they are rejected).
func Parse(data []byte, baseDir string) (*Flow, error) {
	return parse(data, baseDir, 0)
}

const maxNesting = 8

// selectorCommands take a selector mapping, where "label" is a selector key.
var selectorCommands = map[string]bool{
	"tapOn": true, "doubleTapOn": true, "assertVisible": true, "assertNotVisible": true,
}

func parse(data []byte, baseDir string, depth int) (*Flow, error) {
	if depth > maxNesting {
		return nil, fmt.Errorf("runFlow nesting deeper than %d", maxNesting)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
		if len(n.Content) > 0 {
			docs = append(docs, n.Content[0])
		}
	}
	f := &Flow{Env: map[string]string{}}
	var list *yaml.Node
	switch len(docs) {
	case 0:
		return nil, fmt.Errorf("empty flow")
	case 1:
		list = docs[0]
	case 2:
		if err := f.header(docs[0]); err != nil {
			return nil, err
		}
		list = docs[1]
	default:
		return nil, fmt.Errorf("a flow has at most a header document and a command list, got %d documents", len(docs))
	}
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: the flow body must be a list of commands", list.Line)
	}
	p := &parser{baseDir: baseDir, depth: depth}
	steps, err := p.steps(list)
	if err != nil {
		return nil, err
	}
	f.Steps = steps
	return f, nil
}

func (f *Flow) header(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: the flow header must be a mapping (appId, name, env, tags)", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "appId":
			f.AppID = v.Value
		case "name":
			f.Name = v.Value
		case "env":
			if err := v.Decode(&f.Env); err != nil {
				return fmt.Errorf("line %d: env must map names to strings", v.Line)
			}
		case "tags":
		case "onFlowStart", "onFlowComplete":
			return fmt.Errorf("line %d: %s hooks are not supported", v.Line, k)
		default:
			return fmt.Errorf("line %d: unknown header key %q", n.Content[i].Line, k)
		}
	}
	return nil
}

type parser struct {
	baseDir string
	depth   int
}

func (p *parser) steps(list *yaml.Node) ([]Step, error) {
	var out []Step
	for _, item := range list.Content {
		s, err := p.step(item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (p *parser) step(n *yaml.Node) (Step, error) {
	var name string
	var arg *yaml.Node
	switch n.Kind {
	case yaml.ScalarNode:
		name = n.Value
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return Step{}, fmt.Errorf("line %d: each list item holds exactly one command", n.Line)
		}
		name, arg = n.Content[0].Value, n.Content[1]
	default:
		return Step{}, fmt.Errorf("line %d: expected a command", n.Line)
	}
	s := Step{Command: name, Line: n.Line}
	if why, ok := unsupported[name]; ok {
		return s, fmt.Errorf("line %d: %s is not supported: %s", n.Line, name, why)
	}
	opts := map[string]*yaml.Node{}
	if arg != nil && arg.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(arg.Content); i += 2 {
			opts[arg.Content[i].Value] = arg.Content[i+1]
		}
		// Maestro's step label shares its key with the label selector:
		// for selector commands "label" is the selector and "stepLabel"
		// names the step.
		labelKey := "label"
		if selectorCommands[name] {
			labelKey = "stepLabel"
		}
		if v, ok := take(opts, labelKey); ok {
			s.Label = v.Value
		}
		if v, ok := take(opts, "optional"); ok {
			s.Optional = v.Value == "true"
		}
	}
	var err error
	switch name {
	case "launchApp", "waitForAnimationToEnd", "scroll":
		if arg != nil && arg.Kind == yaml.MappingNode {
			if v, ok := take(opts, "timeout"); ok {
				s.Timeout, err = millis(v)
			}
			if name == "launchApp" {
				take(opts, "appId")
				take(opts, "clearState")
			}
			if name == "scroll" {
				if v, ok := take(opts, "direction"); ok {
					s.Direction = strings.ToUpper(v.Value)
				}
			}
		}
	case "tapOn", "doubleTapOn", "assertVisible", "assertNotVisible":
		if name == "doubleTapOn" {
			s.Count = 2
		}
		if arg != nil && arg.Kind == yaml.MappingNode {
			if v, ok := take(opts, "point"); ok {
				s.Point = v.Value
			}
			take(opts, "retryTapIfNoChange")
			take(opts, "waitToSettleTimeoutMs")
			if v, ok := take(opts, "timeout"); ok {
				s.Timeout, err = millis(v)
			}
		}
		if s.Point == "" && err == nil {
			s.Target, err = selector(arg, opts)
		}
	case "inputText":
		s.Text, err = scalarOrKey(arg, opts, "text")
	case "pressKey":
		s.Key, err = scalarOrKey(arg, opts, "key")
	case "eraseText":
		s.Count = 50
		if arg != nil {
			var v string
			if v, err = scalarOrKey(arg, opts, "charactersToErase"); err == nil {
				s.Count, err = strconv.Atoi(v)
			}
		}
	case "scrollUntilVisible":
		if arg == nil || arg.Kind != yaml.MappingNode {
			return s, fmt.Errorf("line %d: scrollUntilVisible needs element:", n.Line)
		}
		el, ok := take(opts, "element")
		if !ok {
			return s, fmt.Errorf("line %d: scrollUntilVisible needs element:", n.Line)
		}
		if s.Target, err = selectorNode(el); err != nil {
			break
		}
		s.Direction = "DOWN"
		if v, ok := take(opts, "direction"); ok {
			s.Direction = strings.ToUpper(v.Value)
		}
		if v, ok := take(opts, "timeout"); ok {
			s.Timeout, err = millis(v)
		}
		take(opts, "visibilityPercentage")
		take(opts, "centerElement")
		take(opts, "speed")
		if err == nil && !map[string]bool{"DOWN": true, "UP": true, "LEFT": true, "RIGHT": true}[s.Direction] {
			err = fmt.Errorf("direction must be DOWN, UP, LEFT or RIGHT")
		}
	case "extendedWaitUntil":
		if arg == nil || arg.Kind != yaml.MappingNode {
			return s, fmt.Errorf("line %d: extendedWaitUntil needs visible: or notVisible:", n.Line)
		}
		if v, ok := take(opts, "visible"); ok {
			s.Visible, err = selectorNode(v)
		}
		if v, ok := take(opts, "notVisible"); ok && err == nil {
			s.NotVisible, err = selectorNode(v)
		}
		if v, ok := take(opts, "timeout"); ok && err == nil {
			s.Timeout, err = millis(v)
		}
		if err == nil && (s.Visible == "") == (s.NotVisible == "") {
			err = fmt.Errorf("give exactly one of visible: or notVisible:")
		}
	case "takeScreenshot":
		s.Text, err = scalarOrKey(arg, opts, "path")
		if err == nil && (strings.Contains(s.Text, "..") || filepath.IsAbs(s.Text)) {
			err = fmt.Errorf("screenshot name must be a relative name without '..'")
		}
	case "runFlow":
		s.Steps, err = p.runFlow(arg, opts)
	case "repeat":
		if arg == nil || arg.Kind != yaml.MappingNode {
			return s, fmt.Errorf("line %d: repeat needs times: and commands:", n.Line)
		}
		t, ok := take(opts, "times")
		c, ok2 := take(opts, "commands")
		if !ok || !ok2 {
			return s, fmt.Errorf("line %d: repeat needs times: and commands: (while: is not supported)", n.Line)
		}
		if s.Count, err = strconv.Atoi(t.Value); err == nil && (s.Count < 0 || s.Count > 1000) {
			err = fmt.Errorf("times must be between 0 and 1000")
		}
		if err == nil {
			s.Steps, err = p.steps(c)
		}
	case "evalJs", "assertJs", "evalMain", "assertMain":
		s.Text, err = scalarOrKey(arg, opts, "script")
	default:
		return s, fmt.Errorf("line %d: unknown command %q (supported: %s)", n.Line, name, strings.Join(Commands, ", "))
	}
	if err != nil {
		return s, fmt.Errorf("line %d: %s: %w", n.Line, name, err)
	}
	for k := range opts {
		return s, fmt.Errorf("line %d: %s: unknown option %q", n.Line, name, k)
	}
	return s, nil
}

func (p *parser) runFlow(arg *yaml.Node, opts map[string]*yaml.Node) ([]Step, error) {
	file := ""
	if arg != nil && arg.Kind == yaml.ScalarNode {
		file = arg.Value
	} else if arg != nil && arg.Kind == yaml.MappingNode {
		if c, ok := take(opts, "commands"); ok {
			return p.steps(c)
		}
		if v, ok := take(opts, "file"); ok {
			file = v.Value
		}
		if _, ok := take(opts, "when"); ok {
			return nil, fmt.Errorf("conditional runFlow (when:) is not supported")
		}
	}
	if file == "" {
		return nil, fmt.Errorf("runFlow needs a file or commands:")
	}
	if p.baseDir == "" {
		return nil, fmt.Errorf("runFlow file references need a flow loaded from a file")
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.baseDir, file)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sub, err := parse(data, filepath.Dir(path), p.depth+1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return sub.Steps, nil
}

func take(m map[string]*yaml.Node, k string) (*yaml.Node, bool) {
	v, ok := m[k]
	delete(m, k)
	return v, ok
}

func scalarOrKey(arg *yaml.Node, opts map[string]*yaml.Node, key string) (string, error) {
	if arg == nil {
		return "", fmt.Errorf("missing argument")
	}
	if arg.Kind == yaml.ScalarNode {
		return arg.Value, nil
	}
	if v, ok := take(opts, key); ok && v.Kind == yaml.ScalarNode {
		return v.Value, nil
	}
	return "", fmt.Errorf("expected a string or %s:", key)
}

func millis(v *yaml.Node) (time.Duration, error) {
	ms, err := strconv.Atoi(v.Value)
	if err != nil || ms < 0 {
		return 0, fmt.Errorf("timeout must be milliseconds, got %q", v.Value)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// selector reads a command argument that is either a text shorthand or a
// selector mapping (whose keys are consumed from opts) and returns a target.
//
// Mapping keys: text (accessible name or visible text, case-insensitive
// substring, unique), role (with text as the name), label, id (DOM id or
// data-testid), css (raw selector), ref (@N from a snapshot).
func selector(arg *yaml.Node, opts map[string]*yaml.Node) (string, error) {
	if arg == nil {
		return "", fmt.Errorf("missing selector")
	}
	if arg.Kind == yaml.ScalarNode {
		return TextTarget(arg.Value), nil
	}
	var text, role, label, id, css, ref string
	str := func(k string, dst *string) {
		if v, ok := take(opts, k); ok {
			*dst = v.Value
		}
	}
	str("text", &text)
	str("role", &role)
	str("label", &label)
	str("id", &id)
	str("css", &css)
	str("ref", &ref)
	for _, k := range []string{"index", "enabled", "checked", "focused", "selected", "below", "above", "leftOf", "rightOf", "childOf", "containsChild"} {
		if _, ok := take(opts, k); ok {
			return "", fmt.Errorf("selector key %q is not supported here: use text, role, label, id, css or ref", k)
		}
	}
	return BuildTarget(text, role, label, id, css, ref)
}

// TextTarget builds a target matching visible text or accessible name.
func TextTarget(text string) string { return fmt.Sprintf("getByText(%q)", text) }

// BuildTarget combines selector fields into one target string.
func BuildTarget(text, role, label, id, css, ref string) (string, error) {
	n := 0
	for _, v := range []string{text, label, id, css, ref} {
		if v != "" {
			n++
		}
	}
	if role != "" && text == "" {
		n++
	}
	if n == 0 {
		return "", fmt.Errorf("empty selector: give text, role, label, id, css or ref")
	}
	if n > 1 {
		return "", fmt.Errorf("a selector takes one of text (optionally with role), label, id, css or ref")
	}
	switch {
	case ref != "":
		r := strings.TrimPrefix(strings.TrimPrefix(ref, "@"), "e")
		return "@" + r, nil
	case css != "":
		return css, nil
	case id != "":
		return fmt.Sprintf("[data-testid=%q], #%s", id, cssIdent(id)), nil
	case label != "":
		return fmt.Sprintf("getByLabel(%q)", label), nil
	case role != "" && text != "":
		return fmt.Sprintf("getByRole(%q, { name: %q })", role, text), nil
	case role != "":
		return fmt.Sprintf("getByRole(%q)", role), nil
	}
	return TextTarget(text), nil
}

var cssIdentSafe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func cssIdent(id string) string {
	if cssIdentSafe.MatchString(id) {
		return id
	}
	var b strings.Builder
	for _, r := range id {
		if r < 0x80 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			b.WriteString(`\` + string(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func selectorNode(n *yaml.Node) (string, error) {
	opts := map[string]*yaml.Node{}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			opts[n.Content[i].Value] = n.Content[i+1]
		}
	}
	t, err := selector(n, opts)
	if err != nil {
		return "", err
	}
	for k := range opts {
		return "", fmt.Errorf("unknown selector key %q", k)
	}
	return t, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Expand substitutes ${NAME} from env; unknown names are left as written so
// the failure is visible in the step output rather than silently empty.
func Expand(s string, env map[string]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := env[m[2:len(m)-1]]; ok {
			return v
		}
		return m
	})
}
