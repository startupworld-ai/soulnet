//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// processAlive reports whether a process with this pid exists (signal 0 probe;
// EPERM means it exists but is not ours — still alive).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// killProcess force-kills the process (SIGKILL — the graceful window has passed).
func killProcess(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// detachProcess makes the child survive this helper and its terminal: a new
// session (setsid) detaches it from the process group and controlling tty.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
