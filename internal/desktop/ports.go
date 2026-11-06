package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dev-toolings/mcp-app-desktop/internal/tauri"
)

// Placeholders a development command may use in its argv or environment to
// receive the ports this server chose. They are also exported as environment
// variables of the same name, so a dev script can read them without any
// placeholder: pass --remote-debugging-port=$MCP_APP_DESKTOP_CDP_PORT and
// --inspect=127.0.0.1:$MCP_APP_DESKTOP_INSPECT_PORT to the electron binary.
// A Tauri app with the bridge plugin reads MCP_APP_DESKTOP_BRIDGE_PORT and
// MCP_APP_DESKTOP_BRIDGE_TOKEN itself.
const (
	EnvCDPPort     = "MCP_APP_DESKTOP_CDP_PORT"
	EnvInspectPort = "MCP_APP_DESKTOP_INSPECT_PORT"
	// The names before the project became mcp-app-desktop, still exported
	// and expanded so existing dev scripts keep working.
	legacyEnvCDPPort     = "MCP_ELECTRON_CDP_PORT"
	legacyEnvInspectPort = "MCP_ELECTRON_INSPECT_PORT"
	// envWebView2Args gives WebView2 (Tauri on Windows) extra Chromium
	// switches: the DevTools port.
	envWebView2Args = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
)

// portVars are the variables this server sets; inherited values are dropped.
var portVars = []string{EnvCDPPort, EnvInspectPort, legacyEnvCDPPort, legacyEnvInspectPort, tauri.EnvBridgePort, tauri.EnvBridgeToken}

// ports are the endpoints offered to one launched application.
type ports struct {
	CDP, Inspect, Bridge int
	Token                string
}

// allocPorts picks free loopback ports: CDP and the bridge always (a dev
// command may start either framework), the Node inspector when asked.
func allocPorts(inspect bool) (ports, error) {
	var pp ports
	var err error
	if pp.CDP, err = freePort(); err != nil {
		return pp, err
	}
	if pp.Bridge, err = freePort(); err != nil {
		return pp, err
	}
	if inspect {
		if pp.Inspect, err = freePort(); err != nil {
			return pp, err
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return pp, err
	}
	pp.Token = hex.EncodeToString(buf)
	return pp, nil
}

// env returns the variables handing the ports to the application.
func (pp ports) env() []string {
	var out []string
	if pp.CDP > 0 {
		out = append(out, EnvCDPPort+"="+strconv.Itoa(pp.CDP), legacyEnvCDPPort+"="+strconv.Itoa(pp.CDP))
		if runtime.GOOS == "windows" {
			args := "--remote-debugging-port=" + strconv.Itoa(pp.CDP)
			if prev := os.Getenv(envWebView2Args); prev != "" {
				args = prev + " " + args
			}
			out = append(out, envWebView2Args+"="+args)
		}
	}
	if pp.Inspect > 0 {
		out = append(out, EnvInspectPort+"="+strconv.Itoa(pp.Inspect), legacyEnvInspectPort+"="+strconv.Itoa(pp.Inspect))
	}
	if pp.Bridge > 0 {
		out = append(out, tauri.EnvBridgePort+"="+strconv.Itoa(pp.Bridge))
		if pp.Token != "" {
			out = append(out, tauri.EnvBridgeToken+"="+pp.Token)
		}
	}
	return out
}

// freePort asks the kernel for an unused loopback TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// expandPorts substitutes ${NAME} and $NAME for the port variables (current
// and legacy names) in s.
func expandPorts(s string, pp ports) string {
	vals := map[string]int{EnvCDPPort: pp.CDP, legacyEnvCDPPort: pp.CDP, EnvInspectPort: pp.Inspect, legacyEnvInspectPort: pp.Inspect, tauri.EnvBridgePort: pp.Bridge}
	var pairs []string
	// Longer names first: $MCP_APP_DESKTOP_CDP_PORT must not be eaten by a
	// shorter prefix.
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, k := range names {
		v := strconv.Itoa(vals[k])
		pairs = append(pairs, "${"+k+"}", v, "$"+k, v)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// probeTimeout bounds one liveness probe of a debugging endpoint.
const probeTimeout = 1500 * time.Millisecond

// versionInfo is the answer of /json/version.
type versionInfo struct {
	Browser   string `json:"Browser"`
	UserAgent string `json:"User-Agent"`
	WebSocket string `json:"webSocketDebuggerUrl"`
}

// frameworkOf tells an attached CDP endpoint's framework: Electron puts
// itself in the user agent; a WebView2 endpoint (Edg/…) without it is taken
// for a Tauri app on Windows.
func frameworkOf(v versionInfo) string {
	if !strings.Contains(v.UserAgent, "Electron/") && strings.HasPrefix(v.Browser, "Edg/") {
		return FrameworkTauri
	}
	return FrameworkElectron
}

// probeCDP reports whether a Chromium remote debugging endpoint answers at
// endpoint (http://127.0.0.1:PORT). A Node inspector also answers
// /json/version, with Browser "node.js/…": it is not a CDP browser endpoint.
func probeCDP(ctx context.Context, endpoint string) (versionInfo, bool) {
	var v versionInfo
	if err := readJSON(ctx, strings.TrimRight(endpoint, "/")+"/json/version", &v); err != nil {
		return v, false
	}
	if v.Browser == "" || strings.HasPrefix(v.Browser, "node.js/") {
		return v, false
	}
	return v, true
}

// inspectorTarget is one entry of a Node inspector's /json/list.
type inspectorTarget struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	WebSocket string `json:"webSocketDebuggerUrl"`
}

// probeInspector returns the WebSocket URL of a Node inspector listening at
// endpoint, or "" when none answers.
func probeInspector(ctx context.Context, endpoint string) string {
	var list []inspectorTarget
	if err := readJSON(ctx, strings.TrimRight(endpoint, "/")+"/json/list", &list); err != nil {
		return ""
	}
	for _, t := range list {
		if t.Type == "node" && t.WebSocket != "" {
			return t.WebSocket
		}
	}
	return ""
}

func readJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Chromium requires a Host header that is an IP or localhost: a loopback
	// URL already satisfies that.
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20))
	return dec.Decode(out)
}

// loopbackEndpoint builds http://127.0.0.1:PORT.
func loopbackEndpoint(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }
