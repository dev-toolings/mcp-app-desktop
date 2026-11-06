//go:build windows

package desktop

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
)

func setProcessGroup(cmd *exec.Cmd) {}

// signalGroup has no process-group semantics on Windows: taskkill the tree.
func signalGroup(pid int, kill bool) {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if kill {
		args = append(args, "/F")
	}
	_ = exec.Command("taskkill", args...).Run()
}

func signalPID(pid int, kill bool) {
	if !kill {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// processGroupOf is unsupported: a dev command is never adopted on Windows.
func processGroupOf(pid int) (int, error) { return 0, errors.New("no process groups on Windows") }
