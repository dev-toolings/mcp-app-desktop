package driver

import (
	"fmt"
	"strings"
)

// Combo is a parsed key combination in DOM terms: Mods are "Meta",
// "Control", "Alt" and "Shift" in the order written; Key is a DOM key value
// ("Enter", "ArrowDown", "F5", " ", or a single US-layout character).
type Combo struct {
	Mods []string
	Key  string
}

// namedKeys maps the key names an agent writes to DOM key values.
var namedKeys = map[string]string{
	"enter": "Enter", "return": "Enter", "tab": "Tab", "escape": "Escape", "esc": "Escape",
	"backspace": "Backspace", "delete": "Delete", "del": "Delete", "space": " ",
	"arrowup": "ArrowUp", "up": "ArrowUp", "arrowdown": "ArrowDown", "down": "ArrowDown",
	"arrowleft": "ArrowLeft", "left": "ArrowLeft", "arrowright": "ArrowRight", "right": "ArrowRight",
	"home": "Home", "end": "End", "pageup": "PageUp", "pagedown": "PageDown", "insert": "Insert",
	"f1": "F1", "f2": "F2", "f3": "F3", "f4": "F4", "f5": "F5", "f6": "F6",
	"f7": "F7", "f8": "F8", "f9": "F9", "f10": "F10", "f11": "F11", "f12": "F12",
}

var modifierKeys = map[string]string{
	"cmd": "Meta", "command": "Meta", "meta": "Meta", "super": "Meta", "win": "Meta",
	"ctrl": "Control", "control": "Control",
	"alt": "Alt", "option": "Alt", "opt": "Alt",
	"shift": "Shift",
}

// ParseCombo turns "cmd+shift+p", "Enter" or "a" into modifiers and a key.
func ParseCombo(combo string) (Combo, error) {
	var c Combo
	parts := strings.Split(strings.TrimSpace(combo), "+")
	if len(parts) == 0 || strings.TrimSpace(combo) == "" {
		return c, fmt.Errorf("key is required")
	}
	// "shift++" means the + key: rebuild a trailing empty part as "+".
	if strings.HasSuffix(combo, "+") && len(parts) >= 2 && parts[len(parts)-1] == "" {
		parts = append(parts[:len(parts)-2], "+")
	}
	for i, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if i < len(parts)-1 {
			m, ok := modifierKeys[name]
			if !ok {
				return Combo{}, fmt.Errorf("unknown modifier %q in %q (cmd, ctrl, alt, shift)", p, combo)
			}
			c.Mods = append(c.Mods, m)
			continue
		}
		if k, ok := namedKeys[name]; ok {
			c.Key = k
			continue
		}
		r := []rune(strings.TrimSpace(p))
		if len(r) != 1 {
			return Combo{}, fmt.Errorf("unknown key %q: use a single character, Enter, Tab, Escape, Backspace, Delete, Arrow*, Home, End, PageUp, PageDown, F1-F12, optionally with cmd/ctrl/alt/shift+", p)
		}
		if r[0] > 0x7f {
			return Combo{}, fmt.Errorf("key %q is not on the US layout: type it with the type tool instead", p)
		}
		c.Key = string(r[0])
	}
	return c, nil
}
