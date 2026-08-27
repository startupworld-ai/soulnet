//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// createNoWindow spawns console-less children (same flag the SoulMirror
// winproc helper uses); combined with a new process group the child is fully
// independent of this helper's lifetime.
const createNoWindow = 0x08000000

// processAlive reports whether a process with this pid is still running: open
// a handle (fails once the pid is released) and poll its exit state — a pid
// whose handle is still open somewhere else stays openable after exit, so
// WaitForSingleObject with a zero timeout is the authoritative probe.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return true // handle is open but unqueryable: assume alive, the kill fence follows
	}
	return event == uint32(syscall.WAIT_TIMEOUT)
}

// killProcess force-kills the process (TerminateProcess).
func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil // already gone
	}
	defer func() { _ = p.Release() }()
	return p.Kill()
}

// detachProcess makes the child survive this helper: no console window, its
// own process group.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
}
