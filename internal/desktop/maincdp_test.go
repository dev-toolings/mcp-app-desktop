package desktop

import (
	"bufio"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startNodeInspector runs a plain Node process with --inspect on a free
// loopback port and returns its endpoint. Node's inspector speaks the same
// protocol as Electron's main process, minus the electron module. The
// process is killed by pid when the test ends.
func startNodeInspector(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--inspect=127.0.0.1:0", "-e", "setInterval(() => {}, 1 << 30)")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	found := make(chan string, 1)
	go func() {
		re := regexp.MustCompile(`ws://(127\.0\.0\.1:\d+)/`)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				found <- "http://" + m[1]
				break
			}
		}
		for sc.Scan() {
		}
	}()
	select {
	case ep := <-found:
		return ep
	case <-time.After(10 * time.Second):
		t.Fatal("node inspector did not start")
	}
	return ""
}

// connectNode attaches to the inspector and sets globalThis.answer (the
// inspector listens before the -e script runs, so the script cannot).
func connectNode(t *testing.T, ctx context.Context, ep string) *MainClient {
	t.Helper()
	m, err := ConnectMain(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Eval(ctx, `globalThis.answer = 42`, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMainEvalReturnsLastValue(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Node process")
	}
	ep := startNodeInspector(t)
	ctx := context.Background()
	m := connectNode(t, ctx, ep)

	cases := []struct{ name, expr, want string }{
		{"bare expression", `JSON.stringify(globalThis.answer)`, `42`},
		{"number", `globalThis.answer + 1`, `43`},
		{"object", `({a: 1, b: [2]})`, `{"a":1,"b":[2]}`},
		{"object literal without parens", `{a: 1}`, `{"a":1}`},
		{"iife", `(()=>{const x = globalThis.answer; return 'stubbed ' + x})()`, `stubbed 42`},
		{"iife with trailing semicolon", "(() => { return 7 })();\n", `7`},
		{"async iife", `(async()=>{ await new Promise(r => setTimeout(r, 20)); return JSON.stringify({ok: true}) })()`, `{"ok":true}`},
		{"promise", `new Promise(r => setTimeout(() => r('late'), 30))`, `late`},
		{"statements ending with an expression", "const a = 2;\nconst b = 3;\na * b", `6`},
		{"statements with return", "const w = [1, 2, 3];\nreturn w.length", `3`},
		{"early return kept", "const x = 0; if (!x) return 'none'; 'some'", `none`},
		{"top-level await expression", `await Promise.resolve('awaited')`, `awaited`},
		{"top-level await then expression", "const v = await Promise.resolve(5)\nv * 2", `10`},
		{"top-level await with semicolons", "const v = await Promise.resolve(5);\nv + 1;", `6`},
		{"top-level await with return", "const v = await Promise.resolve('r');\nreturn v + '!'", `r!`},
		{"comment after last expression", "const v = await Promise.resolve(1);\nv + 1 // done", `2`},
		{"cut in a line comment is ignored", "await Promise.resolve(1); 'kept' // a;b", `kept`},
		{"continued line is not cut", "const v = await Promise.resolve([3, 1, 2])\nv\n  .sort()", `[1,2,3]`},
		{"loop body is not turned into a return", "let n = 0\nfor (const i of [1, 2, 3])\n  n += await Promise.resolve(i)\nreturn n", `6`},
		{"undefined", `void 0`, `undefined`},
		{"function value", `function named() {}; named`, `"[Function named]"`},
		{"require in scope", `typeof require('path').join`, `function`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Eval(ctx, c.expr, 5*time.Second)
			if err != nil {
				t.Fatalf("Eval(%q): %v", c.expr, err)
			}
			if got != c.want {
				t.Fatalf("Eval(%q) = %q, want %q", c.expr, got, c.want)
			}
		})
	}
}

func TestMainEvalReportsErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Node process")
	}
	ep := startNodeInspector(t)
	ctx := context.Background()
	m := connectNode(t, ctx, ep)

	cases := []struct{ name, expr, want string }{
		{"throw", `throw new Error('boom')`, "boom"},
		{"throw in iife", `(() => { throw new TypeError('bad type') })()`, "bad type"},
		{"rejected promise", `(async () => { throw new Error('async boom') })()`, "async boom"},
		{"syntax error", `const = 1`, "SyntaxError"},
		{"runtime JSON error runs once", `globalThis.runs = (globalThis.runs || 0) + 1; JSON.parse('{')`, "JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Eval(ctx, c.expr, 5*time.Second)
			if err == nil {
				t.Fatalf("Eval(%q) = %q, want an error containing %q", c.expr, got, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Eval(%q) error %q does not mention %q", c.expr, err, c.want)
			}
		})
	}
	// A SyntaxError thrown at run time must not make the source run twice.
	if got, err := m.Eval(ctx, `globalThis.runs`, 5*time.Second); err != nil || got != "1" {
		t.Fatalf("runs = %q (%v), want 1", got, err)
	}
}

func TestMainEvalPromiseTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Node process")
	}
	ep := startNodeInspector(t)
	ctx := context.Background()
	m := connectNode(t, ctx, ep)
	start := time.Now()
	_, err := m.Eval(ctx, `new Promise(() => {})`, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not settle within 300 ms") {
		t.Fatalf("want a promise timeout error, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	// The connection stays usable after a timeout.
	if got, err := m.Eval(ctx, `1 + 1`, time.Second); err != nil || got != "2" {
		t.Fatalf("after timeout: %q, %v", got, err)
	}
	// Synchronous code is terminated by the inspector at the same timeout.
	if _, err := m.Eval(ctx, `while (true) {}`, 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "terminated after 300ms") {
		t.Fatalf("want a termination error, got %v", err)
	}
	if got, err := m.Eval(ctx, `globalThis.answer`, time.Second); err != nil || got != "42" {
		t.Fatalf("after termination: %q, %v", got, err)
	}
}
