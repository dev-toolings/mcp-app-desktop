// Package tauri drives Tauri applications on macOS and Linux, where the
// webview (WKWebView, WebKitGTK) has no DevTools Protocol: the app embeds
// the tauri-plugin-mcp-app-desktop plugin (debug builds), which listens on a
// loopback port and evaluates scripts in its webviews. This package speaks
// that protocol, installs a page agent in every webview it drives, and
// implements driver.App and driver.Window on top of it.
package tauri

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Environment variables the plugin reads in the application's process.
const (
	EnvBridgePort  = "MCP_APP_DESKTOP_BRIDGE_PORT"
	EnvBridgeToken = "MCP_APP_DESKTOP_BRIDGE_TOKEN"
)

// ProtocolVersion is the bridge protocol this server speaks.
const ProtocolVersion = 1

// Hello is the plugin's identity, returned by the hello command.
type Hello struct {
	Protocol      int    `json:"protocol"`
	PluginVersion string `json:"plugin_version"`
	TauriVersion  string `json:"tauri_version"`
	AppName       string `json:"app_name"`
	AppVersion    string `json:"app_version"`
	Identifier    string `json:"identifier"`
	PID           int    `json:"pid"`
	OS            string `json:"os"`
}

// maxLine bounds one response line (a full-window PNG in base64 fits).
const maxLine = 64 << 20

type response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Client is one connection to a bridge. Calls may run concurrently.
type Client struct {
	addr  string
	conn  net.Conn
	Hello Hello

	wmu     sync.Mutex // serialises writes
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan response
	closed  chan struct{}
	err     error
	once    sync.Once
}

// ErrUnreachable is returned by Dial when nothing listens at the address.
var ErrUnreachable = errors.New("no Tauri bridge answers")

// Dial connects to the bridge at addr (127.0.0.1:PORT) and says hello with
// token (empty when the app was started without one).
func Dial(ctx context.Context, addr, token string) (*Client, error) {
	if err := requireLoopback(addr); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w at %s: %v", ErrUnreachable, addr, err)
	}
	c := &Client{addr: addr, conn: conn, pending: map[uint64]chan response{}, closed: make(chan struct{})}
	go c.read()
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := c.Call(hctx, "hello", map[string]any{"token": token, "protocol": ProtocolVersion})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("bridge at %s: hello: %w", addr, err)
	}
	if err := json.Unmarshal(raw, &c.Hello); err != nil {
		c.Close()
		return nil, fmt.Errorf("bridge at %s: hello: %w", addr, err)
	}
	if c.Hello.Protocol != ProtocolVersion {
		c.Close()
		return nil, fmt.Errorf("bridge at %s speaks protocol %d, this server %d: update tauri-plugin-mcp-app-desktop or the server", addr, c.Hello.Protocol, ProtocolVersion)
	}
	return c, nil
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bridge address %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost") {
		return nil
	}
	return fmt.Errorf("bridge address %q is not a loopback address; mcp-app-desktop only drives local applications", addr)
}

// Addr is the bridge address.
func (c *Client) Addr() string { return c.addr }

func (c *Client) read() {
	r := bufio.NewReaderSize(c.conn, 64<<10)
	var err error
	for {
		var line []byte
		line, err = readLine(r)
		if err != nil {
			break
		}
		var res response
		if json.Unmarshal(line, &res) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[res.ID]
		delete(c.pending, res.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
	c.shutdown(fmt.Errorf("bridge connection to %s closed: %v", c.addr, err))
}

// readLine reads one newline-terminated line of at most maxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("response line over %d MiB", maxLine>>20)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func (c *Client) shutdown(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		close(c.closed)
		c.mu.Unlock()
		_ = c.conn.Close()
	})
}

// Close ends the connection; pending calls fail.
func (c *Client) Close() { c.shutdown(errors.New("bridge connection closed")) }

// Closed reports whether the connection ended.
func (c *Client) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Call sends one command and waits for its answer (or ctx).
func (c *Client) Call(ctx context.Context, cmd string, params map[string]any) (json.RawMessage, error) {
	req := map[string]any{}
	for k, v := range params {
		req[k] = v
	}
	c.mu.Lock()
	if c.Closed() {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	req["id"], req["cmd"] = id, cmd
	line, err := json.Marshal(req)
	if err != nil {
		c.forget(id)
		return nil, err
	}
	line = append(line, '\n')
	c.wmu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(dl)
	} else {
		_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	_, err = c.conn.Write(line)
	c.wmu.Unlock()
	if err != nil {
		c.forget(id)
		c.shutdown(fmt.Errorf("bridge connection to %s: write: %v", c.addr, err))
		return nil, err
	}
	select {
	case res := <-ch:
		if !res.OK {
			return nil, errors.New(res.Error)
		}
		return res.Result, nil
	case <-c.closed:
		c.forget(id)
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return nil, err
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Alive reports whether the bridge answers a ping within d.
func (c *Client) Alive(d time.Duration) bool {
	if c.Closed() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := c.Call(ctx, "ping", nil)
	return err == nil
}

// Probe reports whether a bridge listens at addr, without saying hello (a
// plain TCP connect, so a token is not needed to know the app is up).
func Probe(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
