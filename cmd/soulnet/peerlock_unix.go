//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// openLocked opens the lock file and takes a kernel advisory lock on it
// (flock LOCK_EX, non-blocking). The lock dies with the process, however the
// process dies; the file content (holder pid) stays readable to everyone.
func openLocked(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return f, nil
}
