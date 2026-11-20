package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// bridgeFileMax bounds a bridge file: an endpoint and a token.
const bridgeFileMax = 64 * 1024

// ReadBridgeFile reads where a running Tauri app's bridge listens and its
// token from the file the app's dev script wrote for that purpose:
//
//	{"endpoint": "127.0.0.1:52988", "token": "…"}
//
// The token then goes from the file to the bridge without passing through
// the caller. The file holds a secret: a symbolic link, or a file other
// users can read, is refused rather than trusted.
func ReadBridgeFile(path string) (endpoint, token string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s: %v (the app's dev script writes it when it opens the bridge, and removes it on exit: is the app running?)", path, err)}
	}
	if !info.Mode().IsRegular() {
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s is not a regular file", path)}
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s is readable by other users (mode %04o): it holds the bridge token, write it with mode 0600", path, info.Mode().Perm())}
	}
	if info.Size() > bridgeFileMax {
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s is %d bytes: not a bridge file ({\"endpoint\", \"token\"})", path, info.Size())}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("bridge_file %s: %w", path, err)
	}
	var file struct {
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		// Never the content: it may be the token itself.
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s is not JSON ({\"endpoint\": \"127.0.0.1:PORT\", \"token\": \"…\"})", path)}
	}
	if strings.TrimSpace(file.Endpoint) == "" {
		return "", "", &UsageError{fmt.Sprintf("bridge_file %s has no endpoint ({\"endpoint\": \"127.0.0.1:PORT\", \"token\": \"…\"})", path)}
	}
	return file.Endpoint, file.Token, nil
}
