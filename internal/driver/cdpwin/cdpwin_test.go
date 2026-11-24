package cdpwin

import "testing"

func TestMouseModifiers(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  int
	}{
		{nil, 0},
		{[]string{"Alt"}, 1},
		{[]string{"Control"}, 2},
		{[]string{"Meta"}, 4},
		{[]string{"Shift"}, 8},
		{[]string{"Alt", "Control", "Meta", "Shift", "Shift"}, 15},
	} {
		if got := mouseModifiers(tc.names); got != tc.want {
			t.Errorf("%v: got %d, want %d", tc.names, got, tc.want)
		}
	}
}
