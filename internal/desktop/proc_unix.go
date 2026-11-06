//go:build !windows

package desktop

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends SIGTERM (or SIGKILL) to a whole process group.
func signalGroup(pgid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pgid, sig)
}

// signalPID sends SIGTERM (or SIGKILL) to one process.
func signalPID(pid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(pid, sig)
}

// processGroupOf returns the process group of pid.
func processGroupOf(pid int) (int, error) { return syscall.Getpgid(pid) }
